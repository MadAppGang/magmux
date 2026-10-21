package adapters

import (
	"math"
	"strings"
	"testing"
)

// TestEncodeProjectDirMatchesClaude runs the port against the vectors
// extracted from the Claude Code 2.1.273 bundle and checked against the
// extracted JavaScript executed as an oracle. Each case is labelled with how
// it was established:
//
//	V — verified against a directory that exists on this machine
//	J — verified against the extracted JavaScript, run under bun
//
// The J cases are the ones that matter most, because several of them cannot
// occur here: no real directory on this machine is over 200 characters, and
// every one of them is pure ASCII.
func TestEncodeProjectDirMatchesClaude(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"a plain repository (V)", "/Users/user/dev/magmux", "-Users-user-dev-magmux"},
		{
			"a worktree inside it (V)",
			"/Users/user/dev/magmux/.claude/worktrees/claude-session-control",
			"-Users-user-dev-magmux--claude-worktrees-claude-session-control",
		},
		{"underscores are NOT preserved (J)", "/Users/user/dev/some_project", "-Users-user-dev-some-project"},
		{"dots become dashes (J)", "/Users/user/dev/project.with.dots", "-Users-user-dev-project-with-dots"},
		{"spaces become dashes (J)", "/Users/user/dev/project with spaces", "-Users-user-dev-project-with-spaces"},
		{"a trailing slash is not trimmed (J)", "/Users/user/dev/", "-Users-user-dev-"},
		{"runs are NOT collapsed (V)", "/foo//bar", "-foo--bar"},
		{"a leading dot (V)", "/Users/user/.claude", "-Users-user--claude"},
		{"a tilde is not expanded (J)", "~/dev/x", "--dev-x"},
		{"case is NOT lowered (V)", "/Users/user/.claude/worktrees/CG70413-failing", "-Users-user--claude-worktrees-CG70413-failing"},
		{"a plus sign (V)", "/Users/user/dev/x/.claude/worktrees/fix+recover-y", "-Users-user-dev-x--claude-worktrees-fix-recover-y"},
		{"a resolved temp path (V)", "/private/tmp/cc-agent-probe", "-private-tmp-cc-agent-probe"},
		{"the empty string has NO fallback (J)", "", ""},
		{"a bare slash (J)", "/", "-"},
		{"a precomposed e-acute is one code unit (J)", "/Users/user/dev/café", "-Users-user-dev-caf-"},
		{"a decomposed one is two, and the e survives (J)", "/Users/user/dev/café", "-Users-user-dev-cafe-"},
		{"a non-BMP character is a surrogate PAIR, so two dashes (J)", "/Users/user/dev/\U0001F680rocket", "-Users-user-dev---rocket"},
		{"two BMP CJK characters plus the slash (J)", "/中文", "---"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EncodeProjectDir(c.in); got != c.want {
				t.Errorf("EncodeProjectDir(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// The hash branch, which no directory on this machine exercises. It proves the
// truncate-and-hash path was transcribed correctly: a wrong shift, a wrong sign
// or a missing truncation all produce a plausible base-36 string that is simply
// not the one Claude Code writes.
//
// This vector WAS one of the 7,841 hash-branch cases the port was checked
// against. It no longer is: sanitising the corpus changed its input, so the
// expected value was re-derived from an independent awk implementation of the
// algorithm (see TestJavaHashMatchesTheOracle). The externally-derived
// truncation vectors that were not touched live in spec_decode_test.go.
func TestEncodeProjectDirHashesWhatItTruncates(t *testing.T) {
	in := "/Users/user/dev/" + strings.Repeat("x", 250)
	want := "-Users-user-dev-" + strings.Repeat("x", 184) + "-c9gw6w"
	got := EncodeProjectDir(in)
	if got != want {
		t.Errorf("EncodeProjectDir(<266 chars>)\n got %q\nwant %q", got, want)
	}
	if len(strings.Split(got, "-c9gw6w")[0]) != projectDirMaxLen {
		t.Errorf("the truncated part is %d characters, want %d",
			len(strings.Split(got, "-c9gw6w")[0]), projectDirMaxLen)
	}
}

// 200 characters is kept whole; 201 is truncated and hashed. Off by one here
// puts a whole class of directories in the wrong place, and the class is
// invisible on any machine whose paths are short.
func TestTwoHundredCharactersIsTheBoundary(t *testing.T) {
	truncated := "-" + strings.Repeat("a", projectDirMaxLen-1)

	got := EncodeProjectDir("/" + strings.Repeat("a", projectDirMaxLen-1))
	if got != truncated {
		t.Errorf("a name exactly at the boundary was not returned whole:\n got %q\nwant %q", got, truncated)
	}

	got = EncodeProjectDir("/" + strings.Repeat("a", projectDirMaxLen))
	if len(got) <= projectDirMaxLen {
		t.Fatalf("a name one character over the boundary was not hashed: %q", got)
	}
	if got[:projectDirMaxLen] != truncated {
		t.Errorf("the first %d characters were not the truncated encoding: %q",
			projectDirMaxLen, got[:projectDirMaxLen])
	}
	if got[projectDirMaxLen] != '-' {
		t.Errorf("the hash is not separated from the encoding by a dash: %q", got)
	}
	if hash := got[projectDirMaxLen+1:]; hash == "" || strings.ContainsAny(hash, "-") {
		t.Errorf("the hash suffix %q is not a bare base-36 number", hash)
	}
}

// The hash is taken over the ORIGINAL string, not over the truncated prefix.
// Hashing the prefix would give every directory sharing a 200-character head
// the same name — which is precisely the collision the hash exists to avoid,
// reintroduced by an easy misreading of the source.
func TestTheHashIsTakenOverTheWholeInput(t *testing.T) {
	head := "/Users/user/dev/" + strings.Repeat("x", 240)
	a := EncodeProjectDir(head + "one")
	b := EncodeProjectDir(head + "two")

	if a[:projectDirMaxLen] != b[:projectDirMaxLen] {
		t.Fatalf("the two inputs were supposed to share a truncated prefix:\n%q\n%q", a, b)
	}
	if a == b {
		t.Errorf("two directories differing only after character 200 encoded identically: %q", a)
	}
}

// The hash itself, against the classic 31x string hash. The three short
// vectors are the well-known values for this hash, so a transcription error
// shows up without needing the bundle.
//
// The long vector was RE-DERIVED when the corpus was sanitised: its input used
// to name a real user, and changing the string changes the hash. It is not
// taken from this package's own output — an independent implementation of the
// algorithm (31x over UTF-16 code units, truncated to int32) was written in
// awk and produced the same two values, -741498872 and "c9gw6w". The four
// externally-derived truncation vectors in spec_decode_test.go are untouched
// and still pin the hash against the bundle.
func TestJavaHashMatchesTheOracle(t *testing.T) {
	cases := []struct {
		in   string
		want int32
	}{
		{"", 0},
		{"a", 97},
		{"abc", 96354},
		{"/Users/user/dev/" + strings.Repeat("x", 250), -741498872},
	}
	for _, c := range cases {
		if got := javaHash(c.in); got != c.want {
			t.Errorf("javaHash(%.20q…) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The absolute value is taken as a 64-bit number, exactly as JavaScript's
// Math.abs promotes to a double.
//
// Negating the most negative int32 inside an int32 overflows straight back to
// itself, so the naive port emits a name with a minus sign in it — for a
// directory Claude Code named "zik0zk". One input in four billion, and
// undiagnosable when it happens.
func TestBase36AbsPromotesTheMostNegativeInt32(t *testing.T) {
	if got := base36Abs(math.MinInt32); got != "zik0zk" {
		t.Errorf("base36Abs(math.MinInt32) = %q, want %q", got, "zik0zk")
	}
	if got := base36Abs(-1); got != "1" {
		t.Errorf("base36Abs(-1) = %q, want %q", got, "1")
	}
	if got := base36Abs(0); got != "0" {
		t.Errorf("base36Abs(0) = %q, want %q", got, "0")
	}
	if got := base36Abs(35); got != "z" {
		t.Errorf("base36Abs(35) = %q, want %q — base-36 digits are lowercase", got, "z")
	}
}

// The environment override needs BOTH variables. Honouring the name on its own
// sends every lookup to a directory Claude Code never wrote to, and the
// condition is invisible in the variable's own name.
func TestProjectDirNameOverrideNeedsBothVariables(t *testing.T) {
	const launched = "/Users/user/dev/magmux"
	encoded := EncodeProjectDir(launched)

	cases := []struct {
		name       string
		configDir  string
		dirName    string
		want       string
		wantReason string
	}{
		{"neither set", "", "", encoded, "the encoding is used"},
		{"name alone is ignored", "", "custom-dir", encoded, "the name does nothing without the config dir"},
		{"both set", "/tmp/cfg", "custom-dir", "custom-dir", "the name wins"},
		{"an invalid name falls back", "/tmp/cfg", "has spaces", encoded, "the name is rejected"},
		{"a reserved name falls back", "/tmp/cfg", "NUL", encoded, "the name is reserved"},
		{"a too-long name falls back", "/tmp/cfg", strings.Repeat("a", 65), encoded, "the name is over 64 characters"},
		{"the longest legal name wins", "/tmp/cfg", strings.Repeat("a", 64), strings.Repeat("a", 64), "64 characters is legal"},
		{"an empty name falls back", "/tmp/cfg", "", encoded, "an empty name is no name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", c.configDir)
			t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", c.dirName)
			if got := ProjectDirName(launched); got != c.want {
				t.Errorf("ProjectDirName(%q) = %q, want %q (%s)", launched, got, c.want, c.wantReason)
			}
		})
	}
}

// The trap, stated as a test.
//
// A session record carries the LIVE working directory. Encoding that value
// gives a directory name that is not where the transcript is — and, worse, one
// that EXISTS and holds another session's transcripts. The failure is "read
// the wrong session and believe it", which no error path catches.
//
// The record used here has a worktree as its live directory, and the name it
// derives to is the decoy directory built for exactly this case.
func TestEncodingTheLiveDirectoryFindsTheDecoy(t *testing.T) {
	live, ok := recordValue(t, "98693.json", KeyCWD)
	if !ok {
		t.Fatal("test data 98693.json carries no working directory")
	}

	derived := EncodeProjectDir(live)
	const decoy = "-Users-user-dev-magmux--claude-worktrees-claude-session-control"
	if derived != decoy {
		t.Errorf("EncodeProjectDir(live cwd) = %q, want the decoy %q", derived, decoy)
	}

	launched := EncodeProjectDir("/Users/user/dev/magmux")
	if derived == launched {
		t.Fatal("the two directories are the same, so this record proves nothing")
	}
	if !strings.HasPrefix(derived, launched) {
		t.Errorf("the derived name %q is not even under the launch directory's name %q", derived, launched)
	}
}
