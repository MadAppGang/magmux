// Command probe is an INDEPENDENT ground-truth reader for the document
// `magmux agents` publishes. It shares no code with the service: it decodes
// Claude Code's records with its own field mapping and its own rules, so
// agreement between the two means something. A probe built on the service's own
// decoder would agree with it by construction, and would have missed every
// defect live validation actually found.
//
// It replaces the Python harnesses this feature was first validated with
// (ac1-5-truth.py, ac1-5-compare.py, ac5-recheck.py), and it does in one pass
// what those did in three: the tail count and the whole-file count are computed
// together, so AC5 is an exact equality rather than an expected mismatch that
// needed a second script to interpret.
//
// # Running it
//
// It lives in the claudestate module as claudestate/_probe. The directory
// begins with "_", which every `./...` package pattern skips, so `go build
// ./...` does not see it; CI builds and vets it by naming it (`./_probe`). It
// imports the standard library ONLY and nothing in the module imports it
// (guard G13), so it can never agree with the library by construction. It must
// never become a test. Run it by hand (architecture §4.8):
//
//	go build -o /tmp/magmux ./cmd/magmux && /tmp/magmux agents --once --json > /tmp/doc.json
//	go -C claudestate run ./_probe -doc /tmp/doc.json -expect-jobs
//	go -C claudestate run ./_probe -root SESSIONS -jobs JOBS -projects PROJECTS -doc FILE -expect-jobs
//	go -C claudestate run ./_probe -truth-json > /tmp/truth.json
//	go -C claudestate run ./_probe -budget
//
// -doc names the document to compare against (-service is its older spelling
// and still works). -expect-jobs reads JOBS (jobs/*/state.json) as well as the
// registry and runs the job checks; without it no job record is read, so a
// document carrying job rows reports them as unexplained. -truth-json prints
// the ground truth to STDOUT. -budget prints this probe's own quarter of
// RLIMIT_NOFILE, V3's descriptor comparator, computed without the library.
// The probe writes nothing, not even a file it was asked to: that is what lets
// guard G2 hold it to "no writes" with no exception.
//
// It exits 0 when no criterion FAILED, and 1 when one did. A criterion whose
// precondition never occurred on the machine is reported NOT VERIFIED, never
// PASS: the absence of a case is not evidence about it.
//
// # How rows are matched (architecture §3.4 and §9's V1 method)
//
// Every record is KEYED from its own fields, by this probe's own reading of
// the spec, and never from another record:
//
//	K1  registry, kind "bg", jobId and sessionId set  -> (id, job: jobId)
//	K2  job record with a sessionId                   -> (id, job: daemonShort, else <dir>)
//	    job record without one                        -> orphan "jobs/<dir>"
//	K3  any other registry record with an id or a pid  -> (id, pid: body, else <pid>.json)
//	K4  registry record with neither                   -> orphan <file name>
//
// Records that produce one key are ONE expected session (a twin, two claimants
// of one job, a stale <pid>.json beside a renamed one). The document's rows
// are keyed the same way: a row with a `job` member is (id, job), a row with
// neither an id nor a pid is an orphan, and any other row is (id, pid).
// Orphans are matched by count.
//
// # What it is held to
//
//   - It never writes under ~/.claude. It opens files there for reading only.
//   - The only probe it sends a process is kill(pid, 0), which delivers no
//     signal. The validation criteria permit exactly that and nothing else.
//   - It never reads a *.key file, and it never speaks to tmux.
//
// It is NOT a test and must never become one. No test in this repository may
// read ~/.claude or probe a pid it does not own; TestNoTestCanReachTheSignalOracle
// scans the source to enforce it.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tailWindow is how much of a transcript the service examines, from the END.
// It is stated here as a number rather than imported, because a probe that took
// the constant from the code under test could not disagree with it.
const tailWindow = 200 << 10

// apiErrorPatterns is the whole of this probe's rule for "an API-level error".
// The field is Claude Code's own `isApiErrorMessage`, never `is_error`: all 497
// observed `is_error` records were recovered turns, and counting them flagged
// 29 of 32 healthy sessions as broken.
var apiErrorPatterns = [][]byte{
	[]byte(`"isApiErrorMessage":true`),
	[]byte(`"isApiErrorMessage": true`),
}

// waitingVocabulary maps Claude Code's spelling to the document's. A value that
// is absent here is a value the service must publish as null while keeping the
// raw string — that is how `"dialog open"`, which Claude Code began writing
// during this feature's own validation run, was handled without a code change.
var waitingVocabulary = map[string]string{
	"input needed":      "input",
	"permission prompt": "permission",
}

// key is one expected session's identity, §3.4's K1–K4 as this probe reads
// them. Exactly one of (PID), (Job) and (Orphan) identifies it beside ID.
type key struct {
	ID     string `json:"id"`
	PID    int    `json:"pid,omitempty"`
	Job    string `json:"job,omitempty"`
	Orphan string `json:"orphan,omitempty"`
}

func (k key) String() string {
	switch {
	case k.Orphan != "":
		return "orphan " + k.Orphan
	case k.Job != "":
		return fmt.Sprintf("(%s, job %s)", k.ID, k.Job)
	}
	return fmt.Sprintf("(%s, pid %d)", k.ID, k.PID)
}

func (k key) less(o key) bool {
	if k.ID != o.ID {
		return k.ID < o.ID
	}
	if k.Job != o.Job {
		return k.Job < o.Job
	}
	if k.PID != o.PID {
		return k.PID < o.PID
	}
	return k.Orphan < o.Orphan
}

