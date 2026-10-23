package doctor

import (
	"bytes"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// retention is the window rule 4 compares against: Env.Retention, else
// settings.json's cleanupPeriodDays, else 30 days when settings.json says
// nothing. With no Claude root and no override it is UNKNOWN, never a silent 30.
func (d *run) retention() (time.Duration, string, bool) {
	if d.env.Retention > 0 {
		return d.env.Retention, "configured", true
	}
	if d.env.Settings == nil {
		return 0, "", false
	}
	// Named first: a FIFO named settings.json would block ReadFile's open.
	if regularByName(d.env.Settings, claude.SettingsFile) == nil {
		if b, err := fs.ReadFile(d.env.Settings, claude.SettingsFile); err == nil {
			if r, ok := claude.DecodeRetention(b); ok {
				return r, claude.SettingsFile, true
			}
		}
	}
	return claude.DefaultTranscriptRetention, "the default", true
}

// transcript-retention-floor: the oldest */*.jsonl against the window.
// Deliberately different from tt: with no window at all this SKIPS, where tt
// warns; doctor is not in V2.
func (d *run) transcriptRetentionFloor() Result {
	files := d.shallowTranscripts()
	if len(files) == 0 {
		return skip("no transcripts under the projects tree")
	}
	window, from, ok := d.retention()
	if !ok {
		return skip("the retention window is unknown: no Claude root and no override were given")
	}
	oldest := files[0]
	for _, f := range files[1:] {
		if f.mod.Before(oldest.mod) {
			oldest = f
		}
	}
	floorDays := d.env.Now.Sub(oldest.mod).Hours() / 24
	windowDays := window.Hours() / 24
	ev := []string{fmt.Sprintf("%d files; floor %.2f days; window %.2f days (%s)", len(files), floorDays, windowDays, from),
		"oldest: " + oldest.rel}
	if floorDays < windowDays-1 {
		return result(VerdictWarn, fmt.Sprintf("transcripts are reaped %.2f days earlier than the window", windowDays-floorDays), ev...)
	}
	return result(VerdictOK, "the floor is at or beyond the window", ev...)
}

// transcript-deep-population: convergence of the two populations is the
// finding, because then a recursive re-derivation of the floor would look
// right.
func (d *run) transcriptDeepPopulation() Result {
	shallow := d.shallowTranscripts()
	fsys := d.env.Root.Projects.FS
	if len(shallow) == 0 || fsys == nil {
		return skip("no transcripts to compare")
	}
	window, _, ok := d.retention()
	if !ok {
		window = claude.DefaultTranscriptRetention
	}
	cutoff := d.env.Now.Add(-window)
	deep, beyond := 0, 0
	var deepOldest time.Time
	_ = fs.WalkDir(fsys, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, claude.TranscriptSuffix) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		deep++
		mod := info.ModTime().UTC()
		if deepOldest.IsZero() || mod.Before(deepOldest) {
			deepOldest = mod
		}
		if mod.Before(cutoff) {
			beyond++
		}
		return nil
	})
	shallowOldest := shallow[0].mod
	for _, f := range shallow[1:] {
		if f.mod.Before(shallowOldest) {
			shallowOldest = f.mod
		}
	}
	deepDays := d.env.Now.Sub(deepOldest).Hours() / 24
	shallowDays := d.env.Now.Sub(shallowOldest).Hours() / 24
	ev := []string{fmt.Sprintf("shallow: %d files, floor %.2f days", len(shallow), shallowDays),
		fmt.Sprintf("recursive: %d files, floor %.2f days, %d past the window", deep, deepDays, beyond)}
	if deepDays <= shallowDays+1 {
		return result(VerdictWarn, "the populations have converged; the non-recursive glob must be asserted by test, not by inspection", ev...)
	}
	return result(VerdictOK, fmt.Sprintf("the deeper population is %.2f days older", deepDays-shallowDays), ev...)
}

