package obsview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// The fleet table and its JSON form, shared by every surface that shows the
// document: `magmux agents` prints them, the socket `agents` verb and the MCP
// tool `list_agents` return the same bytes. Moved here from mux's obscmd.go
// unchanged, because `mcp` cannot import `mux`.

// WriteFleetJSON emits the document.
//
// Indented, and one whole document per change when streaming. A stream of
// concatenated JSON values is what `json.Decoder` reads natively, so a
// consumer needs no framing, and a human reading it over the shoulder of a
// running session can see what changed.
//
// Every string is carried EXACT — this is the document, not a rendering of
// it — and TerminalSafeJSON escapes the DEL and C1 characters encoding/json
// would otherwise write raw, which changes no decoded value.
func WriteFleetJSON(out io.Writer, fleet obs.Fleet) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fleet); err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents: encoding the document: %v\n", err)
		return
	}
	if _, err := out.Write(TerminalSafeJSON(buf.Bytes())); err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents: writing the document: %v\n", err)
	}
}

// WriteFleetTable renders the document for a person.
//
// Every undetermined value prints as "-", and that is the whole point of the
// column being there: "we did not determine this" and "no" are different
// answers, and a table that printed an empty cell for both would throw the
// distinction away at the last possible moment.
func WriteFleetTable(out io.Writer, fleet obs.Fleet) {
	if len(fleet.Sessions) == 0 {
		fmt.Fprintf(out, "no sessions at %s\n", fleet.Epoch.Format(time.RFC3339))
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "PID\tPANE\tSTATUS\tTIER\tWAITING ON\tPROCESS\tPANE ALIVE\tGATE\tSUB\tERR\tIN STATUS FOR\tATTENTION\tNAME\n")
	for _, s := range fleet.Sessions {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.PID,
			OrDash(paneIDText(s)),
			OrDash(StatusText(s)),
			OrDash(tierText(s.Confidence)),
			OrDash(WaitingText(s)),
			OrDash(livenessText(s.ProcessAlive)),
			OrDash(livenessText(s.PaneAlive)),
			OrDash(gateText(s)),
			OrDash(countText(s.SubagentCount)),
			OrDash(countText(s.Fault.APIErrors)),
			OrDash(inStatusForText(s, fleet.Epoch)),
			// NOT OrDash: this column has its own spelling for undetermined,
			// and it is the one column where a "-" would be read as a "no".
			attentionText(s),
			// Through the one display rule: a name is text from a file magmux
			// does not write, and it can be generated from conversation
			// content. See display.go.
			OrDash(DisplaySafe(s.Name)),
		)
	}
	tw.Flush()
	writeAttentionWhy(out, fleet)
	plural := "sessions"
	if len(fleet.Sessions) == 1 {
		plural = "session"
	}
	fmt.Fprintf(out, "%d %s at %s\n", len(fleet.Sessions), plural, fleet.Epoch.Format(time.RFC3339))
}

// attentionText renders the three-state verdict as THREE legible states.
//
// "YES", "no" and "?", and not a tick against a blank. A blank cell in a yes/no
// column is read as a no by everyone who has ever read a table, so a session
// nobody could reach a verdict about would be silently filed under "fine" —
// which is the exact failure the *bool behind this column exists to prevent,
// reintroduced at the last possible moment.
//
// It is also the one column that does not print "-" for its undetermined
// value, deliberately, and the help text says so. Everywhere else "-" means
// "magmux did not determine this"; here the same dash would collide with the
// determined negative sitting right beside it in the same column.
//
// The case is doing work too: YES is the answer a reader is scanning for, and
// "no" in lower case lets it recede. There is nothing else in the table in
// capitals.
func attentionText(s obs.Session) string {
	switch {
	case s.Attention.Needed == nil:
		return "?"
	case *s.Attention.Needed:
		return "YES"
	default:
		return "no"
	}
}

// tierText names which tier the published status came from.
//
// The wire word, unabbreviated: "authoritative" is a wide column and a second,
// shorter spelling invented for the table would be one more thing a reader has
// to map back to the JSON. An undetermined tier prints empty, so OrDash turns
// it into the same "-" as every other undetermined column — that one IS a
// plain absence of an answer and reads correctly as one.
func tierText(t obs.Tier) string {
	if t == obs.TierUnknown {
		return ""
	}
	return t.String()
}