// record is one registry file as this probe reads it.
type record struct {
	File            string `json:"file"`
	PID             int    `json:"pid"`
	ID              string `json:"id"`
	Kind            string `json:"kind,omitempty"`
	JobID           string `json:"jobId,omitempty"`
	Key             key    `json:"key"`
	Status          string `json:"status"`
	WaitingFor      string `json:"waitingFor,omitempty"`
	StatusUpdatedAt *int64 `json:"statusUpdatedAt"`
	ProcessAlive    string `json:"processAlive"`

	// SharedIdentity is true when another live record claims the same session
	// id — a session resumed into a second terminal while the first still runs.
	// The service keys its actors by session id, so the second record is
	// published as read with no middleware: no pane, no transcript facts,
	// attention undetermined. That is safe and never wrong, but it discards
	// facts about a live session, so this probe reports those rows as a known
	// LIMITATION rather than as a failure — and reports an undetermined value
	// WITHOUT this explanation as a failure, which is what it is.
	SharedIdentity bool `json:"sharedIdentity,omitempty"`

	// ModTime is when the registry file was last written. It exists to bound a
	// race the probe itself creates: the service's snapshot and this probe's
	// read are seconds apart, and Claude Code rewrites these records
	// continuously. A record written AFTER the snapshot's epoch legitimately
	// disagrees with it, and calling that a defect would make the harness fail
	// at random. Reading mtime is a read of metadata, not of content.
	ModTime time.Time `json:"modTime"`

	// TranscriptModTime bounds the same race for the facts DERIVED from a
	// transcript: a session that wrote a turn between the two reads changes its
	// own error count and subagent count.
	TranscriptModTime time.Time `json:"transcriptModTime,omitempty"`

	TranscriptsFound int    `json:"transcriptsFound"`
	Transcript       string `json:"transcript,omitempty"`
	TranscriptBytes  int64  `json:"transcriptBytes,omitempty"`
	ErrorsInTail     *int   `json:"apiErrorsInTail,omitempty"`
	ErrorsWholeFile  *int   `json:"apiErrorsWholeFile,omitempty"`
	Subagents        *int   `json:"subagents,omitempty"`
}

// jobRecord is one jobs/<dir>/state.json as this probe reads it.
type jobRecord struct {
	Dir       string    `json:"dir"`
	ID        string    `json:"id"`
	Short     string    `json:"daemonShort,omitempty"`
	State     string    `json:"state"`
	UpdatedAt string    `json:"updatedAt,omitempty"`
	Key       key       `json:"key"`
	ModTime   time.Time `json:"modTime"`
}

// truth is what -truth-json prints: the machine as this probe read it.
type truth struct {
	ReadAt     time.Time           `json:"readAt"`
	Root       string              `json:"root"`
	Jobs       string              `json:"jobs,omitempty"`
	Projects   string              `json:"projects"`
	TailWindow int64               `json:"tailWindow"`
	ReadJobs   bool                `json:"readJobs"`
	Records    []record            `json:"records"`
	JobRecords []jobRecord         `json:"jobRecords"`
	Unreadable []map[string]string `json:"unreadable"`
}

type svcFault struct {
	APIErrors *int   `json:"apiErrors"`
	Detail    string `json:"detail"`
}

type svcSession struct {
	ID            string    `json:"id"`
	PID           int       `json:"pid"`
	Job           string    `json:"job"`
	Status        string    `json:"status"`
	StatusRaw     string    `json:"statusRaw"`
	StatusSince   *string   `json:"statusSince"`
	WaitingOn     *string   `json:"waitingOn"`
	ProcessAlive  *string   `json:"processAlive"`
	SubagentCount *int      `json:"subagentCount"`
	Fault         *svcFault `json:"fault"`
}

// rowKey keys a document row the way §9's V1 method says: a `job` member is
// (id, job), a row with neither an id nor a pid is an orphan, anything else is
// (id, pid). Orphans carry no identity, so they are matched by count.
func (s svcSession) rowKey() (key, bool) {
	switch {
	case s.Job != "":
		return key{ID: s.ID, Job: s.Job}, true
	case s.ID == "" && s.PID == 0:
		return key{}, false
	}
	return key{ID: s.ID, PID: s.PID}, true
}

type svcDoc struct {
	Epoch    json.RawMessage `json:"epoch"`
	Sessions []svcSession    `json:"sessions"`
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("cannot resolve the home directory: %v", err)
	}
	root := flag.String("root", filepath.Join(home, ".claude", "sessions"),
		"the session registry to read (read-only)")
	jobs := flag.String("jobs", filepath.Join(home, ".claude", "jobs"),
		"the background-jobs directory, read only under -expect-jobs (read-only)")
	projects := flag.String("projects", filepath.Join(home, ".claude", "projects"),
		"where transcripts live (read-only)")
	doc := flag.String("doc", "",
		"JSON from `magmux agents --once --json`; without it only ground truth is produced")
	service := flag.String("service", "", "the older spelling of -doc")
	expectJobs := flag.Bool("expect-jobs", false,
		"read -jobs as well as the registry, and run the job checks")
	truthJSON := flag.Bool("truth-json", false,
		"print the ground truth this probe read to stdout, as JSON; cannot be combined with -doc")
	budget := flag.Bool("budget", false,
		"print this probe's own quarter of RLIMIT_NOFILE (V3's descriptor comparator) and exit")
	flag.Parse()

	if *budget {
		if err := printBudget(); err != nil {
			fatal("%v", err)
		}
		return
	}
	if *doc != "" && *service != "" && *doc != *service {
		fatal("-doc and -service are two spellings of one flag; give one")
	}
	if *doc == "" {
		doc = service
	}
	if *truthJSON && *doc != "" {
		fatal("-truth-json prints JSON to stdout and -doc prints a report there; run them separately")
	}

	t, err := readTruth(*root, *projects)
	if err != nil {
		fatal("%v", err)
	}
	if *expectJobs {
		if err := readJobs(&t, *jobs); err != nil {
			fatal("%v", err)
		}
	}

	if *truthJSON {
		if err := printJSON(t); err != nil {
			fatal("%v", err)
		}
		return
	}

	if *doc == "" {
		fmt.Printf("read %d records from %s and %d job records; no -doc given, so nothing was compared\n",
			len(t.Records), *root, len(t.JobRecords))
		return
	}

	d, err := readService(*doc)
	if err != nil {
		fatal("%v", err)
	}
	if failed := compare(t, d); failed {
		os.Exit(1)
	}
}

// printBudget is V3's comparator: a quarter of the soft RLIMIT_NOFILE this
// process got, which a Go process launched from the same shell shares (the
// runtime raises the soft limit at start, for every Go binary alike).
func printBudget() error {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return fmt.Errorf("reading RLIMIT_NOFILE: %w", err)
	}
	fmt.Printf("RLIMIT_NOFILE soft %d hard %d; budget (a quarter of the soft limit) %d descriptors\n",
		lim.Cur, lim.Max, lim.Cur/4)
	return nil
}

