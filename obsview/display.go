package obsview

// Registry text on its way to a person: the ONE rule, shared by every renderer.
//
// A session's name, its raw status and waiting words, and the reasons the
// verdict builds out of its transcript all come from files magmux does not
// write and cannot constrain. They are UNTRUSTED, and not in a theoretical
// sense: a name is not always typed by a person — Claude Code can generate one
// from conversation content — so text an attacker can influence (a crafted
// repository, a prompt) can reach it, and from there a terminal. An OSC 52
// sequence in a name is a clipboard write; others retitle the window, forge a
// hyperlink, or move the cursor to overwrite a YES in the ATTENTION column.
//
// # Why this file exists
//
// Until fix round 2 the rule lived in ONE of three renderers. The notifier
// neutralised these strings before handing them to osascript; `magmux agents`
// (and so `list_agents`, which returns its table) wrote them to the terminal
// byte for byte, and the attention view painted them into the panel's own VT
// parser. Two renderers had forgotten a rule the third followed privately —
// and the private copy was itself incomplete, flattening C0 and DEL but
// passing C1, so U+009B, an 8-bit CSI, went through all three. Three copies of
// a safety rule is how that happens, so there is one, here, and the notifier's
// copy is deleted. TestTheNotifierHasNoPrivateCopyOfTheRule (in mux, which reads
// mux/obsnotify.go) keeps it deleted.
//
// # Where it is applied, and where it deliberately is not
//
// At the RENDERERS, never at decode. The document keeps what the tool wrote,
// exactly: StatusRaw is documented as the only source of truth when a status
// is unrecognised, and a raw value "cleaned" at the source would no longer be
// raw. `--json` therefore carries every string exact — see TerminalSafeJSON,
// which escapes rather than replaces.

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// DisplaySafe returns s with every control character neutralised, for a person
// to read on a terminal, in the panel, or in a notification banner.
//
// Three classes are neutralised, and they are the three a terminal acts on:
// C0 (U+0000-U+001F, ESC and BEL among them), DEL (U+007F), and C1
// (U+0080-U+009F, which includes the 8-bit CSI U+009B and OSC U+009D). A byte
// that is not valid UTF-8 is neutralised too: ranging over a string yields
// U+FFFD for it, which is what is written, so a raw 0x9B can never reach a
// terminal that would read it as an 8-bit CSI.
//
// The replacement is chosen per class:
//
//   - whitespace controls (tab, newline, carriage return, vertical tab, form
//     feed) become a SPACE. A multi-line error text stays readable on one
//     line, and a newline in a name cannot forge a row of a table;
//   - every other control becomes U+FFFD, VISIBLY. Deleting them would leave
//     a name that silently lost bytes, and a person has a right to see that a
//     session's name held something strange — "\x1b]52;…" reads as
//     "�]52;…", which is inert and says what it was.
//
// Everything else is kept, U+00A0 onwards included; this is a rule about
// control characters, not about which scripts a name may use.
func DisplaySafe(s string) string {
	clean := true
	for i, r := range s {
		if isDisplayControl(r) || (r == utf8.RuneError && !isEncodedRuneError(s[i:])) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t', r == '\n', r == '\r', r == '\v', r == '\f':
			b.WriteByte(' ')
		case isDisplayControl(r):
			b.WriteRune(utf8.RuneError)
		default:
			// An invalid byte arrives here as utf8.RuneError and is written as
			// its three-byte encoding — the raw byte is gone either way.
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isDisplayControl is C0, DEL and C1: the characters a terminal acts on
// rather than prints.
func isDisplayControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// isEncodedRuneError reports whether s begins with a genuine, correctly
// encoded U+FFFD — which is text and is kept — as opposed to an invalid byte
// that ranging over the string also reports as U+FFFD.
func isEncodedRuneError(s string) bool {
	_, size := utf8.DecodeRuneInString(s)
	return size == 3
}

// TerminalSafeJSON makes encoding/json's output safe to print on a terminal
// WITHOUT changing the document it encodes.
//
// # Why this is needed at all
//
// encoding/json escapes C0 — ESC becomes \u001b, BEL \u0007 — and it was
// assumed, in the brief for fix round 2, that this made `--json` safe. It
// does not: it writes DEL and every C1 character RAW, so a name carrying
// U+009B put an 8-bit CSI on the terminal of anyone running `magmux agents
// --json` without a pipe. Measured, not inferred, and kept as a test:
// TestJSONCarriesRegistryTextExactAndEscaped.
//
// # Why escaping is exact
//
// JSON may escape any character as \u followed by four hex digits, and a
// decoder reads the escape back as the same character. So each raw DEL or C1
// becomes its escape, and the decoded document is identical, byte for byte,
// to the one encoded — which is the property `--json` owes a consumer, and
// the reason the renderers' replacement rule does not apply here.
//
// It is safe to do on the ENCODED bytes. encoding/json's output is valid
// UTF-8 (an invalid input byte is already written as \ufffd), every JSON
// token outside a string is ASCII, and in valid UTF-8 the pair C2 80-9F can
// only be a C1 character. So a match can only be inside a string, and only be
// the character it looks like.
func TerminalSafeJSON(b []byte) []byte {
	if bytes.IndexByte(b, 0x7f) < 0 && !hasC1(b) {
		return b
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(b)+16)
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x7f:
			out = append(out, '\\', 'u', '0', '0', '7', 'f')
		case c == 0xc2 && i+1 < len(b) && b[i+1] >= 0x80 && b[i+1] <= 0x9f:
			n := b[i+1]
			out = append(out, '\\', 'u', '0', '0', hex[n>>4], hex[n&0xf])
			i++
		default:
			out = append(out, c)
		}
	}
	return out
}

func hasC1(b []byte) bool {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xc2 && b[i+1] >= 0x80 && b[i+1] <= 0x9f {
			return true
		}
	}
	return false
}
