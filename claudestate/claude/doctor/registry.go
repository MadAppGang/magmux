package doctor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

func (d *run) registryOrSkip() ([]regEntry, *Result) {
	regs, err := d.registry()
	if err != nil || len(regs) == 0 {
		r := skip(fmt.Sprintf("no registry records were read (%v)", err))
		return nil, &r
	}
	return regs, nil
}

// registry-keys: the 19 universal keys are on every record. A key gone is a
// fail; a key the format grew is a warn: nothing breaks, but the record now
// says more than this build reads.
func (d *run) registryKeys() Result {
	regs, s := d.registryOrSkip()
	if s != nil {
		return *s
	}
	v := claude.Vocabulary().Registry
	seen := map[string]int{}
	for _, r := range regs {
		for k := range r.keys {
			seen[k]++
		}
	}
	var missing, added []string
	for _, k := range v.Universal {
		if seen[k] != len(regs) {
			missing = append(missing, k)
		}
	}
	known := map[string]bool{}
	for _, k := range append(append([]string{}, v.Universal...), v.Optional...) {
		known[k] = true
	}
	for k := range seen {
		if !known[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	ev := []string{fmt.Sprintf("%d records", len(regs))}
	for _, k := range v.Optional {
		ev = append(ev, fmt.Sprintf("optional %s on %d/%d", k, seen[k], len(regs)))
	}
	switch {
	case len(missing) > 0:
		return result(VerdictFail, "keys this build reads are not on every record: "+strings.Join(missing, ", "), ev...)
	case len(added) > 0:
		return result(VerdictWarn, "the format grew keys this build does not read: "+strings.Join(added, ", "), ev...)
	}
	return result(VerdictOK, fmt.Sprintf("all %d present on every record", len(v.Universal)), ev...)
}

// registry-status-values: every status word is one this build maps.
func (d *run) registryStatusValues() Result {
	regs, s := d.registryOrSkip()
	if s != nil {
		return *s
	}
	observed := map[string]int{}
	for _, r := range regs {
		if r.keys[claude.KeyStatus] {
			observed[r.rec.StatusRaw]++
		}
	}
	ev := []string{"observed: " + counts(observed)}
	if u := unknownValues(observed, claude.Vocabulary().RegistryStatus); len(u) > 0 {
		return result(VerdictWarn, "unmapped status values decode to unknown: "+strings.Join(u, ", "), ev...)
	}
	return result(VerdictOK, "every value maps", ev...)
}

// registry-waiting-for: present on waiting records with a mapped value. With
// nothing waiting the field cannot be observed, and that is a skip.
func (d *run) registryWaitingFor() Result {
	regs, s := d.registryOrSkip()
	if s != nil {
		return *s
	}
	waiting, carrying := 0, 0
	observed := map[string]int{}
	for _, r := range regs {
		if r.keys[claude.KeyWaitingFor] {
			observed[r.rec.WaitingRaw]++
		}
		if r.rec.Status != model.StatusWaiting {
			continue
		}
		waiting++
		if r.keys[claude.KeyWaitingFor] {
			carrying++
		}
	}
	ev := []string{fmt.Sprintf("%d waiting records, %d carrying waitingFor", waiting, carrying), "values: " + counts(observed)}
	switch {
	case waiting == 0:
		return skip("no session is waiting, so the field's presence cannot be observed", ev...)
	case carrying < waiting:
		return result(VerdictWarn, fmt.Sprintf("%d of %d waiting records carry no waitingFor; their reason is unknown",
			waiting-carrying, waiting), ev...)
	}
	if u := unknownValues(observed, claude.Vocabulary().WaitingFor); len(u) > 0 {
		return result(VerdictWarn, "unmapped waitingFor values: "+strings.Join(u, ", "), ev...)
	}
	return result(VerdictOK, "present and mapped on every waiting record", ev...)
}

// registry-tmux-shape: every reference parses, sigils included.
func (d *run) registryTmuxShape() Result {
	regs, s := d.registryOrSkip()
	if s != nil {
		return *s
	}
	present, matched := 0, 0
	var bad []string
	for _, r := range regs {
		if r.rec.Tmux == nil {
			continue
		}
		present++
		if r.rec.Tmux.Session != "" {
			matched++
		} else if len(bad) < 3 {
			bad = append(bad, r.rec.Tmux.Raw)
		}
	}
	ev := []string{fmt.Sprintf("%d of %d records carry a reference; %d parse", present, len(regs), matched)}
	switch {
	case present == 0:
		return skip("no record carries a tmux reference", ev...)
	case matched < present:
		return result(VerdictFail, fmt.Sprintf("%d references do not parse, so a pane gate cannot vouch for them: %s",
			present-matched, strings.Join(bad, " ")), ev...)
	}
	return result(VerdictOK, "every reference parses", ev...)
}

// registry-mtime-tracks-status: the registry is rewritten IN PLACE, which the
// watch rests on. Reported as seen (D7): a file later than its status is
// updatedAt moving without a status change.
func (d *run) registryMtimeTracksStatus() Result {
	regs, s := d.registryOrSkip()
	if s != nil {
		return *s
	}
	const tolerance = 60 * time.Second
	compared, within := 0, 0
	var worst time.Duration
	for _, r := range regs {
		if r.rec.StatusUpdatedAt == nil || r.mod.IsZero() {
			continue
		}
		compared++
		delta := r.mod.Sub(*r.rec.StatusUpdatedAt)
		if delta < 0 {
			delta = -delta
		}
		if delta <= tolerance {
			within++
		}
		if delta > worst {
			worst = delta
		}
	}
	ev := []string{fmt.Sprintf("%d compared; %d within %s of statusUpdatedAt; largest gap %s",
		compared, within, tolerance, worst.Round(time.Second))}
	switch {
	case compared == 0:
		return skip("no record carries both a statusUpdatedAt and a readable mtime", ev...)
	case within == 0:
		return result(VerdictFail, "no file's mtime tracks its status instant; the in-place-write assumption the watch rests on is gone", ev...)
	}
	return result(VerdictOK, fmt.Sprintf("%d of %d track; the rest moved without a status change", within, compared), ev...)
}

// registry-version-spread never fails: its value is the record it leaves.
func (d *run) registryVersionSpread() Result {
	regs, s := d.registryOrSkip()
	if s != nil {
		return *s
	}
	observed := map[string]int{}
	for _, r := range regs {
		if r.keys[claude.KeyVersion] {
			observed[r.rec.Version]++
		}
	}
	return result(VerdictOK, "recorded for comparison with the next run; a widening spread is a format change in flight",
		fmt.Sprintf("%d distinct versions across %d records: %s", len(observed), len(regs), counts(observed)))
}
