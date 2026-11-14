// Package source holds the stateful, bounded readers the pipeline's
// coordinator owns: the registry (last good value, one torn-read retry), the
// job records (read only where the cycle permits), the agents listing (its
// gates as timestamps), and the one bounded tail reader.
//
// It is separate from claude because claude is pure: these OPEN files. Every
// open is a read; nothing here creates, writes, renames or removes anything
// (guard G2). Each value is owned by the coordinator's goroutine alone.
package source

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

// DefaultRetryDelay is how long a torn registry read waits before its one
// retry (C8). The registry is rewritten IN PLACE, so a reader can catch a file
// half-written; the cheapest correct response is to let the writer finish.
const DefaultRetryDelay = 20 * time.Millisecond

// maxRetriesPerRead bounds the retries one Read spends, so a directory being
// rewritten wholesale costs latency, never a stalled cycle.
const maxRetriesPerRead = 4

// Result is one source's read: what it found, what went wrong, and the state
// the document reports for it (§3.1's Sources table).
type Result struct {
	State    model.SourceState
	Detail   string
	Problems []model.Problem
}

// Registry reads sessions/*.json.
type Registry struct {
	tree  ports.Tree
	clock ports.Clock
	retry time.Duration
	known map[string]claude.Record // file -> last good value
}

// NewRegistry builds the reader. A zero retry takes DefaultRetryDelay.
func NewRegistry(tree ports.Tree, clock ports.Clock, retry time.Duration) *Registry {
	if retry <= 0 {
		retry = DefaultRetryDelay
	}
	return &Registry{tree: tree, clock: clock, retry: retry, known: map[string]claude.Record{}}
}

// RegistryResult is one read of the registry.
type RegistryResult struct {
	Result
	Records []claude.Record
}

// Read lists the directory and decodes every record once. A missing directory
// is a FACT (a machine where Claude has never run), reported OK and "absent".
// A torn record is retried once after the delay, then its last good value is
// kept with a ProblemTorn; with no last good value it is absent from this
// read, and the Problem says so. A record larger than claude.MaxRecordBytes is
// never read whole.
func (r *Registry) Read() RegistryResult {
	var out RegistryResult
	if r.tree.IsZero() {
		out.State = model.SourceOff
		return out
	}
	entries, err := fs.ReadDir(r.tree.FS, ".")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.known = map[string]claude.Record{}
		out.State, out.Detail = model.SourceOK, "absent"
		return out
	case err != nil:
		out.State, out.Detail = model.SourceFailed, err.Error()
		out.Problems = append(out.Problems, model.Problem{Scope: model.ScopeSource, Kind: model.ProblemUnreadable,
			Detail: "listing the registry: " + err.Error()})
		return out
	}
	out.State = model.SourceOK
	seen := map[string]bool{}
	retries := 0
	degrade := func(p model.Problem) {
		out.Problems = append(out.Problems, p)
		out.State = model.SourceDegraded
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && claude.IsRecordName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		seen[name] = true
		b, err := readBounded(r.tree.FS, name)
		if errors.Is(err, fs.ErrNotExist) {
			delete(r.known, name) // removed between the listing and the read
			continue
		}
		if err != nil {
			degrade(model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemUnreadable, Path: name, Detail: err.Error()})
			r.keep(&out, name)
			continue
		}
		rec, ps, err := claude.DecodeRecord(name, b)
		if errors.Is(err, claude.ErrTorn) && retries < maxRetriesPerRead {
			retries++
			r.wait()
			if b, rerr := readBounded(r.tree.FS, name); rerr == nil {
				rec, ps, err = claude.DecodeRecord(name, b)
			}
		}
		if err != nil {
			detail := "not a JSON object twice running"
			if _, had := r.known[name]; had {
				detail += "; the last good value is kept"
			} else {
				detail += "; there is no earlier value, so this record is absent from the document. Absent is not dead"
			}
			degrade(model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemTorn, Path: name, Detail: detail})
			r.keep(&out, name)
			continue
		}
		out.Problems = append(out.Problems, ps...)
		r.known[name] = rec
		out.Records = append(out.Records, rec)
	}
	for name := range r.known {
		if !seen[name] {
			delete(r.known, name)
		}
	}
	return out
}

func (r *Registry) keep(out *RegistryResult, name string) {
	if rec, ok := r.known[name]; ok {
		out.Records = append(out.Records, rec)
	}
}

// wait is the one torn-read delay: a one-shot timer from the injected Clock.
func (r *Registry) wait() {
	t := r.clock.NewTimer(r.retry)
	defer t.Stop()
	<-t.C()
}