// transcript-untimestamped-types: decided by the DISTANCE back to the newest
// timestamped record against the 64 KB bound; new untimestamped types are
// evidence.
func (d *run) transcriptUntimestampedTypes() Result {
	newest, ok := d.newestTranscript()
	if !ok {
		return skip("no transcript to read")
	}
	lines, err := tailLines(d.env.Root.Projects.FS, newest.rel, claude.TimingTailBytes)
	if err != nil || len(lines) == 0 {
		return skip(fmt.Sprintf("could not read the tail of %s: %v", newest.rel, err))
	}
	known := map[string]bool{}
	for _, t := range claude.Vocabulary().UntimestampedTypes {
		known[t] = true
	}
	var newTypes []string
	bytesBack, recordsBack, consumed := -1, -1, 0
	for i := len(lines) - 1; i >= 0; i-- {
		consumed += len(lines[i]) + 1
		keys, ok := claude.TopLevelKeys(lines[i])
		if !ok {
			continue
		}
		if contains(keys, claude.KeyTimestamp) {
			if bytesBack < 0 {
				bytesBack, recordsBack = consumed, len(lines)-i
			}
			continue
		}
		if l, ok := claude.DecodeLine(lines[i]); ok && l.KindRaw != "" && !known[l.KindRaw] && bytesBack < 0 {
			newTypes = append(newTypes, l.KindRaw)
		}
	}
	ev := []string{"read: " + newest.rel,
		fmt.Sprintf("%d lines; newest timestamped record %d bytes / %d records back", len(lines), bytesBack, recordsBack)}
	if len(newTypes) > 0 {
		sort.Strings(newTypes)
		ev = append(ev, "untimestamped types not in the vocabulary: "+strings.Join(newTypes, ", "))
	}
	switch {
	case bytesBack < 0:
		return result(VerdictFail, "the 64 KB tail holds no timestamped record at all", ev...)
	case bytesBack > claude.TimingTailBytes/4:
		return result(VerdictWarn, fmt.Sprintf("the newest timestamp is %d bytes back, past a quarter of the bound", bytesBack), ev...)
	}
	return result(VerdictOK, fmt.Sprintf("the newest timestamp is %d bytes back", bytesBack), ev...)
}

// transcript-agent-color-shape: records against mentions. The day they part
// with no record left, the record type went away.
func (d *run) transcriptAgentColorShape() Result {
	newest, ok := d.newestTranscript()
	if !ok {
		return skip("no transcript to read")
	}
	lines, err := tailLines(d.env.Root.Projects.FS, newest.rel, claude.FactsTailBytes)
	if err != nil {
		return skip("could not read a transcript tail")
	}
	mention := []byte(strconv.Quote(claude.KeyAgentColor))
	records, mentions := 0, 0
	for _, line := range lines {
		if bytes.Contains(line, mention) {
			mentions++
		}
		if l, ok := claude.DecodeLine(line); ok && l.Kind == claude.LineAgentColor {
			records++
		}
	}
	ev := []string{"read: " + newest.rel, fmt.Sprintf("%d agent-color records, %d lines mentioning the key", records, mentions)}
	switch {
	case records == 0 && mentions > 0:
		return result(VerdictWarn, "the colour is mentioned but no agent-color record remains; colour reads as absent", ev...)
	case records == 0:
		return skip("this tail carries no colour record either way", ev...)
	}
	return result(VerdictOK, "the record type is intact", ev...)
}

// transcript-api-error-key over the five newest tails, because the keys are
// rare enough that one tail often carries neither.
func (d *run) transcriptAPIErrorKey() Result {
	files := append([]transcriptStat(nil), d.shallowTranscripts()...)
	if len(files) == 0 {
		return skip("no transcript to read")
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].rel < files[j].rel
	})
	apiKey, errKey := []byte(strconv.Quote(claude.KeyIsAPIErrorMessage)), []byte(strconv.Quote(claude.KeyIsError))
	api, isErr, read := 0, 0, 0
	for _, f := range files {
		if read >= 5 {
			break
		}
		lines, err := tailLines(d.env.Root.Projects.FS, f.rel, claude.FactsTailBytes)
		if err != nil {
			continue
		}
		read++
		for _, l := range lines {
			if bytes.Contains(l, apiKey) {
				api++
			}
			if bytes.Contains(l, errKey) {
				isErr++
			}
		}
	}
	ev := []string{fmt.Sprintf("over %d recent tails: %s on %d lines, %s on %d", read, claude.KeyIsAPIErrorMessage, api, claude.KeyIsError, isErr)}
	switch {
	case api == 0 && isErr == 0:
		return skip("neither key appears in these tails, the ordinary case for healthy sessions", ev...)
	case api == 0:
		return result(VerdictWarn, "is_error appears but the counted key does not; if it was renamed, API errors read 0 forever", ev...)
	}
	return result(VerdictOK, "the key that is counted is present", ev...)
}

// subagent-meta-files: the LAYOUT is the fixed thing, never the count.
func (d *run) subagentMetaFiles() Result {
	fsys := d.env.Root.Projects.FS
	if fsys == nil {
		return skip("no projects tree was given")
	}
	dirs, err := fs.Glob(fsys, "*/*/"+claude.SubagentsDirName)
	if err != nil || len(dirs) == 0 {
		return skip("no subagents directory under the projects tree")
	}
	metas := 0
	for _, dir := range dirs {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), claude.SubagentMetaSuffix) {
				metas++
			}
		}
	}
	ev := []string{fmt.Sprintf("%d meta files across %d subagents directories", metas, len(dirs))}
	if metas == 0 {
		return result(VerdictWarn, "the directories exist but hold no meta files", ev...)
	}
	return result(VerdictOK, "the layout is intact", ev...)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
