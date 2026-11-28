package mux

// A gated typed send authorises each write by the box matching what was
// written so far under inputKey, which cannot see whitespace. So the gated
// path re-cuts its writes: every write but the last ends with a visible rune.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/protocol"
)

func TestEvidencedWrites(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"no whitespace is unchanged", strings.Repeat("abcdefghij", 10),
			[]string{strings.Repeat("abcdefghij", 3) + "ab", "cdefghij" + strings.Repeat("abcdefghij", 2) + "abcd", "efghij" + strings.Repeat("abcdefghij", 2) + "abcdef", "ghij"}},
		{"a blank run joins the next visible rune", "fix" + strings.Repeat(" ", 70) + "it",
			[]string{"fix", strings.Repeat(" ", 70) + "it"}},
		{"newlines join the next line", "a\n\n\nb",
			[]string{"a", "\n\n\nb"}},
		{"a CRLF is one typed newline", "a\r\nb",
			[]string{"a", "\nb"}},
		{"trailing whitespace rides in the last write", "done  \n\n",
			[]string{"done  \n\n"}},
		{"all whitespace is one write", "   \n  \n",
			[]string{"   \n  \n"}},
		{"leading whitespace leads the first write", "\n  x y",
			[]string{"\n  x y"}},
		{"empty text has no writes", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := typedWrites(c.text)
			out := evidencedWrites(in)
			got := make([]string, len(out))
			for i, w := range out {
				got[i] = string(w)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) || len(got) != len(c.want) {
				t.Errorf("evidencedWrites(%q) = %q, want %q", c.text, got, c.want)
			}
			if a, b := bytes.Join(in, nil), bytes.Join(out, nil); !bytes.Equal(a, b) {
				t.Errorf("the re-cut changed the bytes: %q, want %q", b, a)
			}
			for i, w := range out[:max(len(out)-1, 0)] {
				if r, _ := utf8.DecodeLastRune(w); unicode.IsSpace(r) {
					t.Errorf("write %d of %d (%q) ends with whitespace; nothing in the box could show it was read", i+1, len(out), w)
				}
			}
		})
	}
}

func TestEvidencedWritesDoesNotAliasItsInput(t *testing.T) {
	in := typedWrites("a  b  c")
	keep := bytes.Join(in, nil)
	out := evidencedWrites(in)
	for _, w := range out {
		for i := range w {
			w[i] = 'X'
		}
	}
	if got := bytes.Join(in, nil); !bytes.Equal(got, keep) {
		t.Errorf("writing to the output changed the input: %q, want %q", got, keep)
	}
}

func TestABlankRunPastThePasteThresholdIsBadRequest(t *testing.T) {
	text := "start" + strings.Repeat(" ", pasteCollapseRunes) + "end"
	g := &fakeGate{}
	m, _, log := gatedMux(t, g)

	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})

	wantErr(t, o, protocol.CodeBadRequest, "", false)
	if !strings.Contains(o.err.Error(), fmt.Sprintf("%d blank runes", pasteCollapseRunes)) {
		t.Errorf("the refusal does not name the run's length: %v", o.err)
	}
	if got := log.settle(); got != "" {
		t.Errorf("a refused send wrote %q", got)
	}
	if n := outRows(m); n != 0 {
		t.Errorf("a refused send filed %d OUT rows", n)
	}

	t.Run("a run that fits is typed", func(t *testing.T) {
		g := &fakeGate{}
		m, _, log := gatedMux(t, g)
		text := "start" + strings.Repeat(" ", pasteCollapseRunes-10) + "end"
		o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
		if o.err != nil {
			t.Fatalf("send: %v", o.err)
		}
		if got := log.settle(); got != text+"\r" {
			t.Errorf("the child received %d bytes, want the text and one Enter", len(got))
		}
		if o.rep.writesTotal != 2 || o.rep.writes != 2 {
			t.Errorf("writes %d/%d, want 2/2: the blank run rides with the rune after it", o.rep.writes, o.rep.writesTotal)
		}
	})
}