// writeAttentionWhy prints the reason behind every verdict worth explaining,
// under the table and at full width.
//
// # Why a block and not a column
//
// The reason is a sentence. A sentence in a thirteenth column wraps, and a
// wrapped row in a tabwriter table destroys the alignment of every row after
// it — so the column that matters most would be the one that makes the table
// unreadable. Under the table it has the whole width.
//
// # What appears here
//
// Everything a person would have to act on: a YES, an undetermined "?", and
// any session whose two tiers disagreed. A determined "no" is left out, because
// its reason is already legible from the row — "it is working", "nothing
// outstanding" — and thirty of them would bury the two lines that matter.
// `--json` carries every reason regardless; this is the triage view.
func writeAttentionWhy(out io.Writer, fleet obs.Fleet) {
	var shown []obs.Session
	for _, s := range fleet.Sessions {
		if s.Conflict != nil || s.Attention.Needed == nil || *s.Attention.Needed {
			shown = append(shown, s)
		}
	}
	if len(shown) == 0 {
		return
	}

	fmt.Fprintf(out, "\nwhy:\n")
	for _, s := range shown {
		// The reason is magmux's sentence, but it can END in the transcript's
		// own words — an API error's text is appended verbatim — so it goes
		// through the display rule like the name does.
		fmt.Fprintf(out, "  %-7d %-3s %s\n", s.PID, attentionText(s), DisplaySafe(s.Attention.Reason))
		if s.Conflict != nil {
			// Said on its own line even though the reason usually mentions it,
			// because a conflict can ride along with a verdict that was
			// decided by something else entirely — an API error, or a gate —
			// and then this is the only place it appears at all.
			fmt.Fprintf(out, "  %-7s %-3s conflict: the %s tier said %s — %s\n",
				"", "", s.Conflict.Tier, s.Conflict.Status, DisplaySafe(s.Conflict.Detail))
		}
	}
}

// StatusText prefers magmux's own word and falls back to the raw string the
// tool wrote, in angle brackets so that a value magmux does not recognise is
// visibly not one of its own. Showing the raw value is the difference between
// a new Claude release being discoverable and being invisible.
//
// The raw value goes through DisplaySafe HERE, once, because this is where it
// becomes display text: both the table and the attention panel render through
// this function, so neither can forget it. The document itself keeps the raw
// value exact.
func StatusText(s obs.Session) string {
	if s.Status != obs.StatusUnknown {
		return s.Status.String()
	}
	if s.StatusRaw != "" {
		return "<" + DisplaySafe(s.StatusRaw) + ">"
	}
	return ""
}

// WaitingText is StatusText for the waiting reason, on the same terms.
func WaitingText(s obs.Session) string {
	if s.Waiting != obs.WaitingReasonUnknown {
		return s.Waiting.String()
	}
	if s.WaitingRaw != "" {
		return "<" + DisplaySafe(s.WaitingRaw) + ">"
	}
	return ""
}

// paneIDText is the magmux pane the session is running in.
//
// Empty — and so "-" — for a session that is not one of magmux's panes, which
// is NOT hypothetical and is not an error: discovery sees sessions magmux did
// not start, and this command, running outside any magmux, sees nothing else.
// They are shown rather than filtered, because whether to show them is the
// reader's decision and dropping them here would remove the choice.
func paneIDText(s obs.Session) string {
	if s.PaneID == nil {
		return ""
	}
	return strconv.Itoa(*s.PaneID)
}

// gateText names the gate that REJECTED this session, or "" when none did.
//
// The two gates are separate columns and this is a third, rather than one
// column collapsing them, because a dead process and a vanished pane are
// different situations with different remedies — and the collapse would happen
// at the wire, where it is least recoverable. This column is the summary; the
// two beside it are the evidence.
//
// An UNDETERMINED gate never rejects. "Nobody could tell" is not "no", and a
// session excluded because nobody looked is the failure the whole tri-state
// exists to prevent.
func gateText(s obs.Session) string {
	switch {
	case s.ProcessAlive == obs.LivenessNo && s.PaneAlive == obs.LivenessNo:
		return "process+pane"
	case s.ProcessAlive == obs.LivenessNo:
		return "process"
	case s.PaneAlive == obs.LivenessNo:
		return "pane"
	}
	return ""
}

// livenessText renders the undetermined answer as EMPTY, so that OrDash turns
// it into the same "-" every other undetermined column uses. Liveness.String
// spells it "unknown", which is right for a log line and wrong in a column
// beside "yes" and "no": it reads as a third determined state rather than as
// the absence of an answer.
func livenessText(l obs.Liveness) string {
	if l == obs.LivenessUnknown {
		return ""
	}
	return l.String()
}

func inStatusForText(s obs.Session, epoch time.Time) string {
	d, ok := s.InStatusFor(epoch)
	if !ok {
		return ""
	}
	return FormatDuration(d)
}

// countText renders a determined count, and NOTHING for an undetermined one, so
// that OrDash turns it into the same "-" every other undetermined column uses.
//
// This is the column where the distinction earns its keep: 0 means the session
// spawned no subagents, or hit no API errors, and "-" means nobody could look —
// most often because no transcript has been found for it. Printing 0 for both
// tells a user a session is healthy when the truth is that it is unexamined.
func countText(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

// OrDash prints an undetermined value — the empty string every *Text helper
// returns for one — as "-", the table's one spelling of "not determined".
func OrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// FormatDuration renders a time.Duration compactly (e.g. "1.2s", "4m 12s",
// "1h 3m").
//
// A COPY of mux's formatDuration, which the terminal core uses for its own
// chrome and which must not depend on this package. The two must agree — the
// panel's attention view and this table print the same IN STATUS FOR — and
// mux's TestFormatDurationAgreesWithObsview holds them to it.
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}
