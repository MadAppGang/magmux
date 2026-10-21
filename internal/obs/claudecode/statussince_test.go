package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
)

// TestStatusSinceIsNeverCorrected is acceptance criterion 3's regression
// guard, under the name validation-criteria.md gives it. Fix round 1, item 7:
// the criterion required it to exist and it did not.
//
// # The refuted defect it guards against
//
// The original AC3 asked magmux to "correct a session whose statusUpdatedAt is
// days stale while its pid lives". Phase 2 established that statusUpdatedAt
// means "in this status SINCE", not "last refreshed": the registry rewrites it
// only on a real transition, so an old value on a live session is a session
// that has been in one status for a long time — a FACT, and the one R6's
// `delay` and the attention view's ordering both consume.
//
// The counter-example the criterion was written about is session 98198: its
// registry instant is 1.37 hours OLDER than its own transcript's latest write.
// That write is an artifact-autoreact-ledger record — bookkeeping with no
// conversational turn behind it — so the transcript moved and the session did
// not. A plausible-looking "fix" reads the transcript's newer instant and
// overwrites statusSince with it, and would destroy a correct value. This test
// fails if anybody does, wherever in the pipeline they do it.
//
// # How it is staged
//
// The REAL record and the REAL transcript from testdata, copied into a
// temporary directory so the transcript's modification time can be set to the
// instant it was really last written — the ledger's own savedAt, 1.37 h after
// the status instant. The copy is necessary because a checkout sets every
// file's mtime to whenever it happened, which would make the bait arbitrary.
//
// Run twice: once with the record exactly as captured, and once with its
// statusUpdatedAt restored to the MILLISECOND value measured on the live file
// (testdata/PROVENANCE.md: 1789536692864). The corpus was truncated to whole
// seconds when it was built, so only the second run can tell "verbatim to the
// millisecond" from "rounded to the second".
//
// Nothing here reads ~/.claude or probes a pid: the registry and transcript
// are copies of testdata, and liveness comes from a table.
func TestStatusSinceIsNeverCorrected(t *testing.T) {
	const (
		pid       = zeroPID // 98198
		sessionID = zeroSession
		projDir   = "-Users-user-dev-magmux"
		// The ledger record's own savedAt — when the transcript was last
		// written, 1.37 h after the session entered its status.
		ledgerSavedAt = int64(1789541609884)
		// The live record's statusUpdatedAt, to the millisecond.
		liveStatusMS = int64(1789536692864)
	)
	now := time.Unix(1789545200, 0).UTC() // PROVENANCE.md rule 4's NOW

	record, err := os.ReadFile(filepath.Join(testdataSessions, "98198.json"))
	if err != nil {
		t.Fatalf("reading the 98198 record: %v", err)
	}
	transcript, err := os.ReadFile(filepath.Join(testdataProjects, projDir,
		sessionID+adapters.TranscriptSuffix))
	if err != nil {
		t.Fatalf("reading the 98198 transcript: %v", err)
	}

	for _, tc := range []struct {
		name     string
		statusMS int64 // 0 keeps the record as captured
	}{
		{"the record as captured", 0},
		{"the live millisecond value", liveStatusMS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinTheDirNameOverride(t)
			body := record
			if tc.statusMS != 0 {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(record, &fields); err != nil {
					t.Fatal(err)
				}
				ms, _ := json.Marshal(tc.statusMS)
				fields[adapters.KeyStatusUpdatedAt] = ms
				fields[adapters.KeyUpdatedAt] = ms
				if body, err = json.Marshal(fields); err != nil {
					t.Fatal(err)
				}
			}

			// The raw instant, read back out of the bytes the pipeline will
			// read, by nothing but encoding/json.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			var rawMS int64
			if err := json.Unmarshal(raw[adapters.KeyStatusUpdatedAt], &rawMS); err != nil {
				t.Fatalf("the staged record's status instant: %v", err)
			}
			want := time.UnixMilli(rawMS).UTC()

			sessions := filepath.Join(t.TempDir(), "sessions")
			projects := filepath.Join(t.TempDir(), "projects")
			if err := os.MkdirAll(sessions, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(projects, projDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sessions, "98198.json"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			tpath := filepath.Join(projects, projDir, sessionID+adapters.TranscriptSuffix)
			if err := os.WriteFile(tpath, transcript, 0o644); err != nil {
				t.Fatal(err)
			}
			written := time.UnixMilli(ledgerSavedAt)
			if err := os.Chtimes(tpath, written, written); err != nil {
				t.Fatal(err)
			}

			// The bait is armed: the transcript really is newer, by the
			// measured margin. Without this the test could pass because there
			// was nothing to be tempted by.
			st, err := os.Stat(tpath)
			if err != nil {
				t.Fatal(err)
			}
			if lead := st.ModTime().Sub(want); lead < 80*time.Minute {
				t.Fatalf("the transcript is only %v newer than the status instant; the case needs "+
					"the measured 1.37 h", lead)
			}

			p, err := pipeline.Start(pipeline.Config{
				Root:   sessions,
				Oracle: fileOracle{pid: obs.LivenessYes},
				Now:    func() time.Time { return now },
				Chain:  []pipeline.Factory{Middleware(Config{Root: projects})},
			})
			if err != nil {
				t.Fatalf("starting the pipeline: %v", err)
			}
			defer p.Stop()
			sub, err := p.Subscribe("statussince", 1)
			if err != nil {
				t.Fatal(err)
			}
			p.Refresh()
			var doc obs.Fleet
			select {
			case d := <-sub.C():
				doc = d.Doc
			case <-time.After(10 * time.Second):
				t.Fatal("no document arrived")
			}

			var got obs.Session
			for _, s := range doc.Sessions {
				if s.ID == sessionID {
					got = s
				}
			}
			if got.PID != pid {
				t.Fatalf("session %s is not in the document", sessionID)
			}
			// The transcript was actually read — otherwise "statusSince was not
			// corrected from it" would be true for want of a transcript.
			if got.Color == nil || *got.Color != "cyan" {
				t.Fatalf("the transcript was not read (colour %v); the test would prove nothing",
					colourOf(got.Color))
			}

			if got.StatusSince == nil {
				t.Fatal("statusSince is undetermined; the registry states it")
			}
			if !got.StatusSince.Equal(want) || got.StatusSince.UnixMilli() != rawMS {
				t.Fatalf("statusSince = %s (%d ms), want the registry's statusUpdatedAt verbatim: "+
					"%s (%d ms). The transcript was last written at %s — if statusSince is that, "+
					"something \"corrected\" a correct value from transcript activity, which is the "+
					"refuted AC3 defect", got.StatusSince.UTC().Format(time.RFC3339Nano),
					got.StatusSince.UnixMilli(), want.Format(time.RFC3339Nano), rawMS,
					written.UTC().Format(time.RFC3339Nano))
			}

			// And on the wire, where a consumer reads it.
			wire, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var onWire struct {
				StatusSince time.Time `json:"statusSince"`
			}
			if err := json.Unmarshal(wire, &onWire); err != nil {
				t.Fatal(err)
			}
			if onWire.StatusSince.UnixMilli() != rawMS {
				t.Errorf("the published statusSince is %d ms, want %d", onWire.StatusSince.UnixMilli(), rawMS)
			}

			// inStatusFor is derived from it, as a fact and not flagged.
			if d, ok := got.InStatusFor(doc.Epoch); !ok || d != now.Sub(want) {
				t.Errorf("inStatusFor = %v, %v; want %v from the registry's instant", d, ok, now.Sub(want))
			}
			t.Logf("statusSince %s = statusUpdatedAt %d ms; transcript last written %s (%.2f h later)",
				got.StatusSince.UTC().Format(time.RFC3339Nano), rawMS,
				written.UTC().Format(time.RFC3339Nano), written.Sub(want).Hours())
		})
	}
}