// ---------------------------------------------------------------- ground truth

// pidFromName reads "<pid>.json": canonical decimal digits, greater than zero.
func pidFromName(name string) (int, bool) {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok || base == "" {
		return 0, false
	}
	n, err := strconv.Atoi(base)
	if err != nil || n <= 0 || strconv.Itoa(n) != base {
		return 0, false
	}
	return n, true
}

// registryKey is K1, K3 or K4 for one registry record, from its own fields.
func registryKey(r record) key {
	switch {
	case r.Kind == "bg" && r.JobID != "" && r.ID != "":
		return key{ID: r.ID, Job: r.JobID}
	case r.ID != "" || r.PID > 0:
		return key{ID: r.ID, PID: r.PID}
	}
	return key{Orphan: r.File}
}

// jobKey is K2 for one job record, from its own fields.
func jobKey(j jobRecord) key {
	if j.ID == "" {
		return key{Orphan: "jobs/" + j.Dir}
	}
	short := j.Short
	if short == "" {
		short = j.Dir
	}
	return key{ID: j.ID, Job: short}
}

func readTruth(root, projects string) (truth, error) {
	t := truth{
		ReadAt:     time.Now().UTC(),
		Root:       root,
		Projects:   projects,
		TailWindow: tailWindow,
		Records:    []record{},
		JobRecords: []jobRecord{},
		Unreadable: []map[string]string{},
	}
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		return t, fmt.Errorf("globbing %s: %w", root, err)
	}
	sort.Strings(files)

	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Unreadable = append(t.Unreadable, map[string]string{
				"file": filepath.Base(path), "error": err.Error()})
			continue
		}
		var d struct {
			PID             *int         `json:"pid"`
			SessionID       string       `json:"sessionId"`
			Kind            string       `json:"kind"`
			JobID           string       `json:"jobId"`
			Status          string       `json:"status"`
			WaitingFor      string       `json:"waitingFor"`
			StatusUpdatedAt *json.Number `json:"statusUpdatedAt"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Unreadable = append(t.Unreadable, map[string]string{
				"file": filepath.Base(path), "error": truncate(err.Error(), 120)})
			continue
		}

		r := record{
			File:       filepath.Base(path),
			ID:         d.SessionID,
			Kind:       d.Kind,
			JobID:      d.JobID,
			Status:     d.Status,
			WaitingFor: d.WaitingFor,
		}
		if st, err := os.Stat(path); err == nil {
			r.ModTime = st.ModTime().UTC()
		}
		// The body's pid, else the file name's (D11): the name is an index
		// into the record, and only stands in when the body says nothing.
		if d.PID != nil && *d.PID > 0 {
			r.PID = *d.PID
		} else if n, ok := pidFromName(r.File); ok {
			r.PID = n
		}
		if r.PID > 0 {
			r.ProcessAlive = alive(r.PID)
		} else {
			r.ProcessAlive = "unknown"
		}
		r.Key = registryKey(r)
		if d.StatusUpdatedAt != nil {
			if ms, err := d.StatusUpdatedAt.Int64(); err == nil {
				r.StatusUpdatedAt = &ms
			}
		}

		// Locate by SESSION ID across every project directory, never by a slug
		// derived from a cwd. A derived slug resolves ~96% of sessions and the
		// 4% that miss land in a directory that EXISTS holding another
		// session's transcripts, so the failure is a wrong answer rather than a
		// miss.
		hits, err := filepath.Glob(filepath.Join(projects, "*", r.ID+".jsonl"))
		if err == nil && r.ID != "" {
			r.TranscriptsFound = len(hits)
			if len(hits) == 1 {
				r.Transcript = hits[0]
				if st, err := os.Stat(hits[0]); err == nil {
					r.TranscriptModTime = st.ModTime().UTC()
				}
				tail, whole, size, err := countErrors(hits[0])
				if err == nil {
					r.ErrorsInTail = &tail
					r.ErrorsWholeFile = &whole
					r.TranscriptBytes = size
				}
				n := subagents(hits[0])
				r.Subagents = &n
			}
		}
		t.Records = append(t.Records, r)
	}

	// Mark every record whose session id another record also claims. Done after
	// the walk, because the second claimant is only visible once both are read.
	seen := map[string]int{}
	for _, r := range t.Records {
		if r.ID != "" {
			seen[r.ID]++
		}
	}
	for i := range t.Records {
		if seen[t.Records[i].ID] > 1 {
			t.Records[i].SharedIdentity = true
		}
	}
	return t, nil
}

// readJobs reads jobs/<dir>/state.json, one level deep and nothing else in a
// job directory. A missing jobs directory is a machine with no background
// jobs, not an error.
func readJobs(t *truth, dir string) error {
	t.Jobs, t.ReadJobs = dir, true
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "state.json")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Unreadable = append(t.Unreadable, map[string]string{"file": "jobs/" + e.Name(), "error": err.Error()})
			continue
		}
		var d struct {
			SessionID   string `json:"sessionId"`
			DaemonShort string `json:"daemonShort"`
			State       string `json:"state"`
			UpdatedAt   string `json:"updatedAt"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Unreadable = append(t.Unreadable, map[string]string{
				"file": "jobs/" + e.Name(), "error": truncate(err.Error(), 120)})
			continue
		}
		j := jobRecord{Dir: e.Name(), ID: d.SessionID, Short: d.DaemonShort, State: d.State, UpdatedAt: d.UpdatedAt}
		if st, err := os.Stat(path); err == nil {
			j.ModTime = st.ModTime().UTC()
		}
		j.Key = jobKey(j)
		t.JobRecords = append(t.JobRecords, j)
	}
	return nil
}

// alive is kill(pid, 0), which delivers no signal. EPERM means the process
// exists and is not ours, which is ALIVE — reporting it dead would drop every
// session started by another user from the document.
func alive(pid int) string {
	if pid <= 0 {
		return "unknown"
	}
	switch err := syscall.Kill(pid, 0); err {
	case nil:
		return "yes"
	case syscall.ESRCH:
		return "no"
	case syscall.EPERM:
		return "yes"
	default:
		return "unknown"
	}
}

