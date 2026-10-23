package verdict

import (
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

// TestADialogOpenSessionIsWaitingOnADialog is the decision magmux's fix round
// 2 test was written to be changed by. Claude Code 2.1.276 writes a third
// waitingFor value, "dialog open"; magmux left it unrecognised and said this
// test would move when recognising it was decided. FR4 decided it: it is
// model.WaitingDialog, with the raw word kept, and attention still fires
// because the session IS waiting.
func TestADialogOpenSessionIsWaitingOnADialog(t *testing.T) {
	rec := `{"` + claude.KeySessionID + `":"s","` + claude.KeyPID + `":26188,"` + claude.KeyStatus + `":"` +
		claude.ValueStatusWaiting + `","` + claude.KeyWaitingFor + `":"` + claude.ValueWaitingDialog + `"}`
	r, ps, err := claude.DecodeRecord("26188.json", []byte(rec))
	if err != nil || len(ps) != 0 {
		t.Fatalf("%v %v", err, ps)
	}
	eff := claude.Effective(claude.EffectiveInput{Registry: &r})
	s := model.Session{Status: eff.Status, StatusRaw: eff.Raw, Waiting: eff.Waiting, WaitingRaw: eff.WaitingRaw}
	if s.Waiting != model.WaitingDialog || s.WaitingRaw != claude.ValueWaitingDialog {
		t.Fatalf("%v %q", s.Waiting, s.WaitingRaw)
	}
	got := Resolve(s, Screen{}, NOW, Rules{})
	if got.Attention.Needed == nil || !*got.Attention.Needed || !strings.Contains(got.Attention.Reason, "dialog") {
		t.Fatalf("attention %s: %q", needed(got.Attention), got.Attention.Reason)
	}
}
