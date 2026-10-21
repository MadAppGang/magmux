package adapters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/MadAppGang/magmux/internal/obs"
)

// ParseRecord turns the bytes of one `~/.claude/sessions/<pid>.json` record
// into a [obs.Session].
//
// # Why this is here and not in the registry source
//
// schema.go is the only file allowed to NAME Claude's keys and values, and
// this file is the only one allowed to read a record through them. Everything
// it hands back is in obs's own vocabulary, so the package that walks the
// directory, retries torn reads and asks the liveness oracle never touches a
// Claude string at all. When the format changes, two files here move and
// nothing else does.
//
// # It is lenient in one direction only
//
// Unknown keys are IGNORED, not an error: a future Claude with five new keys
// must decode exactly as today's does, because the alternative is that magmux
// stops reporting the fleet the day Claude ships a field. Key ORDER is
// likewise irrelevant — this reads by name into a map, and a record whose keys
// arrive in a different order produces an identical [obs.Session].
//
// A value that IS recognised but cannot be understood — a status word this
// version has never seen, an instant in the wrong unit — degrades to "no
// opinion" with the raw form kept, and is named in the returned notes. It is
// never guessed at. The notes exist so the caller can report a degradation
// rather than swallow it; see the registry's problems.
//
// The error return is for one thing only: the bytes are not a record at all.
// That means TORN, not dead — Claude rewrites these files in place, so a
// reader can genuinely catch one half-written — and the caller's job is to
// retry, not to conclude anything about the session. See the registry.
func ParseRecord(data []byte) (obs.Session, []string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return obs.Session{}, nil, fmt.Errorf("adapters: the record is empty")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers stay as text until something asks for one. A 13-digit epoch and
	// a pid both survive float64 comfortably, but "comfortably" is a property
	// of the values we have measured rather than of the format, and a pid
	// coerced through a float is the kind of thing nobody looks at twice.
	dec.UseNumber()

	var rec map[string]any
	if err := dec.Decode(&rec); err != nil {
		return obs.Session{}, nil, fmt.Errorf("adapters: %w", err)
	}

	var notes []string
	note := func(format string, a ...any) { notes = append(notes, fmt.Sprintf(format, a...)) }

	s := obs.Session{
		ID:   recString(rec, KeySessionID),
		Name: recString(rec, KeyName),
	}

	if pid, ok := recInt(rec, KeyPID); ok {
		s.PID = pid
	} else {
		note("no usable process id in the record")
	}

	// PreviousNames keeps the order it was written in. Flattening to the
	// latest would lose the only trail back to what a human last called a
	// session before it was renamed.
	//
	// Each element is decoded ON ITS OWN, in whichever of Claude's two shapes
	// it arrived in — see KeyFormerNames and DecodeFormerName. One bad element
	// costs that element and is noted by its index; every other name
	// survives. PreviousNames and PreviousNameDetail are written here and
	// nowhere else, from the one loop, which is what keeps them parallel.
	//
	// Until fix round 2 this read strings only. Claude Code 2.1.276 writes
	// objects, so every former name on the machine was noted and dropped, and
	// a renamed session was published with previousNames null.
	if raw, present := rec[KeyFormerNames]; present && raw != nil {
		list, isList := raw.([]any)
		if !isList {
			// A present value of the wrong shape is a format change, the same
			// kind as an object element was, one level up. Silence would make
			// it indistinguishable from a session that was never renamed.
			note("%s is %s, not a list, so no previous name was read", KeyFormerNames, jsonKind(raw))
		}
		for i, v := range list {
			p, ok, why := DecodeFormerName(v)
			for _, w := range why {
				note("previous name %d %s", i, w)
			}
			if !ok {
				continue
			}
			s.PreviousNames = append(s.PreviousNames, p.Name)
			s.PreviousNameDetail = append(s.PreviousNameDetail, p)
		}
	}

	// The raw string is kept whether or not it decodes. When Status is
	// StatusUnknown it is the ONLY source of truth, and when Status is known it
	// is still what the tool actually wrote — which is what makes a value
	// magmux does not recognise discoverable rather than merely absent.
	s.StatusRaw = recString(rec, KeyStatus)
	if v, ok := DecodeStatus(s.StatusRaw); ok {
		s.Status = v
	} else if s.StatusRaw != "" {
		note("unrecognised status %q", s.StatusRaw)
	}

	// Absence is normal and is NOT a degradation: the key is simply not there
	// on a session that is waiting for nothing nameable, and the answer for
	// that is "undetermined". A present value that does not decode IS worth
	// naming, because it is a format change.
	s.WaitingRaw = recString(rec, KeyWaitingFor)
	if v, ok := DecodeWaitingFor(s.WaitingRaw); ok {
		s.Waiting = v
	} else if s.WaitingRaw != "" {
		note("unrecognised waiting reason %q", s.WaitingRaw)
	}

	if ms, ok := recInt64(rec, KeyStatusUpdatedAt); ok {
		if at, ok := DecodeInstant(ms); ok {
			s.StatusSince = &at
		} else {
			note("%s is %d, which is not a plausible epoch-millisecond value", KeyStatusUpdatedAt, ms)
		}
	}

	// Deliberately NOT decoded here, each for its own reason:
	//
	//   - KeyCWD is the session's LIVE working directory and Session.LaunchDir
	//     is the directory it was LAUNCHED in. They are usually equal and the
	//     difference is exactly the 4% that resolves to another session's
	//     transcripts — a directory that EXISTS and holds somebody else's data,
	//     so the failure is "read the wrong session and believe it". magmux
	//     holds the real launch directory for the sessions it spawned; that is
	//     where LaunchDir comes from, and nothing else will do.
	//   - KeyTmux is the terminal coordinate, and the pane gate that reads it
	//     needs magmux's pane table. Parsing it now would mean inventing a
	//     field for a raw coordinate that nothing yet consumes.
	//   - KeyNameSource, KeyPeerProtocol and KeyPeerFeatures have no field in
	//     the core vocabulary because nothing has needed one.
	return s, notes, nil
}

// PIDFromRecordName reads the process id out of a registry filename.
//
// The layout is `<pid>.json`, and the pid in the NAME is an index into the
// record rather than a second opinion about it — see the registry, which
// reports a disagreement as a problem and believes the document.
//
// It is here rather than in the registry because the filename convention is
// Claude Code's, exactly like the keys above, and a second place that knows it
// is a second place to change.
func PIDFromRecordName(name string) (int, bool) {
	base := strings.TrimSuffix(name, RecordSuffix)
	if base == name {
		return 0, false
	}
	pid, err := strconv.Atoi(base)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// RecordSuffix is the suffix of a registry record's filename.
//
// The directory also holds `<pid>.<sha256>.key` siblings and whatever else a
// user drops in, so this is the filter — and a `.key` file is never read for a
// second reason: they are named like key material.
const RecordSuffix = ".json"

func recString(rec map[string]any, key string) string {
	s, _ := rec[key].(string)
	return s
}

func recInt64(rec map[string]any, key string) (int64, bool) {
	n, ok := rec[key].(json.Number)
	if !ok {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}

func recInt(rec map[string]any, key string) (int, bool) {
	v, ok := recInt64(rec, key)
	if !ok {
		return 0, false
	}
	return int(v), true
}