// subagents counts the meta files beside a transcript. An ABSENT directory is a
// determined zero, not undetermined: measured over 3189 live transcripts, 2997
// (94%) have no subagents directory at all, because Claude Code creates it
// lazily. Treating absent as unknown would blank the column for 94% of rows.
func subagents(transcript string) int {
	dir := filepath.Join(filepath.Dir(transcript),
		strings.TrimSuffix(filepath.Base(transcript), ".jsonl"), "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".meta.json") {
			n++
		}
	}
	return n
}

// countErrors returns the API-error count inside the tail window, the count
// across the WHOLE file, and the file's size. Both numbers matter: the service
// publishes the tail count, and the difference between the two is exactly what
// the 200 KB truncation hides — it was 13 of 36 sessions on this machine, every
// one published as a bare zero before fix round 1.
func countErrors(path string) (tail, whole int, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return 0, 0, 0, err
	}
	size = st.Size()

	off := size - tailWindow
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, 0, size, err
	}
	var r io.Reader = f
	if off > 0 {
		// A window that starts mid-record drops that half-record rather than
		// counting it, which is what the service does and what any bounded
		// window must do to avoid decoding a fragment.
		br := bufio.NewReaderSize(f, 1<<20)
		if _, err := br.ReadString('\n'); err != nil && err != io.EOF {
			return 0, 0, size, err
		}
		r = br
	}
	if tail, err = countIn(r); err != nil {
		return 0, 0, size, err
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return tail, 0, size, err
	}
	if whole, err = countIn(f); err != nil {
		return tail, 0, size, err
	}
	return tail, whole, size, nil
}

// countIn counts occurrences of the API-error flag in a byte stream, in bounded
// memory. It does not split the stream into records: the flag is a top-level
// field that appears at most once per record, and the machine holds a 630 MB
// transcript, so buffering a whole record is not safe.
func countIn(r io.Reader) (int, error) {
	const chunk = 1 << 20
	longest := 0
	for _, p := range apiErrorPatterns {
		if len(p) > longest {
			longest = len(p)
		}
	}
	buf := make([]byte, chunk)
	var carry []byte
	n := 0
	for {
		m, readErr := r.Read(buf)
		if m > 0 {
			seg := make([]byte, 0, len(carry)+m)
			seg = append(seg, carry...)
			seg = append(seg, buf[:m]...)
			base := len(carry)
			for _, p := range apiErrorPatterns {
				for off := 0; ; {
					i := bytes.Index(seg[off:], p)
					if i < 0 {
						break
					}
					at := off + i
					// Count a match only when it ENDS inside the new bytes.
					// One entirely inside the carry was already counted when
					// the carry was new.
					if at+len(p) > base {
						n++
					}
					off = at + 1
				}
			}
			keep := longest - 1
			if keep > len(seg) {
				keep = len(seg)
			}
			carry = append(carry[:0:0], seg[len(seg)-keep:]...)
		}
		if readErr == io.EOF {
			return n, nil
		}
		if readErr != nil {
			return n, readErr
		}
	}
}

// ------------------------------------------------------------------- comparing

func readService(path string) (svcDoc, error) {
	var doc svcDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("decoding %s: %w", path, err)
	}
	return doc, nil
}

// verdicts collects results so the summary cannot disagree with the body.
type verdicts struct {
	pass, fail, notVerified, limitation []string
}

// limited records a KNOWN limitation: the document published nothing wrong, and
// something determinable was nonetheless left out. It is not a pass and not a
// failure, and collapsing it into either would lose the distinction that makes
// it worth reporting.
func (v *verdicts) limited(name, detail string) {
	v.limitation = append(v.limitation, name)
	fmt.Printf("  LIMITATION   %s — %s\n", name, detail)
}

func (v *verdicts) passed(name, detail string) {
	v.pass = append(v.pass, name)
	fmt.Printf("  PASS         %s — %s\n", name, detail)
}

func (v *verdicts) failed(name, detail string) {
	v.fail = append(v.fail, name)
	fmt.Printf("  FAIL         %s — %s\n", name, detail)
}

func (v *verdicts) unverified(name, detail string) {
	v.notVerified = append(v.notVerified, name)
	fmt.Printf("  NOT VERIFIED %s — %s\n", name, detail)
}

// expected is one expected session: a key and every record that produced it,
// registry claimants in byte-wise file order, then job records.
type expected struct {
	key     key
	records []record
	jobs    []jobRecord
}

// speaker is the registry claimant whose words a (id, pid) session carries:
// the lowest file name, byte-wise (§3.4 Rule 2, a K3 duplicate).
func (e *expected) speaker() (record, bool) {
	if len(e.records) == 0 {
		return record{}, false
	}
	return e.records[0], true
}

// movedAfter reports whether every claimant was written after the snapshot:
// a session that began during the run, not one the service dropped.
func (e *expected) movedAfter(epoch time.Time) bool {
	if epoch.IsZero() {
		return false
	}
	for _, r := range e.records {
		if !r.ModTime.After(epoch) {
			return false
		}
	}
	for _, j := range e.jobs {
		if !j.ModTime.After(epoch) {
			return false
		}
	}
	return true
}

// world is the two sides keyed: what the records say should be published, and
// what was.
type world struct {
	exp        map[key]*expected
	orphans    []string // expected orphans: a file name, or "jobs/<dir>"
	rows       map[key][]svcSession
	orphanRows int
	readJobs   bool
}

func keyed(t truth, doc svcDoc) world {
	w := world{exp: map[key]*expected{}, rows: map[key][]svcSession{}, readJobs: t.ReadJobs}
	get := func(k key) *expected {
		e := w.exp[k]
		if e == nil {
			e = &expected{key: k}
			w.exp[k] = e
		}
		return e
	}
	files := append([]record(nil), t.Records...)
	sort.Slice(files, func(i, j int) bool { return files[i].File < files[j].File })
	for _, r := range files {
		if r.Key.Orphan != "" {
			w.orphans = append(w.orphans, r.Key.Orphan)
			continue
		}
		e := get(r.Key)
		e.records = append(e.records, r)
	}
	for _, j := range t.JobRecords {
		if j.Key.Orphan != "" {
			w.orphans = append(w.orphans, j.Key.Orphan)
			continue
		}
		e := get(j.Key)
		e.jobs = append(e.jobs, j)
	}
	for _, s := range doc.Sessions {
		k, ok := s.rowKey()
		if !ok {
			w.orphanRows++
			continue
		}
		w.rows[k] = append(w.rows[k], s)
	}
	return w
}

