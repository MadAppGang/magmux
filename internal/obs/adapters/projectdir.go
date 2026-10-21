package adapters

import (
	"os"
	"regexp"
	"strconv"
	"unicode/utf16"
)

// projectDirMaxLen is the length at which Claude Code stops using the encoded
// directory whole and starts truncating it with a hash.
const projectDirMaxLen = 200

// EncodeProjectDir returns the directory name Claude Code files a session's
// transcript under, at <config>/projects/<name>/<sessionId>.jsonl.
//
// # The input is the LAUNCH directory, not the live one
//
// launchDir must be the directory Claude Code was started in, NFC-normalised
// and with symlinks already resolved. It is not the session's current working
// directory, and the difference is the entire reason this function is easy to
// get wrong: a session that changes directory — or that is moved into a
// worktree after startup — keeps writing to the directory chosen at startup.
//
// Feeding the live directory to a correct encoder resolves about 96% of real
// sessions. The other 4% do not fail loudly: the wrongly derived name is
// usually a directory that EXISTS, holding another session's transcripts, so
// the failure is "read the wrong session and believe it" rather than "file not
// found". Every reimplementation hits the same ceiling for the same reason,
// and none of them are wrong about the transformation.
//
// magmux is exempt from that ceiling only because it spawns the sessions it
// observes, so it holds the launch directory itself. It is the value magmux
// passed to the child, and nothing else will do. A session magmux did not
// start, or one that moved, needs the fallback that scans the project
// directories and matches on transcript content — that fallback is not
// belt-and-braces, it is the only correct answer for those cases.
//
// # The transformation
//
// Every character that is not [a-zA-Z0-9] becomes a single dash. Nothing is
// lowercased, no run of dashes is collapsed, no leading or trailing dash is
// trimmed, a tilde is not expanded, and an empty string encodes to an empty
// string with no fallback name. The replacement is per UTF-16 CODE UNIT, not
// per rune, so a character outside the basic multilingual plane — an emoji —
// is a surrogate pair and becomes TWO dashes.
//
// Beware a similarly shaped helper in the same Claude Code module that
// lowercases, collapses runs and trims edge dashes. It names workflow scripts,
// not project directories, and it does exactly what a reimplementer assumes,
// so finding it first yields a confident, wrong port.
//
// Extracted verbatim from the Claude Code 2.1.273 bundle:
//
//	var E8=200;
//	function Le(e){return Math.abs(P8(e)).toString(36)}
//	function k(e){return e.replace(/[^a-zA-Z0-9]/g,"-")}
//	function bk(e){let n=k(e);if(n.length<=E8)return n;
//	               return `${n.slice(0,E8)}-${Le(e)}`}
func EncodeProjectDir(launchDir string) string {
	units := utf16.Encode([]rune(launchDir))
	b := make([]byte, 0, len(units))
	for _, u := range units {
		switch {
		case u >= '0' && u <= '9', u >= 'A' && u <= 'Z', u >= 'a' && u <= 'z':
			b = append(b, byte(u))
		default:
			b = append(b, '-')
		}
	}
	if len(b) <= projectDirMaxLen {
		return string(b)
	}
	// The hash is taken over the ORIGINAL string, not the encoded prefix, so
	// two directories sharing a 200-character prefix still get different
	// names. Truncation alone would collide them.
	return string(b[:projectDirMaxLen]) + "-" + base36Abs(javaHash(launchDir))
}

// javaHash is the classic 31x string hash over UTF-16 code units, truncated to
// a signed 32-bit integer at every step.
//
// Extracted verbatim:
//
//	function P8(e){let r=0;for(let n=0;n<e.length;n++)
//	               r=(r<<5)-r+e.charCodeAt(n)|0;return r}
//
// JavaScript computes (r<<5)-r+c in doubles and truncates with |0. Since
// r<<5 is 32r modulo 2^32, the pre-truncation value is congruent to 31r+c
// modulo 2^32, so Go's wrapping int32 multiply-add is exact rather than merely
// close. TestJavaHashMatchesTheOracle checks that rather than taking it on
// trust.
func javaHash(s string) int32 {
	var h int32
	for _, u := range utf16.Encode([]rune(s)) {
		h = h*31 + int32(u)
	}
	return h
}

// base36Abs is Math.abs(n).toString(36), with lowercase digits.
//
// The int64 is load-bearing. JavaScript's Math.abs promotes to a double, so
// the most negative int32 becomes 2147483648 and encodes as "zik0zk";
// negating it inside an int32 would overflow straight back to itself and
// produce a name with a minus sign in it.
func base36Abs(n int32) string {
	v := int64(n)
	if v < 0 {
		v = -v
	}
	return strconv.FormatInt(v, 36)
}

var (
	projectDirNameOK       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	projectDirNameReserved = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

// ProjectDirName is EncodeProjectDir with Claude Code's environment override
// applied: CLAUDE_CODE_PROJECT_DIR_NAME replaces the encoding entirely, but
// only when CLAUDE_CONFIG_DIR is also set, and only when it is 1-64 characters
// of [A-Za-z0-9_-] and is not a Windows reserved device name.
//
// The second condition is the one that surprises. The variable on its own does
// nothing at all, so a caller that honours it unconditionally looks in a
// directory Claude Code never wrote to.
func ProjectDirName(launchDir string) string {
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		if n := os.Getenv("CLAUDE_CODE_PROJECT_DIR_NAME"); n != "" &&
			projectDirNameOK.MatchString(n) && !projectDirNameReserved.MatchString(n) {
			return n
		}
	}
	return EncodeProjectDir(launchDir)
}
