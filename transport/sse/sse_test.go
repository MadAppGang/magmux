package sse

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestEncodeShape pins the bytes, because the format has no escape: whatever
// goes into a `data:` line is what a client reads back, and the only framing is
// the blank line.
func TestEncodeShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    string
		id   string
		data string
		want string
	}{
		{
			name: "a named event with an id",
			n:    "frame", id: "7", data: `{"type":"frame"}`,
			want: "event: frame\nid: 7\ndata: {\"type\":\"frame\"}\n\n",
		},
		{
			name: "no name and no id",
			data: "x",
			want: "data: x\n\n",
		},
		{
			name: "a multi-line payload becomes several data lines",
			n:    "e", data: "a\nb\nc",
			want: "event: e\ndata: a\ndata: b\ndata: c\n\n",
		},
		{
			name: "CRLF in the payload does not leak a stray CR",
			n:    "e", data: "a\r\nb",
			want: "event: e\ndata: a\ndata: b\n\n",
		},
		{
			// A browser dispatches nothing for an event whose data buffer is
			// empty, so the empty data line is what makes this a message at all.
			name: "an empty payload still gets a data line",
			n:    "ping",
			want: "event: ping\ndata: \n\n",
		},
		{
			name: "a newline in a FIELD cannot start a new field",
			n:    "ev\nil: x", id: "1\n2", data: "d",
			want: "event: evil: x\nid: 12\ndata: d\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Encode(tc.n, tc.id, []byte(tc.data))); got != tc.want {
				t.Errorf("Encode =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestRoundTrip is the decoder against the encoder, including the parsing rules
// that a naive split on ": " gets wrong.
func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if _, err := w.Comment("keepalive"); err != nil {
		t.Fatal(err)
	}
	for i, d := range []string{`{"type":"snapshot"}`, "multi\nline", ""} {
		if _, err := w.Event("e", string(rune('0'+i)), []byte(d)); err != nil {
			t.Fatal(err)
		}
	}

	r := NewReader(&buf)
	want := []struct{ id, data string }{
		{"0", `{"type":"snapshot"}`},
		{"1", "multi\nline"},
		{"2", ""},
	}
	for i, wantEv := range want {
		ev, err := r.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if ev.Name != "e" || ev.ID != wantEv.id || string(ev.Data) != wantEv.data {
			t.Errorf("event %d = %+v, want id %q data %q", i, ev, wantEv.id, wantEv.data)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("after the last event the stream is at EOF, got %v", err)
	}
}

// TestReaderFollowsTheParsingRules covers the cases magmux's own encoder never
// produces but a real server might, because this reader is also what a client
// harness uses against magmux.
func TestReaderFollowsTheParsingRules(t *testing.T) {
	in := strings.Join([]string{
		": a comment is ignored",
		"",
		"event:nospace",     // one optional space, not required
		"data:  two spaces", // only the FIRST space is stripped
		"",
		"data", // a field with no colon has an empty value
		"",
		"retry: 250",
		"data: after retry",
		"",
	}, "\n") + "\n"

	r := NewReader(strings.NewReader(in))

	ev, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "nospace" || string(ev.Data) != " two spaces" {
		t.Errorf("first = %+v; want name nospace, data %q", ev, " two spaces")
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "" || string(ev.Data) != "" {
		t.Errorf("second = %+v; a bare `data` line is an empty value", ev)
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Retry != 250 || string(ev.Data) != "after retry" {
		t.Errorf("third = %+v", ev)
	}
}

// TestATruncatedEventIsNotDispatched is the property TestFinalizeTornWriteSSE
// depends on at the other end of the wire: half a `data:` line must never be
// read as a whole message.
func TestATruncatedEventIsNotDispatched(t *testing.T) {
	r := NewReader(strings.NewReader("event: results\ndata: {\"type\":\"resu"))
	ev, err := r.Next()
	if err == nil {
		t.Fatalf("a stream that ended mid-event must not dispatch it, got %+v", ev)
	}
	if err != io.EOF && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("want EOF, got %v", err)
	}
}

// FuzzSSERead drives the decoder from arbitrary bytes: it parses whatever a
// server sends, and magmux's own harness reads magmux's output through it.
func FuzzSSERead(f *testing.F) {
	f.Add("event: frame\nid: 1\ndata: {}\n\n")
	f.Add("data\n\n")
	f.Add(": comment\n\n\n\n")
	f.Add("retry: x\ndata: a\ndata: b\n\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, in string) {
		r := NewReader(strings.NewReader(in))
		for i := 0; i < 64; i++ {
			ev, err := r.Next()
			if err != nil {
				return
			}
			// Whatever was decoded must re-encode to something this same
			// reader decodes identically. That is the only invariant a
			// format with no escapes can be held to, and it is the one that
			// catches a field value smuggling a newline.
			again := NewReader(bytes.NewReader(Encode(ev.Name, ev.ID, ev.Data)))
			got, err := again.Next()
			if err != nil {
				t.Fatalf("re-reading an encoded event failed: %v (from %+v)", err, ev)
			}
			if got.Name != ev.Name || got.ID != ev.ID || !bytes.Equal(got.Data, ev.Data) {
				t.Fatalf("round trip changed the event: %+v -> %+v", ev, got)
			}
		}
	})
}