// byPIDKey is the (id, pid) half of the world: the speaker of every expected
// (id, pid) session, and the row for every (id, pid) key. AC2–AC5's
// per-record checks run over it, as they did before rows had keys.
func (w world) byPIDKey() (map[key]record, map[key]svcSession) {
	probe, svc := map[key]record{}, map[key]svcSession{}
	for k, e := range w.exp {
		if k.Job != "" {
			continue
		}
		if r, ok := e.speaker(); ok {
			probe[k] = r
		}
	}
	for k, ss := range w.rows {
		if k.Job == "" {
			svc[k] = ss[0]
		}
	}
	return probe, svc
}

func compare(t truth, doc svcDoc) (anyFailed bool) {
	fmt.Printf("probe read %d records from %s at %s", len(t.Records), t.Root, t.ReadAt.Format(time.RFC3339))
	if t.ReadJobs {
		fmt.Printf(", and %d job records from %s", len(t.JobRecords), t.Jobs)
	}
	fmt.Println()
	fmt.Printf("service published %d sessions; epoch %s\n", len(doc.Sessions), string(doc.Epoch))
	if len(t.Unreadable) > 0 {
		fmt.Printf("records the probe could not read: %d\n", len(t.Unreadable))
		for _, u := range t.Unreadable {
			fmt.Printf("    %s: %s\n", u["file"], u["error"])
		}
	}
	fmt.Println()

	// The snapshot's own instant, used to tell a record that moved on after the
	// snapshot from a value the service got wrong.
	var epoch time.Time
	var epochStr string
	if err := json.Unmarshal(doc.Epoch, &epochStr); err == nil {
		if parsed, err := time.Parse(time.RFC3339, epochStr); err == nil {
			epoch = parsed.UTC()
			fmt.Printf("the probe read the registry %.0f s after the snapshot; "+
				"a record written inside that gap is reported RACED, not FAILED\n\n",
				t.ReadAt.Sub(epoch).Seconds())
		}
	}

	w := keyed(t, doc)
	probe, svc := w.byPIDKey()

	var v verdicts
	ac1(&v, w, epoch)
	ac2(&v, probe, svc, epoch)
	ac2Jobs(&v, w)
	ac3(&v, probe, svc, epoch)
	ac3Jobs(&v, w)
	ac4(&v, probe, svc, epoch)
	ac5(&v, probe, svc, t.TailWindow, epoch)

	fmt.Printf("\n%d PASS   %d FAIL   %d NOT VERIFIED   %d LIMITATION\n",
		len(v.pass), len(v.fail), len(v.notVerified), len(v.limitation))
	if len(v.limitation) > 0 {
		fmt.Printf("known limitation, nothing wrong published: %s\n",
			strings.Join(v.limitation, ", "))
	}
	if len(v.fail) > 0 {
		fmt.Printf("FAILED: %s\n", strings.Join(v.fail, ", "))
		return true
	}
	if len(v.notVerified) > 0 {
		fmt.Printf("not verified (the precondition did not occur; needs a test-data substitution): %s\n",
			strings.Join(v.notVerified, ", "))
	}
	return false
}

// ac1 — the set of expected keys equals the set of row keys, each exactly once;
// orphans match by count; liveness agrees on every (id, pid) row.
func ac1(v *verdicts, w world, epoch time.Time) {
	fmt.Println("AC1 — the live set, by key")
	var missing, extra, twice, liveDisagree, appeared []string
	probeDead, svcDead, jobKeys := 0, 0, 0
	for _, k := range sortedKeys(w.exp) {
		e := w.exp[k]
		if k.Job != "" {
			jobKeys++
		}
		rows := w.rows[k]
		switch {
		case len(rows) == 0 && e.movedAfter(epoch):
			appeared = append(appeared, k.String())
		case len(rows) == 0:
			missing = append(missing, k.String())
		case len(rows) > 1:
			twice = append(twice, fmt.Sprintf("%s (%d rows)", k, len(rows)))
		}
		if k.Job != "" {
			continue
		}
		r, ok := e.speaker()
		if !ok {
			continue
		}
		if r.ProcessAlive == "no" {
			probeDead++
		}
		if len(rows) > 0 && rows[0].ProcessAlive != nil && *rows[0].ProcessAlive != r.ProcessAlive {
			liveDisagree = append(liveDisagree, fmt.Sprintf("%s: probe %s, document %s", k, r.ProcessAlive, *rows[0].ProcessAlive))
		}
	}
	unexplainedJobs := 0
	for _, k := range sortedKeys(w.rows) {
		if _, ok := w.exp[k]; !ok {
			extra = append(extra, k.String())
			if k.Job != "" {
				unexplainedJobs++
			}
		}
		if s := w.rows[k][0]; k.Job == "" && s.ProcessAlive != nil && *s.ProcessAlive == "no" {
			svcDead++
		}
	}
	fmt.Printf("  probe: %d keys (%d job-keyed) and %d orphan(s), from %d records\n",
		len(w.exp), jobKeys, len(w.orphans), len(w.orphans)+countRecords(w))
	fmt.Printf("  service: %d keyed rows and %d orphan row(s)\n", len(w.rows), w.orphanRows)
	if len(appeared) > 0 {
		fmt.Printf("    RACED %v were written after the snapshot and are excluded\n", appeared)
	}

	switch {
	case len(missing) > 0:
		v.failed("AC1 live set", fmt.Sprintf("the document has no row for %v, whose records predate the snapshot", missing))
	case len(extra) > 0:
		detail := fmt.Sprintf("the document has rows no record explains: %v", extra)
		if unexplainedJobs > 0 && !w.readJobs {
			detail += "; job-keyed rows need the jobs tree: run with -expect-jobs"
		} else {
			detail += " (a record removed during the run explains one; re-run before believing it)"
		}
		v.failed("AC1 live set", detail)
	case len(twice) > 0:
		v.failed("AC1 live set", fmt.Sprintf("keys published more than once: %v", twice))
	case len(w.orphans) != w.orphanRows:
		v.failed("AC1 live set", fmt.Sprintf("%d orphan record(s) %v, but %d orphan row(s)", len(w.orphans), w.orphans, w.orphanRows))
	case len(liveDisagree) > 0:
		v.failed("AC1 live set", fmt.Sprintf("liveness disagrees: %v", liveDisagree))
	default:
		v.passed("AC1 live set", fmt.Sprintf("%d key(s) and %d orphan(s), each published exactly once; liveness agrees on every (id, pid) row",
			len(w.exp)-len(appeared), len(w.orphans)))
	}

	switch {
	case !w.readJobs:
		v.unverified("AC1 job keys", "the jobs tree was not read; run with -expect-jobs")
	case jobKeys == 0:
		v.unverified("AC1 job keys", "no record on this machine is job-keyed; substitute a twin and bg-two-claimants from testdata")
	default:
		v.passed("AC1 job keys", fmt.Sprintf("%d job-keyed session(s), each one row", jobKeys))
	}

	if probeDead == 0 {
		v.unverified("AC1 dead-pid exclusion",
			"every record on this machine has a live pid; substitute testdata 60001.json")
	} else if svcDead == probeDead {
		v.passed("AC1 dead-pid exclusion", fmt.Sprintf("%d dead (id, pid) session(s) agreed", probeDead))
	} else {
		v.failed("AC1 dead-pid exclusion",
			fmt.Sprintf("probe found %d dead (id, pid) session(s), the document reports %d", probeDead, svcDead))
	}
	fmt.Println()
}