// openRegular opens name only if it is a regular file, and checks the type
// TWICE: by name BEFORE the open, and on the handle after it.
//
// The name check is what keeps a FIFO from hanging the caller. os.DirFS's
// Open is a blocking open(2), and on a FIFO with no writer it waits for one,
// for ever — so a check made only on the opened handle is never reached, and
// the coordinator (and with it Stop) would hang on a `*.json` named pipe in
// the registry (review r2 #1). fs.Stat follows a symlink, as the open would,
// so a link to a FIFO is refused the same way.
//
// The handle check is what keeps a file swapped in AFTER the name check from
// passing: the bytes read are the handle's, so the handle is what is judged.
// The window between the two cannot be closed through fs.FS, which has no
// O_NONBLOCK: a FIFO swapped in between them can still block the open. That
// takes a same-user writer racing a stat, not a file left lying in the tree.
func openRegular(fsys fs.FS, name string) (fs.File, fs.FileInfo, error) {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file (%s)", name, info.Mode().Type())
	}
	f, err := fsys.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file (%s)", name, info.Mode().Type())
	}
	return f, info, nil
}

// readBounded reads one record whole, refusing one larger than
// claude.MaxRecordBytes, and one that is not a regular file (openRegular).
//
// The size is checked on the HANDLE that is read, never only on the name: a
// file swapped between a Stat of its name and the read would otherwise pass
// the size check and then be read whole. The read itself is limited too, so a
// size the handle misreports cannot carry it past the bound either.
func readBounded(fsys fs.FS, name string) ([]byte, error) {
	f, info, err := openRegular(fsys, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tooBig := func(size int64) error {
		return fmt.Errorf("%s is %d bytes, larger than the %d-byte record bound", name, size, claude.MaxRecordBytes)
	}
	if info.Size() > claude.MaxRecordBytes {
		return nil, tooBig(info.Size())
	}
	b, err := io.ReadAll(io.LimitReader(f, claude.MaxRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > claude.MaxRecordBytes {
		return nil, fmt.Errorf("%s grew past the %d-byte record bound while it was read", name, claude.MaxRecordBytes)
	}
	return b, nil
}

// Jobs reads jobs/<dir>/state.json, at depth 1 only.
type Jobs struct {
	tree  ports.Tree
	known map[string]claude.Job // dir -> last good value
}

// NewJobs builds the reader.
func NewJobs(tree ports.Tree) *Jobs { return &Jobs{tree: tree, known: map[string]claude.Job{}} }

// Listing is the jobs root's directories, sorted, and the state of the
// listing itself.
type Listing struct {
	Result
	Dirs   []string
	Absent bool
}

// List lists the jobs root. It reads metadata only; the spine covers it while
// watching. Root files (pins.json) are not jobs.
func (j *Jobs) List() Listing {
	var l Listing
	if j.tree.IsZero() {
		l.State = model.SourceOff
		return l
	}
	entries, err := fs.ReadDir(j.tree.FS, ".")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		l.State, l.Detail, l.Absent = model.SourceOK, "absent", true
		return l
	case err != nil:
		l.State, l.Detail = model.SourceFailed, err.Error()
		l.Problems = append(l.Problems, model.Problem{Scope: model.ScopeSource, Kind: model.ProblemUnreadable,
			Detail: "listing the jobs root: " + err.Error()})
		return l
	}
	l.State = model.SourceOK
	for _, e := range entries {
		if e.IsDir() {
			l.Dirs = append(l.Dirs, e.Name())
		}
	}
	sort.Strings(l.Dirs)
	return l
}

// JobsResult is one read of the job records.
type JobsResult struct {
	Result
	Jobs []claude.Job
}

// Read reads the record of every listed directory the cycle PERMITS, and
// refuses the rest with ProblemNotGranted (C17): the permit is enforced here
// at run time, not trusted. A directory with no state.json is not an error
// (half the real ones look like that). A torn record keeps its last good
// value with a ProblemTorn; job records are published by replace, so there is
// no in-place window to retry across.
func (j *Jobs) Read(dirs []string, permit func(dir string) bool) JobsResult {
	var out JobsResult
	out.State = model.SourceOK
	degrade := func(p model.Problem) {
		out.Problems = append(out.Problems, p)
		out.State = model.SourceDegraded
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		seen[dir] = true
		name := path.Join(dir, claude.JobStateFile)
		if !permit(dir) {
			degrade(model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemNotGranted, Path: name,
				Detail: "its directory was not armed before this cycle's read; the next cycle reads it"})
			j.keep(&out, dir)
			continue
		}
		b, err := readBounded(j.tree.FS, name)
		if errors.Is(err, fs.ErrNotExist) {
			delete(j.known, dir)
			continue
		}
		if err != nil {
			degrade(model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemUnreadable, Path: name, Detail: err.Error()})
			j.keep(&out, dir)
			continue
		}
		job, ps, err := claude.DecodeJob(dir, b)
		if err != nil {
			degrade(model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemTorn, Path: name,
				Detail: "not a JSON object; the last good value is kept when there is one"})
			j.keep(&out, dir)
			continue
		}
		out.Problems = append(out.Problems, ps...)
		j.known[dir] = job
		out.Jobs = append(out.Jobs, job)
	}
	for dir := range j.known {
		if !seen[dir] {
			delete(j.known, dir)
		}
	}
	return out
}

func (j *Jobs) keep(out *JobsResult, dir string) {
	if job, ok := j.known[dir]; ok {
		out.Jobs = append(out.Jobs, job)
	}
}
