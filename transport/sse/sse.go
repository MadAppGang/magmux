// Package sse encodes and decodes the text/event-stream wire format.
//
// It is pure: a Writer over an io.Writer and a Reader over an io.Reader, with
// no http.ResponseWriter, no flushing and no deadlines. The transport half of
// SSE — the headers, the Flush, the write deadline through
// http.ResponseController — belongs to whoever owns the response, and keeping
// it out of here is what lets the encoder be tested against a bytes.Buffer and
// the decoder be driven by a fuzzer.
//
// magmux uses one shape: `event:` is the message's own `type` field, `id:` is a
// per-connection counter, and `data:` is the line-JSON the bus already carries.
// The id is NOT a replay cursor and Last-Event-ID is ignored — magmux has no
// event log to replay from, and a client that reconnects gets a fresh aggregate
// and fresh keyframes, which is a better answer than a gap it cannot detect.
//
// It is an implementation detail of magmux, with no API stability before v1.
package sse

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
)

// Event is one decoded message.
type Event struct {
	Name string
	ID   string
	Data []byte
	// Retry is the reconnection time in milliseconds a server asked for, or 0.
	Retry int
}

// Encode renders one event. data may contain newlines, which become several
// `data:` lines — the format has no escape, so a payload with a newline in it
// is the one thing a naive encoder gets wrong.
func Encode(name, id string, data []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(data) + len(name) + len(id) + 24)
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(sanitizeField(name))
		b.WriteByte('\n')
	}
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(sanitizeField(id))
		b.WriteByte('\n')
	}
	// ALWAYS at least one data line, even for an empty payload. The WHATWG
	// rules dispatch nothing when an event's data buffer is empty, so an
	// `event:` and an `id:` followed straight by the blank line is a message a
	// browser silently drops — and, less obviously, one this package's own
	// Reader could not round-trip, which is what the fuzzer found.
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			b.WriteString("data: ")
			b.Write(trimCR(data))
			b.WriteByte('\n')
			break
		}
		b.WriteString("data: ")
		b.Write(trimCR(data[:i]))
		b.WriteByte('\n')
		data = data[i+1:]
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// sanitizeField strips what would break framing out of a field VALUE: a newline
// would start a new field, a carriage return would be swallowed by the line
// split. Field values in magmux are an event type and a counter, so this can
// only ever fire on a bug — and a bug that corrupts a stream silently is worse
// than one that drops a character.
func sanitizeField(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

// Writer renders events onto a stream.
type Writer struct{ w io.Writer }

// NewWriter wraps a destination.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Event writes one event and reports how many bytes reached w.
func (w *Writer) Event(name, id string, data []byte) (int, error) {
	return w.w.Write(Encode(name, id, data))
}

// Comment writes a `: text` line. Clients ignore it; it exists so a stream can
// be kept warm through a proxy without inventing an event type for it.
func (w *Writer) Comment(text string) (int, error) {
	return w.w.Write([]byte(": " + sanitizeField(text) + "\n\n"))
}

// Reader decodes a stream into events.
//
// It follows the WHATWG parsing rules that matter here: `field: value` with one
// optional leading space stripped, a line with no colon being a field with an
// empty value, a leading colon being a comment, data lines joined with newlines
// and a trailing newline removed, and a blank line dispatching.
type Reader struct {
	br    *bufio.Reader
	name  string
	id    string
	data  []byte
	retry int
	seen  bool
}

// NewReader wraps a source.
func NewReader(r io.Reader) *Reader { return &Reader{br: bufio.NewReader(r)} }

// readLine returns the next line, where a line ends at LF, CRLF **or a lone
// CR**.
//
// The lone CR is the part a naive `ReadString('\n')` gets wrong, and the
// fuzzer is what found it: `event:\r0\n\n` is two lines by the spec (an
// `event` field with an empty value, then a bare `0`), and one line with a
// carriage return inside its value if you only split on LF. The difference
// matters because a field value can then hold a CR that re-encodes into a
// different message — the format has no escapes, so a value that can contain a
// terminator is a value that can forge one.
func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		c, err := br.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		switch c {
		case '\n':
			return sb.String(), nil
		case '\r':
			// CRLF is one terminator, not two, or every line would be followed
			// by a phantom blank one and every event would dispatch early.
			if next, err := br.Peek(1); err == nil && next[0] == '\n' {
				_, _ = br.ReadByte()
			}
			return sb.String(), nil
		}
		sb.WriteByte(c)
	}
}

// Next returns the next dispatched event, or an error from the underlying
// stream. io.EOF at a clean boundary means the stream ended.
func (r *Reader) Next() (Event, error) {
	for {
		line, err := readLine(r.br)
		if line == "" && err != nil {
			return Event{}, err
		}

		if line == "" {
			if r.seen {
				ev := r.event()
				r.reset()
				return ev, nil
			}
			// A blank line with nothing buffered is a keep-alive boundary, not
			// an event.
			if err != nil {
				return Event{}, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			if err != nil {
				return Event{}, err
			}
			continue
		}

		field, value := line, ""
		if i := strings.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], strings.TrimPrefix(line[i+1:], " ")
		}
		switch field {
		case "event":
			r.name, r.seen = value, true
		case "id":
			r.id, r.seen = value, true
		case "data":
			if len(r.data) > 0 {
				r.data = append(r.data, '\n')
			}
			r.data = append(r.data, value...)
			r.seen = true
		case "retry":
			if n, e := strconv.Atoi(value); e == nil {
				r.retry, r.seen = n, true
			}
		}
		if err != nil {
			// The stream ended mid-event. Whatever was buffered is incomplete
			// and is deliberately not dispatched: half a JSON message read as a
			// whole one is the failure this format's blank-line terminator
			// exists to prevent.
			return Event{}, err
		}
	}
}

func (r *Reader) event() Event {
	return Event{Name: r.name, ID: r.id, Data: append([]byte(nil), r.data...), Retry: r.retry}
}

func (r *Reader) reset() {
	r.name, r.id, r.data, r.seen = "", "", r.data[:0], false
	r.retry = 0
}