func countRecords(w world) int {
	n := 0
	for _, e := range w.exp {
		n += len(e.records) + len(e.jobs)
	}
	return n
}

// ac2Jobs — a job-keyed row says it is waiting on something only when one of
// its registry claimants says waiting, for that reason (§9 AC2). The exact
// merged value is V2's to check, because tt implements it.
func ac2Jobs(v *verdicts, w world) {
	fmt.Println("AC2 — job rows: waitingOn is a claimant's word")
	if !w.readJobs {
		v.unverified("AC2 job rows", "the jobs tree was not read; run with -expect-jobs")
		fmt.Println()
		return
	}
	compared := 0
	var bad []string
	for _, k := range sortedKeys(w.rows) {
		if k.Job == "" {
			continue
		}
		e := w.exp[k]
		if e == nil {
			continue // AC1 reports it
		}
		for _, s := range w.rows[k] {
			compared++
			if s.WaitingOn == nil {
				continue
			}
			justified := false
			for _, r := range e.records {
				if want, known := waitingVocabulary[r.WaitingFor]; r.Status == "waiting" && known && want == *s.WaitingOn {
					justified = true
				}
			}
			if !justified {
				bad = append(bad, fmt.Sprintf("%s publishes waitingOn %q, and no registry claimant says waiting for it", k, *s.WaitingOn))
			}
		}
	}
	switch {
	case len(bad) > 0:
		v.failed("AC2 job rows", strings.Join(bad, "; "))
	case compared == 0:
		v.unverified("AC2 job rows", "no job-keyed row was published")
	default:
		v.passed("AC2 job rows", fmt.Sprintf("%d job row(s); every non-null waitingOn is a claimant's", compared))
	}
	fmt.Println()
}

// ac3Jobs — a job-keyed row's statusSince is one of its inputs: a registry
// claimant's statusUpdatedAt, or the job's own updatedAt. Never an invented
// instant (§9 AC3).
func ac3Jobs(v *verdicts, w world) {
	fmt.Println("AC3 — job rows: statusSince is one of the inputs")
	if !w.readJobs {
		v.unverified("AC3 job rows", "the jobs tree was not read; run with -expect-jobs")
		fmt.Println()
		return
	}
	compared := 0
	var bad []string
	for _, k := range sortedKeys(w.rows) {
		if k.Job == "" {
			continue
		}
		e := w.exp[k]
		if e == nil {
			continue // AC1 reports it
		}
		for _, s := range w.rows[k] {
			if s.StatusSince == nil {
				continue
			}
			compared++
			got, err := time.Parse(time.RFC3339Nano, *s.StatusSince)
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s: statusSince %q does not parse: %v", k, *s.StatusSince, err))
				continue
			}
			var inputs []string
			match := false
			for _, r := range e.records {
				if r.StatusUpdatedAt != nil {
					inputs = append(inputs, strconv.FormatInt(*r.StatusUpdatedAt, 10)+" ms ("+r.File+")")
					match = match || *r.StatusUpdatedAt == got.UnixMilli()
				}
			}
			for _, j := range e.jobs {
				if at, err := time.Parse(time.RFC3339Nano, j.UpdatedAt); err == nil {
					inputs = append(inputs, j.UpdatedAt+" (jobs/"+j.Dir+")")
					match = match || at.UnixMilli() == got.UnixMilli()
				}
			}
			if !match {
				bad = append(bad, fmt.Sprintf("%s: statusSince %s is none of its inputs %v", k, *s.StatusSince, inputs))
			}
		}
	}
	switch {
	case len(bad) > 0:
		v.failed("AC3 job rows", strings.Join(bad, "; "))
	case compared == 0:
		v.unverified("AC3 job rows", "no job-keyed row carried a statusSince")
	default:
		v.passed("AC3 job rows", fmt.Sprintf("%d compared; every statusSince is a claimant's or the job's own instant", compared))
	}
	fmt.Println()
}

