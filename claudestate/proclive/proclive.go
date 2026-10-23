// Package proclive is the production process oracle: kill(pid, 0), which sends
// nothing and only asks whether the process exists (A6, FR17).
//
//	ESRCH        -> No: there is no such process. Positive evidence of death.
//	nil, EPERM   -> Yes: it exists (EPERM: it belongs to another user, and
//	                existence is what was asked).
//	anything else -> Unknown: the gate could not answer, which is not death.
//	pid <= 0      -> Unknown, with no syscall: kill(2) reads 0 as the caller's
//	                process group and a negative pid as a group, so the answer
//	                would be about the wrong subject.
//
// Only a host's composition root may construct it. Every test injects a fake
// ports.ProcessOracle instead (guard G8): a pid in testdata could collide with
// a real process, and the answer would depend on what else is running.
package proclive

import (
	"errors"
	"syscall"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

// kill is the syscall, as a variable so a test maps errnos without hunting for
// a pid that is reliably dead.
var kill = syscall.Kill

type oracle struct{}

// New returns the production oracle.
func New() ports.ProcessOracle { return oracle{} }

func (oracle) Alive(pid int) model.Liveness {
	if pid <= 0 {
		return model.LivenessUnknown
	}
	switch err := kill(pid, 0); {
	case err == nil, errors.Is(err, syscall.EPERM):
		return model.LivenessYes
	case errors.Is(err, syscall.ESRCH):
		return model.LivenessNo
	default:
		return model.LivenessUnknown
	}
}
