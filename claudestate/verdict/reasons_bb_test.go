package verdict_test

import (
	"testing"

	"github.com/MadAppGang/magmux/claudestate/verdict"
)

func TestBBReasonPredicatesDoNotAcceptUnknownHostProse(t *testing.T) {
	// The contract does not enumerate the ladder's reason words. Only their
	// closed-set rejection is asserted here; positive cases need those words.
	for _, reason := range []string{"", "synthetic-unrecognised-reason", "\x1b]52;c;testdata\x07"} {
		if verdict.IsUndetermined(reason) {
			t.Errorf("unknown reason %q treated as a ladder undetermined word", reason)
		}
		if verdict.IsLivenessDismissal(reason) {
			t.Errorf("unknown reason %q treated as liveness dismissal", reason)
		}
	}
}