// ac2 — a waiting session says so, and says what it waits on. An unrecognised
// reason must publish null and keep the raw string, never guess.
func ac2(v *verdicts, probe map[key]record, svc map[key]svcSession, epoch time.Time) {
	fmt.Println("AC2 — waiting, and what for")
	waiting := map[key]record{}
	raced := 0
	for k, r := range probe {
		if r.Status != "waiting" {
			continue
		}
		// A record that changed after the snapshot may have BECOME waiting since,
		// so the document is allowed to disagree about it.
		if !epoch.IsZero() && r.ModTime.After(epoch) {
			raced++
			continue
		}
		waiting[k] = r
	}
	if raced > 0 {
		fmt.Printf("    RACED %d waiting record(s) were rewritten after the snapshot and are excluded\n", raced)
	}
	if len(waiting) == 0 {
		v.unverified("AC2 waiting", "no session on this machine is waiting; substitute testdata 1724.json")
		v.unverified("AC2 input vs permission", "neither reason occurred; substitute 1724.json and 60002.json")
		fmt.Println()
		return
	}

	ks := sortedKeys(waiting)
	var bad []string
	sawInput, sawPermission, sawNovel := false, false, false
	for _, k := range ks {
		r := waiting[k]
		s, ok := svc[k]
		want, known := waitingVocabulary[r.WaitingFor]
		got := "<absent>"
		if ok && s.WaitingOn != nil {
			got = *s.WaitingOn
		} else if ok {
			got = "null"
		}
		fmt.Printf("  %s: registry %q -> service %s (status %q)\n", k, r.WaitingFor, got, s.Status)
		switch {
		case !ok:
			bad = append(bad, fmt.Sprintf("%s is waiting but absent from the document", k))
		case known && got != want:
			bad = append(bad, fmt.Sprintf("%s: %q must publish %q, got %s", k, r.WaitingFor, want, got))
		case !known && got != "null":
			bad = append(bad, fmt.Sprintf("%s: unrecognised %q must publish null, got %s", k, r.WaitingFor, got))
		}
		if known && want == "input" {
			sawInput = true
		}
		if known && want == "permission" {
			sawPermission = true
		}
		if !known {
			sawNovel = true
		}
	}
	if len(bad) > 0 {
		v.failed("AC2 waiting", strings.Join(bad, "; "))
	} else {
		note := fmt.Sprintf("%d waiting session(s), every reason published correctly", len(waiting))
		if sawNovel {
			note += "; an unrecognised reason was published as null with the raw value kept"
		}
		v.passed("AC2 waiting", note)
	}
	if sawInput && sawPermission {
		v.passed("AC2 input vs permission", "both reasons occurred and stayed distinct")
	} else {
		v.unverified("AC2 input vs permission",
			"only one of the two reasons occurred; substitute 1724.json and 60002.json")
	}
	fmt.Println()
}

// ac3 — statusSince is the registry's own instant, verbatim to the millisecond,
// and is never "corrected" from transcript activity. The counter-example that
// motivated it: a record whose instant is 1.37 hours OLDER than its own
// transcript's latest write, which is a fact and not an error.
func ac3(v *verdicts, probe map[key]record, svc map[key]svcSession, epoch time.Time) {
	fmt.Println("AC3 — statusSince verbatim from statusUpdatedAt")
	compared, secondsShaped := 0, 0
	var mismatch, raced []string
	var oldest int64
	for _, k := range sortedKeys(probe) {
		r := probe[k]
		s, ok := svc[k]
		if !ok {
			continue
		}
		if r.StatusUpdatedAt == nil || s.StatusSince == nil {
			if (r.StatusUpdatedAt == nil) != (s.StatusSince == nil) {
				mismatch = append(mismatch, fmt.Sprintf("%s: one side is null and the other is not", k))
			}
			continue
		}
		compared++
		// A 10-digit value is SECONDS, which this registry does not use. The
		// service must refuse such a value rather than rescale it, because
		// rescaling a format change silently invents a timestamp.
		if *r.StatusUpdatedAt < 1e12 {
			secondsShaped++
		}
		if oldest == 0 || *r.StatusUpdatedAt < oldest {
			oldest = *r.StatusUpdatedAt
		}
		got, err := time.Parse(time.RFC3339, *s.StatusSince)
		if err != nil {
			mismatch = append(mismatch, fmt.Sprintf("%s: statusSince %q does not parse: %v", k, *s.StatusSince, err))
			continue
		}
		gotMS := got.UnixMilli()
		if gotMS != *r.StatusUpdatedAt {
			note := fmt.Sprintf("%s: registry %d, document %d, drift %d ms",
				k, *r.StatusUpdatedAt, gotMS, gotMS-*r.StatusUpdatedAt)
			if !epoch.IsZero() && r.ModTime.After(epoch) {
				raced = append(raced, note+fmt.Sprintf(" — the record was rewritten %.0f s after the snapshot",
					r.ModTime.Sub(epoch).Seconds()))
			} else {
				mismatch = append(mismatch, note)
			}
		}
	}
	fmt.Printf("  compared %d; millisecond-shaped %d, seconds-shaped %d\n",
		compared, compared-secondsShaped, secondsShaped)
	if oldest > 0 {
		age := time.Since(time.UnixMilli(oldest))
		fmt.Printf("  longest in one status: %.1f h — published as a fact, not flagged as stale\n", age.Hours())
	}
	for _, rc := range raced {
		fmt.Printf("    RACED %s\n", rc)
	}
	if len(mismatch) == 0 && compared > 0 {
		note := fmt.Sprintf("%d compared, 0 drift to the millisecond", compared)
		if len(raced) > 0 {
			note = fmt.Sprintf("%d compared, 0 drift on the %d that did not move during the run; "+
				"%d were rewritten after the snapshot and excluded", compared, compared-len(raced), len(raced))
		}
		v.passed("AC3 statusSince", note)
	} else if compared == 0 {
		v.unverified("AC3 statusSince", "no record carried a statusUpdatedAt")
	} else {
		for _, m := range mismatch {
			fmt.Printf("    %s\n", m)
		}
		v.failed("AC3 statusSince", fmt.Sprintf("%d mismatch(es)", len(mismatch)))
	}
	fmt.Println()
}

