package doctor

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

func (d *run) jobsOrSkip() ([]jobEntry, *Result) {
	jobs, err := d.jobsRead()
	if err != nil || len(jobs) == 0 {
		r := skip(fmt.Sprintf("no job records were read (%v)", err))
		return nil, &r
	}
	return jobs, nil
}

// jobs-keys compares against the MEASURED universal union, not the decoder's
// set, so the never-read keys do not make every real record a warning.
func (d *run) jobsKeys() Result {
	jobs, s := d.jobsOrSkip()
	if s != nil {
		return *s
	}
	v := claude.Vocabulary().Jobs
	seen := map[string]int{}
	for _, j := range jobs {
		for k := range j.keys {
			seen[k]++
		}
	}
	var missing, added []string
	for _, k := range v.Universal {
		if seen[k] != len(jobs) {
			missing = append(missing, k)
		}
	}
	known := map[string]bool{}
	for _, k := range append(append([]string{}, v.Universal...), v.Optional...) {
		known[k] = true
	}
	for k, n := range seen {
		if n == len(jobs) && !known[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	ev := []string{fmt.Sprintf("%d depth-1 records; %d measured universal keys", len(jobs), len(v.Universal))}
	switch {
	case len(missing) > 0:
		return result(VerdictFail, "universal keys are not on every record: "+strings.Join(missing, ", "), ev...)
	case len(added) > 0:
		return result(VerdictWarn, "new universal keys this build does not read: "+strings.Join(added, ", "), ev...)
	}
	return result(VerdictOK, fmt.Sprintf("all %d present on every record", len(v.Universal)), ev...)
}

// jobs-state-values is a FAIL on an unmapped state: the terminal gate is built
// on this vocabulary.
func (d *run) jobsStateValues() Result {
	jobs, s := d.jobsOrSkip()
	if s != nil {
		return *s
	}
	observed := map[string]int{}
	for _, j := range jobs {
		if j.keys[claude.KeyState] {
			observed[j.job.StateRaw]++
		}
	}
	ev := []string{"observed: " + counts(observed)}
	if u := unknownValues(observed, claude.Vocabulary().JobState); len(u) > 0 {
		return result(VerdictFail, "unmapped job states: "+strings.Join(u, ", "), ev...)
	}
	return result(VerdictOK, "every value maps", ev...)
}

// jobs-dir-mtime-tracks-record: a record published by REPLACEMENT moves its
// directory's mtime, which is why job directories are watched as directories.
func (d *run) jobsDirMtimeTracksRecord() Result {
	jobs, s := d.jobsOrSkip()
	if s != nil {
		return *s
	}
	compared, tracking := 0, 0
	for _, j := range jobs {
		if j.dirMod.IsZero() || j.mod.IsZero() {
			continue
		}
		compared++
		if delta := j.dirMod.Sub(j.mod); delta >= 0 && delta < 5*time.Minute {
			tracking++
		}
	}
	ev := []string{fmt.Sprintf("%d directories compared; %d at or just after their record", compared, tracking)}
	switch {
	case compared == 0:
		return skip("no job directory could be statted", ev...)
	case tracking == 0:
		return result(VerdictWarn, "no directory tracks its record, which suggests in-place writes; the directory watch no longer covers them", ev...)
	}
	return result(VerdictOK, fmt.Sprintf("%d of %d track, so replacement-publish holds", tracking, compared), ev...)
}

// lastTimelineAt is the at of the last decodable line of a job's timeline.
func (d *run) timeline(dir string) ([]claude.TimelineLine, bool) {
	lines, err := tailLines(d.env.Root.Jobs.FS, path.Join(dir, claude.JobTimelineFile), claude.TimingTailBytes)
	if err != nil || len(lines) == 0 {
		return nil, false
	}
	var out []claude.TimelineLine
	for _, l := range lines {
		if tl, ok := claude.DecodeTimelineLine(l); ok {
			out = append(out, tl)
		}
	}
	return out, true
}

// jobs-updatedat-not-earlier-than-timeline: the error has ONE direction. Late
// under-reports a blocked duration; early over-reports it, which fires an
// attention notification immediately and permanently.
func (d *run) jobsUpdatedAtNotEarlier() Result {
	jobs, s := d.jobsOrSkip()
	if s != nil {
		return *s
	}
	compared, later, equal, earlier := 0, 0, 0, 0
	var worst time.Duration
	for _, j := range jobs {
		if j.job.UpdatedAt == nil {
			continue
		}
		lines, ok := d.timeline(j.dir)
		if !ok {
			continue
		}
		var last *time.Time
		for i := len(lines) - 1; i >= 0; i-- {
			if lines[i].At != nil {
				last = lines[i].At
				break
			}
		}
		if last == nil {
			continue
		}
		compared++
		switch delta := j.job.UpdatedAt.Sub(*last); {
		case delta > time.Millisecond:
			later++
		case delta < -time.Millisecond:
			earlier++
			if -delta > worst {
				worst = -delta
			}
		default:
			equal++
		}
	}
	ev := []string{fmt.Sprintf("%d compared: %d equal, %d later, %d EARLIER", compared, equal, later, earlier)}
	switch {
	case compared == 0:
		return skip("no record has both an updatedAt and a readable timeline", ev...)
	case earlier > 0:
		return result(VerdictFail, fmt.Sprintf("%d records are earlier than their timeline by up to %s; statusSince over-reports",
			earlier, worst.Round(time.Second)), ev...)
	}
	return result(VerdictOK, "the error direction is intact: late, never early", ev...)
}

// jobs-timeline-shape: the witness the check above depends on.
func (d *run) jobsTimelineShape() Result {
	jobs, s := d.jobsOrSkip()
	if s != nil {
		return *s
	}
	seen := map[string]int{}
	entries, files := 0, 0
	for _, j := range jobs {
		lines, ok := d.timeline(j.dir)
		if !ok {
			continue
		}
		files++
		for _, l := range lines {
			entries++
			for _, k := range l.Keys {
				seen[k]++
			}
		}
	}
	if files == 0 {
		return skip("no timeline could be read")
	}
	var missing []string
	for _, k := range claude.Vocabulary().Timeline.Universal {
		if seen[k] == 0 {
			missing = append(missing, k)
		}
	}
	ev := []string{fmt.Sprintf("%d entries over %d timelines (tail-bounded); keys %s", entries, files, counts(seen))}
	if len(missing) > 0 {
		return result(VerdictWarn, "the timeline lost keys: "+strings.Join(missing, ", ")+
			"; nothing at runtime reads it, but the updatedAt-direction check does", ev...)
	}
	return result(VerdictOK, "all four present", ev...)
}

// jobs-linkscanpath: a declared transcript path, under tt's lexical
// containment rule, must exist and be a depth-2 session transcript.
func (d *run) jobsLinkScanPath() Result {
	jobs, s := d.jobsOrSkip()
	if s != nil {
		return *s
	}
	projects := d.env.Root.Projects
	named, exists, depth2, outside := 0, 0, 0, 0
	var missing []string
	for _, j := range jobs {
		p := j.job.LinkScanPath
		if p == "" {
			continue
		}
		named++
		rel, ok := claude.ProjectsRel(projects.Path, p)
		if !ok {
			outside++
			continue
		}
		if strings.Count(rel, "/") == 1 {
			depth2++
		}
		if projects.FS != nil {
			if info, err := fs.Stat(projects.FS, rel); err == nil && info.Mode().IsRegular() {
				exists++
				continue
			}
		}
		if len(missing) < 3 {
			missing = append(missing, j.dir)
		}
	}
	ev := []string{fmt.Sprintf("%d of %d records name a transcript; %d exist; %d are depth-2; %d are outside %s",
		named, len(jobs), exists, depth2, outside, projects.Path)}
	if len(missing) > 0 {
		ev = append(ev, "missing targets (expected past the retention floor): "+strings.Join(missing, ", "))
	}
	switch {
	case named == 0:
		return result(VerdictFail, "no record names a transcript; background timing has no source", ev...)
	case named < len(jobs):
		return result(VerdictWarn, fmt.Sprintf("%d records carry no linkScanPath and get no transcript timing", len(jobs)-named), ev...)
	case outside > 0 || depth2 < named:
		return result(VerdictWarn, fmt.Sprintf("%d paths are not depth-2 inside the projects root and are refused", named-depth2), ev...)
	}
	return result(VerdictOK, "every record names a depth-2 transcript path", ev...)
}

// jobs-signal-keys: tempo is universal and its values are known; needs and
// suggestedReply are recorded beside state and tempo every run.
func (d *run) jobsSignalKeys() Result {
	jobs, s := d.jobsOrSkip()
	if s != nil {
		return *s
	}
	seen := map[string]int{}
	tempo := map[string]int{}
	var pairs []string
	for _, j := range jobs {
		for k := range j.keys {
			seen[k]++
		}
		if j.job.Tempo != nil {
			tempo[*j.job.Tempo]++
		} else if j.keys[claude.KeyTempo] {
			tempo[""]++
		}
		needs, reply := j.keys[claude.KeyNeeds], j.keys[claude.KeySuggestedReply]
		if needs || reply {
			tempoRaw := ""
			if j.job.Tempo != nil {
				tempoRaw = *j.job.Tempo
			}
			pairs = append(pairs, fmt.Sprintf("%s state=%s tempo=%s needs=%t suggestedReply=%t",
				j.dir, j.job.StateRaw, tempoRaw, needs, reply))
		}
	}
	ev := []string{fmt.Sprintf("%s on %d/%d, %s on %d/%d, %s on %d/%d, %s on %d/%d",
		claude.KeyReapedMidWorkAt, seen[claude.KeyReapedMidWorkAt], len(jobs), claude.KeyTempo, seen[claude.KeyTempo], len(jobs),
		claude.KeyNeeds, seen[claude.KeyNeeds], len(jobs), claude.KeySuggestedReply, seen[claude.KeySuggestedReply], len(jobs)),
		"tempo values: " + counts(tempo)}
	ev = append(ev, pairs...)
	delete(tempo, "")
	switch {
	case seen[claude.KeyTempo] < len(jobs):
		return result(VerdictWarn, "tempo is not universal, so the mid-turn blocked signal is unavailable on some records", ev...)
	case len(unknownValues(tempo, claude.Vocabulary().Tempo)) > 0:
		return result(VerdictWarn, "unrecognised tempo values: "+strings.Join(unknownValues(tempo, claude.Vocabulary().Tempo), ", "), ev...)
	}
	return result(VerdictOK, "tempo is universal and its values are known", ev...)
}
