package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

// Ported from S:internal/obs/adapters/projectdir_test.go and spec_decode_test.go.
// The environment override (ProjectDirName) stays with the host: it reads the
// environment, and this package imports no os.

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

// The trap, stated as a test: a record carries the LIVE working directory,
// and encoding it names the decoy directory, which exists and holds another
// session's transcript. The library never does this; it locates by id.
func TestEncodingTheLiveDirectoryFindsTheDecoy(t *testing.T) {
	r, _, _ := mmRecord(t, "98693.json")
	derived := EncodeProjectDir(r.CWD)
	const decoy = "-Users-user-dev-magmux--claude-worktrees-claude-session-control"
	if derived != decoy {
		t.Errorf("EncodeProjectDir(live cwd) = %q, want the decoy %q", derived, decoy)
	}
	if launched := EncodeProjectDir("/Users/user/dev/magmux"); derived == launched || !strings.HasPrefix(derived, launched) {
		t.Fatalf("the trap is disarmed: %q against %q", derived, launched)
	}
}

// WRONG: encoder lowercases/collapses/trims, counts runes instead of UTF-16
// units, hashes the sanitized/truncated input, forgets signed int32 overflow,
// or hashes at length 200. These hash suffixes are fixed independent vectors.
func TestSpecEncodeProjectDir(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"real_launch", "/Users/user/dev/magmux", "-Users-user-dev-magmux"},
		{"real_worktree", "/Users/user/dev/magmux/.claude/worktrees/claude-session-control", "-Users-user-dev-magmux--claude-worktrees-claude-session-control"},
		{"empty", "", ""}, {"root", "/", "-"}, {"case", "/AbC123/", "-AbC123-"},
		{"separators", "/foo//bar._ +", "-foo--bar----"}, {"tilde", "~/dev/x", "--dev-x"},
		{"nfc", "/Users/user/dev/café", "-Users-user-dev-caf-"},
		{"non_bmp", "/Users/user/dev/🚀rocket", "-Users-user-dev---rocket"}, {"cjk", "/中文", "---"},
		{"199", strings.Repeat("a", 199), strings.Repeat("a", 199)},
		{"200", strings.Repeat("a", 200), strings.Repeat("a", 200)},
		{"201", strings.Repeat("a", 201), strings.Repeat("a", 200) + "-rkvsv5"},
		{"utf16_200", strings.Repeat("a", 198) + "🚀", strings.Repeat("a", 198) + "--"},
		{"utf16_201", strings.Repeat("a", 199) + "🚀", strings.Repeat("a", 199) + "--rlxqjo"},
		{"original_dot", strings.Repeat("a", 200) + ".", strings.Repeat("a", 200) + "-rkvstq"},
		{"original_underscore", strings.Repeat("a", 200) + "_", strings.Repeat("a", 200) + "-rkvsv3"},
		// Re-derived when the corpus was sanitised: the input used to name a
		// real user, and changing the string changes the hash. Verified by an
		// independent awk implementation of the same arithmetic, not by this
		// package. The five vectors above it are the untouched ones.
		{"documented_negative_hash", "/Users/user/dev/" + strings.Repeat("x", 250), "-Users-user-dev-" + strings.Repeat("x", 184) + "-c9gw6w"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := EncodeProjectDir(tt.in); got != tt.want {
				t.Fatalf("EncodeProjectDir(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The 20,019 vectors. The encoder extracted verbatim from the Claude Code
// bundle (quoted on EncodeProjectDir) was run under bun over 20,019 inputs from
// a deterministic xorshift32 generator; this test regenerates the same inputs
// and holds the port to the SHA-256 of the 20,019 outputs, so a single wrong
// byte anywhere fails it. 8,150 of the inputs take the truncate-and-hash
// branch. The original 20,019-vector run of 2026-09 was a one-off oracle
// comparison that was never committed; this is its committed replacement, and
// the generator is in the implementation log.
func TestEncodeProjectDirAgreesWithTheBundleOn20019Vectors(t *testing.T) {
	const (
		n          = 20019
		wantHashed = 8150
		wantDigest = "21de14fd543aa522fd42472c1c91e02d03cb94ab8c5178a2f450960e7b5eab15"
	)
	alphabet := []string{"a", "b", "z", "A", "Q", "Z", "0", "5", "9", "/", "/", "/", ".", "_", "-", " ", "+", "~",
		"é", "é", "中", "文", "🚀", "𝄞", "ß", " "}
	s := uint32(0x9e3779b9)
	next := func() uint32 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return s
	}
	samples := map[int]string{
		0:     "095-Z-b5Q---Aaz----5Z--Q0e---0-Q---A--Q-Q---e-5-Q-e---a0AA-b05--A--z-9Q---z----5--5--Ze-aA------99-5---b9-bQ--b5--e-",
		16000: "--A------b--Q---e--e-------e-b------------Q----------9---A------bA---Q--------9zAa-e----Ze-b---9-----A9--99----A--0e-Z9--e--A9--9--------5----0---9---Z-0--A-aZZ------5--A95e--Z-------A-----e-----------cp209w",
	}
	h := sha256.New()
	hashed := 0
	for i := 0; i < n; i++ {
		length := int(next() % 301)
		var b strings.Builder
		for j := 0; j < length; j++ {
			b.WriteString(alphabet[next()%uint32(len(alphabet))])
		}
		out := EncodeProjectDir(b.String())
		if len(out) > projectDirMaxLen && out[projectDirMaxLen] == '-' && !strings.Contains(out[projectDirMaxLen+1:], "-") {
			hashed++
		}
		if want, ok := samples[i]; ok && out != want {
			t.Errorf("vector %d: %q, want %q", i, out, want)
		}
		h.Write([]byte(out + "\n"))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantDigest {
		t.Fatalf("the 20,019 outputs hash to %s, want the bundle's %s", got, wantDigest)
	}
	if hashed != wantHashed {
		t.Errorf("%d vectors took the hash branch, want %d", hashed, wantHashed)
	}
}