// ac4 — a subagent count is a count, and zero is distinct from undetermined.
func ac4(v *verdicts, probe map[key]record, svc map[key]svcSession, epoch time.Time) {
	fmt.Println("AC4 — subagent counts")
	compared, nonZero, raced := 0, 0, 0
	var mismatch, discarded []string
	for _, k := range sortedKeys(probe) {
		r := probe[k]
		s, ok := svc[k]
		if !ok || r.Subagents == nil {
			continue
		}
		if s.SubagentCount == nil {
			// Undetermined is a legitimate answer only when the probe could not
			// look either. The probe DID look, so it is either the known
			// shared-identity limitation or a defect.
			note := fmt.Sprintf("%s: probe counted %d, document says undetermined", k, *r.Subagents)
			if r.SharedIdentity {
				discarded = append(discarded, note+" (its session id is claimed by another live record)")
			} else {
				mismatch = append(mismatch, note)
			}
			continue
		}
		compared++
		if *r.Subagents > 0 {
			nonZero++
		}
		if *s.SubagentCount != *r.Subagents {
			if !epoch.IsZero() && r.TranscriptModTime.After(epoch) {
				raced++
				fmt.Printf("    RACED %s: probe %d, document %d — the session wrote during the run\n",
					k, *r.Subagents, *s.SubagentCount)
			} else {
				mismatch = append(mismatch, fmt.Sprintf("%s: probe %d, document %d", k, *r.Subagents, *s.SubagentCount))
			}
		}
	}
	fmt.Printf("  compared %d; sessions with more than zero subagents: %d\n", compared, nonZero)
	for _, d := range discarded {
		fmt.Printf("    %s\n", d)
	}
	if len(discarded) > 0 {
		v.limited("AC4 subagents", fmt.Sprintf("%d determinable count(s) discarded to shared identity", len(discarded)))
	}
	if len(mismatch) == 0 && compared > 0 {
		v.passed("AC4 subagents", fmt.Sprintf("%d compared, %d non-zero, 0 mismatches; %d excluded as raced",
			compared, nonZero, raced))
	} else if compared == 0 {
		v.unverified("AC4 subagents", "no session had a locatable transcript")
	} else {
		for _, m := range mismatch {
			fmt.Printf("    %s\n", m)
		}
		v.failed("AC4 subagents", fmt.Sprintf("%d mismatch(es)", len(mismatch)))
	}
	fmt.Println()
}

// ac5 — the API-error count is the count within the examined window, and every
// truncated read discloses its window. The defect this closes: a zero over a
// truncated read was published bare, so real errors outside the last 200 KB
// were reported as no errors at all.
func ac5(v *verdicts, probe map[key]record, svc map[key]svcSession, window int64, epoch time.Time) {
	fmt.Println("AC5 — API errors within the examined window, window disclosed")
	compared, truncated, hidden, inWindow, raced := 0, 0, 0, 0, 0
	var mismatch, undisclosed, discarded []string
	for _, k := range sortedKeys(probe) {
		r := probe[k]
		s, ok := svc[k]
		if !ok || r.ErrorsInTail == nil {
			continue
		}
		if s.Fault == nil || s.Fault.APIErrors == nil {
			note := fmt.Sprintf("%s: probe read the transcript, document says undetermined", k)
			if r.SharedIdentity {
				discarded = append(discarded, note+" (its session id is claimed by another live record)")
			} else {
				mismatch = append(mismatch, note)
			}
			continue
		}
		compared++
		if *s.Fault.APIErrors != *r.ErrorsInTail {
			note := fmt.Sprintf("%s: probe tail %d, document %d (whole file %d, %d bytes)",
				k, *r.ErrorsInTail, *s.Fault.APIErrors, *r.ErrorsWholeFile, r.TranscriptBytes)
			if !epoch.IsZero() && r.TranscriptModTime.After(epoch) {
				raced++
				fmt.Printf("    RACED %s — the session wrote during the run, which moves the window\n", note)
			} else {
				mismatch = append(mismatch, note)
			}
		}
		if *r.ErrorsInTail > 0 {
			inWindow++
		}
		if r.TranscriptBytes > window {
			truncated++
			if s.Fault.Detail == "" {
				undisclosed = append(undisclosed, fmt.Sprintf("%s: %d-byte transcript read as a %d-byte window, no disclosure",
					k, r.TranscriptBytes, window))
			}
			if *r.ErrorsWholeFile > *r.ErrorsInTail {
				hidden++
				fmt.Printf("  %s: %d error(s) in the whole file, %d inside the window — %d outside it\n",
					k, *r.ErrorsWholeFile, *r.ErrorsInTail, *r.ErrorsWholeFile-*r.ErrorsInTail)
			}
		}
	}
	fmt.Printf("  compared %d; truncated reads %d; sessions with errors inside the window %d; "+
		"sessions whose errors sit OUTSIDE it %d\n", compared, truncated, inWindow, hidden)
	for _, d := range discarded {
		fmt.Printf("    %s\n", d)
	}
	if len(discarded) > 0 {
		v.limited("AC5 error count", fmt.Sprintf("%d transcript(s) not examined at all, to shared identity", len(discarded)))
	}

	if len(mismatch) > 0 {
		for _, m := range mismatch {
			fmt.Printf("    %s\n", m)
		}
		v.failed("AC5 error count", fmt.Sprintf("%d mismatch(es) against the window the service examined", len(mismatch)))
	} else if compared == 0 {
		v.unverified("AC5 error count", "no session had a locatable transcript")
	} else {
		v.passed("AC5 error count", fmt.Sprintf(
			"%d compared, every count equals the probe's own window count; %d excluded as raced", compared, raced))
	}

	switch {
	case truncated == 0:
		v.unverified("AC5 window disclosed", "no transcript exceeded the window, so nothing was truncated")
	case len(undisclosed) > 0:
		for _, u := range undisclosed {
			fmt.Printf("    %s\n", u)
		}
		v.failed("AC5 window disclosed", fmt.Sprintf("%d truncated read(s) published no window", len(undisclosed)))
	default:
		v.passed("AC5 window disclosed", fmt.Sprintf("all %d truncated read(s) disclose the window", truncated))
	}

	if inWindow == 0 {
		v.unverified("AC5 as originally worded",
			"no session had an error INSIDE the window; substitute a transcript that does")
	} else {
		v.passed("AC5 as originally worded", fmt.Sprintf("%d session(s) had an in-window error and it was reported", inWindow))
	}
	fmt.Println()
}

// ---------------------------------------------------------------------- helpers

func sortedKeys[V any](m map[key]V) []key {
	out := make([]key, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

func printJSON(v any) error {
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return fmt.Errorf("encoding the ground truth: %w", err)
	}
	_, err = os.Stdout.Write(append(raw, '\n'))
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "probe: "+format+"\n", args...)
	os.Exit(2)
}
