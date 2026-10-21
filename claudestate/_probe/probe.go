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
// never become a test. Run it by hand:
//
//	go build -o /tmp/magmux ./cmd/magmux && /tmp/magmux agents --once --json > /tmp/svc.json
//	go -C claudestate run ./_probe -service /tmp/svc.json
//	go -C claudestate run ./_probe -truth-json > /tmp/truth.json
//
// -truth-json prints the ground truth to STDOUT. The probe writes nothing, not
// even a file it was asked to: that is what lets guard G2 hold it to "no
// writes" with no exception.
//
// It exits 0 when no criterion FAILED, and 1 when one did. A criterion whose
// precondition never occurred on the machine is reported NOT VERIFIED, never
// PASS: the absence of a case is not evidence about it.
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

// record is one registry file as this probe reads it.
type record struct {
	File            string `json:"file"`
	PID             int    `json:"pid"`
	ID              string `json:"id"`
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

// truth is what -truth-json prints: the machine as this probe read it.
type truth struct {
	ReadAt     time.Time           `json:"readAt"`
	Root       string              `json:"root"`
	Projects   string              `json:"projects"`
	TailWindow int64               `json:"tailWindow"`
	Records    []record            `json:"records"`
	Unreadable []map[string]string `json:"unreadable"`
}

type svcFault struct {
	APIErrors *int   `json:"apiErrors"`
	Detail    string `json:"detail"`
}

type svcSession struct {
	ID            string    `json:"id"`
	PID           int       `json:"pid"`
	Status        string    `json:"status"`
	StatusRaw     string    `json:"statusRaw"`
	StatusSince   *string   `json:"statusSince"`
	WaitingOn     *string   `json:"waitingOn"`
	ProcessAlive  *string   `json:"processAlive"`
	SubagentCount *int      `json:"subagentCount"`
	Fault         *svcFault `json:"fault"`
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
	projects := flag.String("projects", filepath.Join(home, ".claude", "projects"),
		"where transcripts live (read-only)")
	service := flag.String("service", "",
		"JSON from `magmux agents --once --json`; without it only ground truth is produced")
	truthJSON := flag.Bool("truth-json", false,
		"print the ground truth this probe read to stdout, as JSON; cannot be combined with -service")
	flag.Parse()
	if *truthJSON && *service != "" {
		fatal("-truth-json prints JSON to stdout and -service prints a report there; run them separately")
	}

	t, err := readTruth(*root, *projects)
	if err != nil {
		fatal("%v", err)
	}

	if *truthJSON {
		if err := printJSON(t); err != nil {
			fatal("%v", err)
		}
		return
	}

	if *service == "" {
		fmt.Printf("read %d records from %s; no -service given, so nothing was compared\n",
			len(t.Records), *root)
		return
	}

	doc, err := readService(*service)
	if err != nil {
		fatal("%v", err)
	}
	if failed := compare(t, doc); failed {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- ground truth

func readTruth(root, projects string) (truth, error) {
	t := truth{
		ReadAt:     time.Now().UTC(),
		Root:       root,
		Projects:   projects,
		TailWindow: tailWindow,
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
			Status:     d.Status,
			WaitingFor: d.WaitingFor,
		}
		if st, err := os.Stat(path); err == nil {
			r.ModTime = st.ModTime().UTC()
		}
		if d.PID != nil {
			r.PID = *d.PID
			r.ProcessAlive = alive(*d.PID)
		} else {
			r.ProcessAlive = "unknown"
		}
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

func compare(t truth, doc svcDoc) (anyFailed bool) {
	byPID := map[int]record{}
	for _, r := range t.Records {
		byPID[r.PID] = r
	}
	svcByPID := map[int]svcSession{}
	for _, s := range doc.Sessions {
		svcByPID[s.PID] = s
	}

	fmt.Printf("probe read %d records from %s at %s\n",
		len(t.Records), t.Root, t.ReadAt.Format(time.RFC3339))
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

	var v verdicts
	ac1(&v, byPID, svcByPID, epoch)
	ac2(&v, byPID, svcByPID, epoch)
	ac3(&v, byPID, svcByPID, epoch)
	ac4(&v, byPID, svcByPID, epoch)
	ac5(&v, byPID, svcByPID, t.TailWindow, epoch)

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

// ac1 — every live session is listed, and a record whose process is gone is not.
func ac1(v *verdicts, probe map[int]record, svc map[int]svcSession, epoch time.Time) {
	fmt.Println("AC1 — the live set")
	var missing, extra, liveDisagree, appeared []int
	probeLive, probeDead, svcDead := 0, 0, 0
	for pid, r := range probe {
		s, ok := svc[pid]
		if !ok {
			// A record WRITTEN after the snapshot is a session that started
			// during the run, not one the service dropped.
			if !epoch.IsZero() && r.ModTime.After(epoch) {
				appeared = append(appeared, pid)
			} else {
				missing = append(missing, pid)
			}
		}
		if r.ProcessAlive == "yes" {
			probeLive++
		}
		if r.ProcessAlive == "no" {
			probeDead++
		}
		if ok && s.ProcessAlive != nil && *s.ProcessAlive != r.ProcessAlive {
			liveDisagree = append(liveDisagree, pid)
		}
	}
	for pid := range svc {
		if _, ok := probe[pid]; !ok {
			extra = append(extra, pid)
		}
		if s := svc[pid]; s.ProcessAlive != nil && *s.ProcessAlive == "no" {
			svcDead++
		}
	}
	sort.Ints(missing)
	sort.Ints(extra)
	sort.Ints(liveDisagree)
	sort.Ints(appeared)
	fmt.Printf("  probe: %d records, %d live, %d dead\n", len(probe), probeLive, probeDead)
	fmt.Printf("  service: %d sessions, %d reported dead\n", len(svc), svcDead)
	if len(appeared) > 0 {
		fmt.Printf("    RACED pids %v started after the snapshot and are excluded\n", appeared)
	}
	if len(extra) > 0 {
		// The record the service read is gone now, so it was removed between
		// the two reads. This probe cannot prove the service did not invent the
		// pid; it can only say no record exists for it any more.
		fmt.Printf("    RACED pids %v have no record NOW; their record was removed after the snapshot\n", extra)
	}

	compared := len(probe) - len(appeared)
	switch {
	case len(missing) > 0:
		v.failed("AC1 live set", fmt.Sprintf("the service omits pids %v, whose records predate the snapshot", missing))
	case len(liveDisagree) > 0:
		v.failed("AC1 live set", fmt.Sprintf("liveness disagrees for pids %v", liveDisagree))
	default:
		note := fmt.Sprintf("%d = %d, liveness agrees on every one", len(probe), len(svc))
		if len(appeared) > 0 || len(extra) > 0 {
			note = fmt.Sprintf("%d record(s) compared, liveness agrees on every one; %d started and %d ended during the run",
				compared, len(appeared), len(extra))
		}
		v.passed("AC1 live set", note)
	}

	if probeDead == 0 {
		v.unverified("AC1 dead-pid exclusion",
			"every record on this machine has a live pid; substitute testdata 60001.json")
	} else if svcDead == probeDead {
		v.passed("AC1 dead-pid exclusion", fmt.Sprintf("%d dead record(s) agreed", probeDead))
	} else {
		v.failed("AC1 dead-pid exclusion",
			fmt.Sprintf("probe found %d dead, service reported %d", probeDead, svcDead))
	}
	fmt.Println()
}

// ac2 — a waiting session says so, and says what it waits on. An unrecognised
// reason must publish null and keep the raw string, never guess.
func ac2(v *verdicts, probe map[int]record, svc map[int]svcSession, epoch time.Time) {
	fmt.Println("AC2 — waiting, and what for")
	waiting := map[int]record{}
	raced := 0
	for pid, r := range probe {
		if r.Status != "waiting" {
			continue
		}
		// A record that changed after the snapshot may have BECOME waiting since,
		// so the document is allowed to disagree about it.
		if !epoch.IsZero() && r.ModTime.After(epoch) {
			raced++
			continue
		}
		waiting[pid] = r
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

	pids := sortedKeys(waiting)
	var bad []string
	sawInput, sawPermission, sawNovel := false, false, false
	for _, pid := range pids {
		r := waiting[pid]
		s, ok := svc[pid]
		want, known := waitingVocabulary[r.WaitingFor]
		got := "<absent>"
		if ok && s.WaitingOn != nil {
			got = *s.WaitingOn
		} else if ok {
			got = "null"
		}
		fmt.Printf("  pid %d: registry %q -> service %s (status %q)\n", pid, r.WaitingFor, got, s.Status)
		switch {
		case !ok:
			bad = append(bad, fmt.Sprintf("pid %d is waiting but absent from the document", pid))
		case known && got != want:
			bad = append(bad, fmt.Sprintf("pid %d: %q must publish %q, got %s", pid, r.WaitingFor, want, got))
		case !known && got != "null":
			bad = append(bad, fmt.Sprintf("pid %d: unrecognised %q must publish null, got %s", pid, r.WaitingFor, got))
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
func ac3(v *verdicts, probe map[int]record, svc map[int]svcSession, epoch time.Time) {
	fmt.Println("AC3 — statusSince verbatim from statusUpdatedAt")
	compared, secondsShaped := 0, 0
	var mismatch, raced []string
	var oldest int64
	for _, pid := range sortedKeys(probe) {
		r := probe[pid]
		s, ok := svc[pid]
		if !ok {
			continue
		}
		if r.StatusUpdatedAt == nil || s.StatusSince == nil {
			if (r.StatusUpdatedAt == nil) != (s.StatusSince == nil) {
				mismatch = append(mismatch, fmt.Sprintf("pid %d: one side is null and the other is not", pid))
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
			mismatch = append(mismatch, fmt.Sprintf("pid %d: statusSince %q does not parse: %v", pid, *s.StatusSince, err))
			continue
		}
		gotMS := got.UnixMilli()
		if gotMS != *r.StatusUpdatedAt {
			note := fmt.Sprintf("pid %d: registry %d, document %d, drift %d ms",
				pid, *r.StatusUpdatedAt, gotMS, gotMS-*r.StatusUpdatedAt)
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
func ac4(v *verdicts, probe map[int]record, svc map[int]svcSession, epoch time.Time) {
	fmt.Println("AC4 — subagent counts")
	compared, nonZero, raced := 0, 0, 0
	var mismatch, discarded []string
	for _, pid := range sortedKeys(probe) {
		r := probe[pid]
		s, ok := svc[pid]
		if !ok || r.Subagents == nil {
			continue
		}
		if s.SubagentCount == nil {
			// Undetermined is a legitimate answer only when the probe could not
			// look either. The probe DID look, so it is either the known
			// shared-identity limitation or a defect.
			note := fmt.Sprintf("pid %d: probe counted %d, document says undetermined", pid, *r.Subagents)
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
				fmt.Printf("    RACED pid %d: probe %d, document %d — the session wrote during the run\n",
					pid, *r.Subagents, *s.SubagentCount)
			} else {
				mismatch = append(mismatch, fmt.Sprintf("pid %d: probe %d, document %d", pid, *r.Subagents, *s.SubagentCount))
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
func ac5(v *verdicts, probe map[int]record, svc map[int]svcSession, window int64, epoch time.Time) {
	fmt.Println("AC5 — API errors within the examined window, window disclosed")
	compared, truncated, hidden, inWindow, raced := 0, 0, 0, 0, 0
	var mismatch, undisclosed, discarded []string
	for _, pid := range sortedKeys(probe) {
		r := probe[pid]
		s, ok := svc[pid]
		if !ok || r.ErrorsInTail == nil {
			continue
		}
		if s.Fault == nil || s.Fault.APIErrors == nil {
			note := fmt.Sprintf("pid %d: probe read the transcript, document says undetermined", pid)
			if r.SharedIdentity {
				discarded = append(discarded, note+" (its session id is claimed by another live record)")
			} else {
				mismatch = append(mismatch, note)
			}
			continue
		}
		compared++
		if *s.Fault.APIErrors != *r.ErrorsInTail {
			note := fmt.Sprintf("pid %d: probe tail %d, document %d (whole file %d, %d bytes)",
				pid, *r.ErrorsInTail, *s.Fault.APIErrors, *r.ErrorsWholeFile, r.TranscriptBytes)
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
				undisclosed = append(undisclosed, fmt.Sprintf("pid %d: %d-byte transcript read as a %d-byte window, no disclosure",
					pid, r.TranscriptBytes, window))
			}
			if *r.ErrorsWholeFile > *r.ErrorsInTail {
				hidden++
				fmt.Printf("  pid %d: %d error(s) in the whole file, %d inside the window — %d outside it\n",
					pid, *r.ErrorsWholeFile, *r.ErrorsInTail, *r.ErrorsWholeFile-*r.ErrorsInTail)
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

func sortedKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
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
