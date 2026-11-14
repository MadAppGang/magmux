package difftest

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Report is one comparison, summarised: counts per D-row, every uncited
// difference, the one-sided rows, and (filled by the driver) the RACED rows
// and NOT VERIFIED scenarios.
type Report struct {
	PerRow      map[string]int
	Uncited     []Diff
	Cited       []Diff // every cited difference, for a verbose report
	OneSided    int
	Raced       []RowKey
	NotVerified []string
}

// Summarise counts. Raced and NotVerified are the driver's to fill: a pure
// comparison cannot know either.
func Summarise(d []Diff) Report {
	r := Report{PerRow: map[string]int{}}
	for _, x := range d {
		if x.OneSided {
			r.OneSided++
		}
		if x.Cited == "" {
			r.Uncited = append(r.Uncited, x)
			continue
		}
		r.PerRow[x.Cited]++
		r.Cited = append(r.Cited, x)
	}
	return r
}

// Format renders a difference for a report line.
func (d Diff) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", d.Key)
	if d.OneSided {
		fmt.Fprintf(&b, ": row on one side only (tt %s, lib %s)", d.TT, d.Lib)
	} else {
		fmt.Fprintf(&b, ": %s: tt %s, lib %s", d.Field, d.TT, d.Lib)
	}
	switch {
	case d.Cited != "":
		fmt.Fprintf(&b, " [%s]", d.Cited)
	case len(d.Candidates) > 0:
		fmt.Fprintf(&b, " [UNCITED: ambiguous between %s]", strings.Join(d.Candidates, ", "))
	default:
		b.WriteString(" [UNCITED]")
	}
	return b.String()
}

// FilenameOrderKept is _diff's filename-order preflight (§8.2 step 4), here so
// CI tests it. Each group lists the canonical registry file names that share
// a sessionId and a Record.PID. A name that is "<pid>.json" or
// "<pid>.<anything>" is rewritten through pidMap, as the driver renames it;
// any other name is kept. The error names the first group whose rewritten
// names do not sort byte-wise as the canonical ones do: such a group's
// speaker (§3.4 rule 2, the lowest name byte-wise) would change.
func FilenameOrderKept(groups [][]string, pidMap map[int]int) error {
	for _, g := range groups {
		canon := slices.Clone(g)
		slices.Sort(canon)
		rewritten := make([]string, len(canon))
		back := map[string]string{}
		for i, name := range canon {
			rewritten[i] = RenameByPID(name, pidMap)
			back[rewritten[i]] = name
		}
		slices.Sort(rewritten)
		for i := range canon {
			if back[rewritten[i]] != canon[i] {
				return fmt.Errorf("difftest: the registry files %v sort as %v after the pid rewrite, so the speaker of their group changes",
					canon, rewritten)
			}
		}
	}
	return nil
}

// RenameByPID is the driver's rename rule for one registry-directory entry:
// a base name whose first dot-separated element is a decimal pid in pidMap
// gets that pid replaced; every other name is unchanged.
func RenameByPID(name string, pidMap map[int]int) string {
	dir, base := path.Split(name)
	head, rest, _ := strings.Cut(base, ".")
	n, err := strconv.Atoi(head)
	if err != nil || head == "" || head[0] < '0' || head[0] > '9' {
		return name
	}
	to, ok := pidMap[n]
	if !ok {
		return name
	}
	out := strconv.Itoa(to)
	if strings.Contains(base, ".") {
		out += "." + rest
	}
	return dir + out
}
