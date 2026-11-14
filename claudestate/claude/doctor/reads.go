package doctor

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// run carries the env plus the reads more than one check needs, so the
// registry is read once rather than six times.
type run struct {
	env Env

	regDone bool
	regs    []regEntry
	regErr  error

	jobsDone bool
	jobs     []jobEntry
	jobsErr  error

	shallowDone bool
	shallow     []transcriptStat
}

type regEntry struct {
	name string
	keys map[string]bool
	rec  claude.Record
	mod  time.Time
}

type jobEntry struct {
	dir    string
	keys   map[string]bool
	job    claude.Job
	mod    time.Time // state.json's
	dirMod time.Time
}

type transcriptStat struct {
	rel  string
	mod  time.Time
	size int64
}

// regularByName refuses a name that is not a regular file, BEFORE anything
// opens it. A blocking open of a FIFO with no writer never returns, so every
// file the doctor opens is judged by name first (review r2 #1, the library's
// internal/source.openRegular for the same reason). fs.Stat follows a
// symlink, as the open would.
func regularByName(fsys fs.FS, name string) error {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

// readBounded reads one record whole, refusing one larger than
// claude.MaxRecordBytes rather than reading it. Its first check is by name,
// before the read opens anything, which is what regularByName is for.
func readBounded(fsys fs.FS, name string) ([]byte, fs.FileInfo, error) {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, info, errors.New("not a regular file")
	}
	if info.Size() > claude.MaxRecordBytes {
		return nil, info, errors.New("larger than claude.MaxRecordBytes")
	}
	b, err := fs.ReadFile(fsys, name)
	return b, info, err
}

func keySet(b []byte) (map[string]bool, bool) {
	keys, ok := claude.TopLevelKeys(b)
	if !ok {
		return nil, false
	}
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m, true
}

func (d *run) registry() ([]regEntry, error) {
	if d.regDone {
		return d.regs, d.regErr
	}
	d.regDone = true
	fsys := d.env.Root.Sessions.FS
	if fsys == nil {
		d.regErr = errors.New("no registry tree was given")
		return nil, d.regErr
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		d.regErr = err
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !claude.IsRecordName(e.Name()) {
			continue // a .key sibling is never opened
		}
		b, info, err := readBounded(fsys, e.Name())
		if err != nil {
			continue
		}
		keys, ok := keySet(b)
		if !ok {
			continue
		}
		rec, _, err := claude.DecodeRecord(e.Name(), b)
		if err != nil {
			continue
		}
		d.regs = append(d.regs, regEntry{name: e.Name(), keys: keys, rec: rec, mod: info.ModTime().UTC()})
	}
	return d.regs, nil
}

// jobs reads <dir>/state.json at DEPTH 1 only: the real tree carries decoys
// deeper down.
func (d *run) jobsRead() ([]jobEntry, error) {
	if d.jobsDone {
		return d.jobs, d.jobsErr
	}
	d.jobsDone = true
	fsys := d.env.Root.Jobs.FS
	if fsys == nil {
		d.jobsErr = errors.New("no jobs tree was given")
		return nil, d.jobsErr
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		d.jobsErr = err
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue // pins.json and the like are not jobs
		}
		name := path.Join(e.Name(), claude.JobStateFile)
		b, info, err := readBounded(fsys, name)
		if err != nil {
			continue
		}
		keys, ok := keySet(b)
		if !ok {
			continue
		}
		job, _, err := claude.DecodeJob(e.Name(), b)
		if err != nil {
			continue
		}
		je := jobEntry{dir: e.Name(), keys: keys, job: job, mod: info.ModTime().UTC()}
		if di, err := fs.Stat(fsys, e.Name()); err == nil {
			je.dirMod = di.ModTime().UTC()
		}
		d.jobs = append(d.jobs, je)
	}
	sort.Slice(d.jobs, func(i, j int) bool { return d.jobs[i].dir < d.jobs[j].dir })
	return d.jobs, nil
}

// shallowTranscripts is the NON-RECURSIVE population, */*.jsonl: the glob is
// the check. A recursive walk folds in subagents/, which is not reaped at all.
func (d *run) shallowTranscripts() []transcriptStat {
	if d.shallowDone {
		return d.shallow
	}
	d.shallowDone = true
	fsys := d.env.Root.Projects.FS
	if fsys == nil {
		return nil
	}
	matches, err := fs.Glob(fsys, "*/*"+claude.TranscriptSuffix)
	if err != nil {
		return nil
	}
	for _, m := range matches {
		info, err := fs.Stat(fsys, m)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		d.shallow = append(d.shallow, transcriptStat{rel: m, mod: info.ModTime().UTC(), size: info.Size()})
	}
	return d.shallow
}

func (d *run) newestTranscript() (transcriptStat, bool) {
	files := d.shallowTranscripts()
	if len(files) == 0 {
		return transcriptStat{}, false
	}
	newest := files[0]
	for _, f := range files[1:] {
		if f.mod.After(newest.mod) || (f.mod.Equal(newest.mod) && f.rel < newest.rel) {
			newest = f
		}
	}
	return newest, true
}

// tailLines reads at most n bytes from the END of a file and returns its
// complete, non-blank lines, oldest first. It seeks when the file can, as
// os.DirFS's files do; otherwise it reads and discards the head. The first
// line is dropped unless the read reached the start of the file.
func tailLines(fsys fs.FS, name string, n int) ([][]byte, error) {
	if err := regularByName(fsys, name); err != nil {
		return nil, err
	}
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	var buf []byte
	partial := size > int64(n)
	switch s, ok := f.(io.Seeker); {
	case partial && ok:
		if _, err := s.Seek(size-int64(n), io.SeekStart); err != nil {
			return nil, err
		}
		buf, err = io.ReadAll(io.LimitReader(f, int64(n)))
	default:
		buf, err = io.ReadAll(f)
		if len(buf) > n {
			buf, partial = buf[len(buf)-n:], true
		}
	}
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(buf, []byte{'\n'})
	if partial && len(lines) > 0 {
		lines = lines[1:]
	}
	out := lines[:0]
	for _, l := range lines {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, l)
		}
	}
	return out, nil
}

// counts renders a count map as "value ×n", most frequent first.
func counts(m map[string]int) string {
	type kv struct {
		k string
		n int
	}
	var pairs []kv
	for k, n := range m {
		pairs = append(pairs, kv{k, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].k < pairs[j].k
	})
	var out []string
	for _, p := range pairs {
		k := p.k
		if k == "" {
			k = `""`
		}
		out = append(out, k+" ×"+itoa(p.n))
	}
	return strings.Join(out, ", ")
}

func unknownValues(observed map[string]int, known []string) []string {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	var out []string
	for v := range observed {
		if !set[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
