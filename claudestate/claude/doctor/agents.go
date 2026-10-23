package doctor

import (
	"fmt"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// agents-cli-shape: the listing still answers, with both populations. The
// host takes the listing (spawning is the host's); with none given this
// SKIPS. A listing that will not decode is a WARN, a degradation and not a
// defect: every liveness gate works without it.
func (d *run) agentsCLIShape() Result {
	if d.env.Agents == nil {
		return skip("no agents listing was given")
	}
	l, _, err := claude.DecodeAgents(d.env.Agents.Raw, d.env.Agents.At)
	if err != nil {
		return result(VerdictWarn, "the listing does not decode: "+err.Error()+"; corroboration contributes nothing")
	}
	interactive, background, waiting := 0, 0, 0
	for _, e := range l.Entries {
		if claude.IsInteractiveEntry(e) {
			interactive++
			if e.WaitingRaw != "" {
				waiting++
			}
			continue
		}
		background++
	}
	ev := []string{fmt.Sprintf("%d entries: %d interactive, %d background; %d carry waitingFor; cached=%t",
		len(l.Entries), interactive, background, waiting, d.env.Agents.Cached)}
	if len(l.Entries) == 0 {
		return result(VerdictWarn, "the listing is empty; corroboration contributes nothing this run", ev...)
	}
	return result(VerdictOK, "both populations present", ev...)
}
