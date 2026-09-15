package ws

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// maskInto builds a client frame: header, mask key, masked payload. Every test
// input goes through it, because an unmasked client frame is a protocol error
// and would make every case fail for the same uninteresting reason.
func clientFrame(fin bool, op Opcode, payload []byte) []byte {
	var b bytes.Buffer
	h0 := byte(op)
	if fin {
		h0 |= 0x80
	}
	b.WriteByte(h0)
	n := len(payload)
	switch {
	case n < 126:
		b.WriteByte(0x80 | byte(n))
	case n < 1<<16:
		b.WriteByte(0x80 | 126)
		b.WriteByte(byte(n >> 8))
		b.WriteByte(byte(n))
	default:
		b.WriteByte(0x80 | 127)
		for s := 56; s >= 0; s -= 8 {
			b.WriteByte(byte(n >> s))
		}
	}
	mask := []byte{0x37, 0xfa, 0x21, 0x3d}
	b.Write(mask)
	for i, c := range payload {
		b.WriteByte(c ^ mask[i&3])
	}
	return b.Bytes()
}

// TestAcceptMatchesTheRFCVector pins the handshake against RFC 6455 §1.3's own
// example. A wrong accept is the one handshake bug a browser reports as a
// generic failure with nothing to look at.
func TestAcceptMatchesTheRFCVector(t *testing.T) {
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := Accept(key); got != want {
		t.Errorf("Accept(%q) = %q, want %q", key, got, want)
	}
}

// TestFrameCodecTable is the read side: every rule ReadFrame can check on a
// frame in isolation, with the close code each must be reported with.
func TestFrameCodecTable(t *testing.T) {
	long := bytes.Repeat([]byte("x"), 200)
	huge := bytes.Repeat([]byte("y"), 70000)

	for _, tc := range []struct {
		name    string
		in      []byte
		max     int64
		want    Frame
		wantErr int // 0 = must succeed
	}{
		{
			name: "a short masked text frame",
			in:   clientFrame(true, OpText, []byte("hello")),
			want: Frame{Fin: true, Opcode: OpText, Payload: []byte("hello")},
		},
		{
			name: "an empty frame",
			in:   clientFrame(true, OpText, nil),
			want: Frame{Fin: true, Opcode: OpText, Payload: []byte{}},
		},
		{
			name: "a 16-bit length",
			in:   clientFrame(true, OpBinary, long),
			want: Frame{Fin: true, Opcode: OpBinary, Payload: long},
		},
		{
			name: "a 64-bit length",
			in:   clientFrame(true, OpBinary, huge),
			max:  1 << 20,
			want: Frame{Fin: true, Opcode: OpBinary, Payload: huge},
		},
		{
			name: "a non-final fragment",
			in:   clientFrame(false, OpText, []byte("ab")),
			want: Frame{Fin: false, Opcode: OpText, Payload: []byte("ab")},
		},
		{
			name: "a ping",
			in:   clientFrame(true, OpPing, []byte("p")),
			want: Frame{Fin: true, Opcode: OpPing, Payload: []byte("p")},
		},
		{
			name:    "an unmasked client frame",
			in:      []byte{0x81, 0x01, 'a'},
			wantErr: CloseProtocolError,
		},
		{
			name:    "RSV1 set with no extension negotiated",
			in:      append([]byte{0xC1}, clientFrame(true, OpText, []byte("a"))[1:]...),
			wantErr: CloseProtocolError,
		},
		{
			name:    "a reserved opcode",
			in:      clientFrame(true, Opcode(0x3), []byte("a")),
			wantErr: CloseProtocolError,
		},
		{
			name:    "a reserved control opcode",
			in:      clientFrame(true, Opcode(0xB), nil),
			wantErr: CloseProtocolError,
		},
		{
			name:    "a fragmented control frame",
			in:      clientFrame(false, OpPing, []byte("a")),
			wantErr: CloseProtocolError,
		},
		{
			name:    "an oversized control frame",
			in:      clientFrame(true, OpPing, bytes.Repeat([]byte("z"), 126)),
			wantErr: CloseProtocolError,
		},
		{
			name:    "a non-minimal 16-bit length",
			in:      []byte{0x81, 0xFE, 0x00, 0x05, 0, 0, 0, 0, 'h', 'e', 'l', 'l', 'o'},
			wantErr: CloseProtocolError,
		},
		{
			name: "a non-minimal 64-bit length",
			in: []byte{0x82, 0xFF, 0, 0, 0, 0, 0, 0, 0x00, 0x05,
				0, 0, 0, 0, 'h', 'e', 'l', 'l', 'o'},
			wantErr: CloseProtocolError,
		},
		{
			name:    "the high bit of a 64-bit length",
			in:      []byte{0x82, 0xFF, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			wantErr: CloseProtocolError,
		},
		{
			name:    "a frame larger than the limit",
			in:      clientFrame(true, OpBinary, long),
			max:     64,
			wantErr: CloseMessageTooBig,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			max := tc.max
			if max == 0 {
				max = 1 << 20
			}
			f, err := ReadFrame(bufio.NewReader(bytes.NewReader(tc.in)), max)
			if tc.wantErr != 0 {
				if err == nil {
					t.Fatalf("expected close %d, got frame %+v", tc.wantErr, f)
				}
				if got := CloseCodeOf(err); got != tc.wantErr {
					t.Fatalf("close code = %d, want %d (%v)", got, tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if f.Fin != tc.want.Fin || f.Opcode != tc.want.Opcode || !bytes.Equal(f.Payload, tc.want.Payload) {
				t.Fatalf("got fin=%v op=%v len=%d, want fin=%v op=%v len=%d",
					f.Fin, f.Opcode, len(f.Payload), tc.want.Fin, tc.want.Opcode, len(tc.want.Payload))
			}
		})
	}
}

// TestOversizedFrameIsRefusedWithoutReadingIt is the allocation half of the
// 1009 rule: a peer that ANNOUNCES two gigabytes must not make magmux reserve
// them. The frame body is never sent, so a reader that tried to consume it
// would block forever rather than return.
func TestOversizedFrameIsRefusedWithoutReadingIt(t *testing.T) {
	// Header only: FIN+binary, masked, 64-bit length of 2 GB. No mask key and
	// no payload follow.
	hdr := []byte{0x82, 0xFF, 0, 0, 0, 0, 0x80, 0, 0, 0}
	f, err := ReadFrame(bufio.NewReader(bytes.NewReader(hdr)), 1<<20)
	if err == nil {
		t.Fatalf("expected a refusal, got %+v", f)
	}
	if CloseCodeOf(err) != CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d", CloseCodeOf(err), CloseMessageTooBig)
	}
}

// TestWriteFrameLengthEncodings pins the write side, including the byte count
// the hub's torn-write rule reads.
func TestWriteFrameLengthEncodings(t *testing.T) {
	for _, n := range []int{0, 1, 125, 126, 65535, 65536} {
		payload := bytes.Repeat([]byte("a"), n)
		var buf bytes.Buffer
		written, err := WriteFrame(&buf, true, OpText, payload)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if written != buf.Len() {
			t.Errorf("n=%d: reported %d bytes, buffer holds %d", n, written, buf.Len())
		}
		// A server frame must NOT be masked.
		if buf.Bytes()[1]&0x80 != 0 {
			t.Errorf("n=%d: a server frame must not set the mask bit", n)
		}
		var wantHdr int
		switch {
		case n < 126:
			wantHdr = 2
		case n < 1<<16:
			wantHdr = 4
		default:
			wantHdr = 10
		}
		if buf.Len() != wantHdr+n {
			t.Errorf("n=%d: frame is %d bytes, want %d header + %d payload", n, buf.Len(), wantHdr, n)
		}
	}
}

// TestCloseCodecRoundTrip covers §5.5.1's three cases and the two different
// ways a close body can be wrong.
func TestCloseCodecRoundTrip(t *testing.T) {
	code, reason, err := ParseClose(ClosePayload(CloseGoingAway, "shutdown"))
	if err != nil || code != CloseGoingAway || reason != "shutdown" {
		t.Fatalf("round trip = %d, %q, %v", code, reason, err)
	}
	if code, _, err := ParseClose(nil); err != nil || code != CloseNoStatus {
		t.Errorf("an empty close body is legal and means no status: %d, %v", code, err)
	}
	if _, _, err := ParseClose([]byte{0x03}); CloseCodeOf(err) != CloseProtocolError {
		t.Errorf("a one-byte close body must be 1002, got %v", err)
	}
	for _, bad := range []int{999, 1004, 1005, 1006, 1016, 2999, 5000} {
		if _, _, err := ParseClose([]byte{byte(bad >> 8), byte(bad)}); err == nil {
			t.Errorf("close code %d must be refused on the wire", bad)
		}
	}
	for _, ok := range []int{1000, 1001, 1002, 1003, 1007, 1008, 1009, 1011, 1013, 3000, 4999} {
		if _, _, err := ParseClose([]byte{byte(ok >> 8), byte(ok)}); err != nil {
			t.Errorf("close code %d must be accepted: %v", ok, err)
		}
	}
	if _, _, err := ParseClose(append([]byte{0x03, 0xE8}, 0xff, 0xfe)); CloseCodeOf(err) != CloseInvalidPayload {
		t.Errorf("an invalid-UTF-8 close reason must be 1007, got %v", err)
	}
	// A long reason is truncated to fit the 125-byte control frame.
	p := ClosePayload(CloseNormal, strings.Repeat("é", 200))
	if len(p) > 125 {
		t.Errorf("close payload is %d bytes; a control frame is 125 at most", len(p))
	}
	if _, _, err := ParseClose(p); err != nil {
		t.Errorf("truncation must land on a rune boundary: %v", err)
	}
}

// TestConnReassembly is everything ReadFrame cannot see on its own.
func TestConnReassembly(t *testing.T) {
	feed := func(max int64, frames ...[]byte) *Conn {
		var b bytes.Buffer
		for _, f := range frames {
			b.Write(f)
		}
		return NewConn(nopConn{}, bufio.NewReader(&b), max)
	}

	t.Run("fragments are joined", func(t *testing.T) {
		c := feed(0,
			clientFrame(false, OpText, []byte("he")),
			clientFrame(false, OpContinuation, []byte("ll")),
			clientFrame(true, OpContinuation, []byte("o")))
		op, msg, err := c.ReadMessage()
		if err != nil || op != OpText || string(msg) != "hello" {
			t.Fatalf("= %v, %q, %v", op, msg, err)
		}
	})

	t.Run("a control frame may interleave", func(t *testing.T) {
		c := feed(0,
			clientFrame(false, OpText, []byte("he")),
			clientFrame(true, OpPing, []byte("p")),
			clientFrame(true, OpContinuation, []byte("llo")))
		if op, _, err := c.ReadMessage(); err != nil || op != OpPing {
			t.Fatalf("first = %v, %v; want a ping", op, err)
		}
		if op, msg, err := c.ReadMessage(); err != nil || op != OpText || string(msg) != "hello" {
			t.Fatalf("second = %v, %q, %v", op, msg, err)
		}
	})

	t.Run("a continuation with nothing in progress", func(t *testing.T) {
		c := feed(0, clientFrame(true, OpContinuation, []byte("x")))
		if _, _, err := c.ReadMessage(); CloseCodeOf(err) != CloseProtocolError {
			t.Fatalf("want 1002, got %v", err)
		}
	})

	t.Run("a new data frame inside a fragmented message", func(t *testing.T) {
		// ReadMessage loops until a message is complete, so the violation is
		// reported by the FIRST call: there is no complete message to hand back
		// before it.
		c := feed(0,
			clientFrame(false, OpText, []byte("a")),
			clientFrame(true, OpText, []byte("b")))
		if _, _, err := c.ReadMessage(); CloseCodeOf(err) != CloseProtocolError {
			t.Fatalf("want 1002, got %v", err)
		}
	})

	t.Run("reassembly past the cap", func(t *testing.T) {
		// Each frame fits; the JOIN does not. That is the case a per-frame
		// check cannot catch, which is why the cap is also applied here.
		c := feed(80,
			clientFrame(false, OpBinary, bytes.Repeat([]byte("a"), 50)),
			clientFrame(true, OpContinuation, bytes.Repeat([]byte("b"), 50)))
		if _, _, err := c.ReadMessage(); CloseCodeOf(err) != CloseMessageTooBig {
			t.Fatalf("want 1009, got %v", err)
		}
	})

	t.Run("invalid UTF-8 split across fragments", func(t *testing.T) {
		// The two halves of one rune. Validating per frame would call the first
		// half invalid and the whole message is fine; validating never would
		// let a truly broken message through. It is checked on the JOIN.
		ok := feed(0,
			clientFrame(false, OpText, []byte("\xc3")),
			clientFrame(true, OpContinuation, []byte("\xa9")))
		if _, msg, err := ok.ReadMessage(); err != nil || string(msg) != "é" {
			t.Fatalf("a rune split across frames must survive: %q, %v", msg, err)
		}
		bad := feed(0, clientFrame(true, OpText, []byte{0xff, 0xfe}))
		if _, _, err := bad.ReadMessage(); CloseCodeOf(err) != CloseInvalidPayload {
			t.Fatalf("want 1007, got %v", err)
		}
	})

	t.Run("binary is not UTF-8 checked", func(t *testing.T) {
		c := feed(0, clientFrame(true, OpBinary, []byte{0xff, 0xfe}))
		if op, _, err := c.ReadMessage(); err != nil || op != OpBinary {
			t.Fatalf("= %v, %v", op, err)
		}
	})

	t.Run("a close is remembered", func(t *testing.T) {
		c := feed(0, clientFrame(true, OpClose, ClosePayload(CloseNormal, "bye")))
		if c.ReceivedClose() {
			t.Fatal("nothing has been read yet")
		}
		if op, _, err := c.ReadMessage(); err != nil || op != OpClose {
			t.Fatalf("= %v, %v", op, err)
		}
		if !c.ReceivedClose() {
			t.Fatal("a close must be remembered, so magmux's own close is an echo and nothing follows it")
		}
	})

	t.Run("a truncated frame is an ordinary read error", func(t *testing.T) {
		full := clientFrame(true, OpText, []byte("hello"))
		c := feed(0, full[:len(full)-2])
		_, _, err := c.ReadMessage()
		if err == nil {
			t.Fatal("expected an error")
		}
		if CloseCodeOf(err) != 0 {
			t.Fatalf("a truncated stream is not a protocol violation, got close %d", CloseCodeOf(err))
		}
	})
}

// TestCheckUpgradeRejectsEveryMalformedHandshake.
func TestCheckUpgradeRejectsEveryMalformedHandshake(t *testing.T) {
	good := func() *http.Request {
		r, _ := http.NewRequest("GET", "http://x/v1/ws", nil)
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		return r
	}
	if err := CheckUpgrade(good()); err != nil {
		t.Fatalf("a valid handshake was refused: %v", err)
	}

	for _, tc := range []struct {
		name  string
		mutfn func(*http.Request)
	}{
		{"not a GET", func(r *http.Request) { r.Method = "POST" }},
		{"no Connection: upgrade", func(r *http.Request) { r.Header.Set("Connection", "keep-alive") }},
		{"wrong Upgrade", func(r *http.Request) { r.Header.Set("Upgrade", "h2c") }},
		{"wrong version", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }},
		{"no key", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }},
		{"a key that is not base64", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "!!!") }},
		{"a key of the wrong length", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "YWJj") }},
	} {
		r := good()
		tc.mutfn(r)
		if err := CheckUpgrade(r); err == nil {
			t.Errorf("%s: must be refused", tc.name)
		}
	}
}

// TestSubprotocolCarriesTheTokenAndIsNeverEchoedBack is the browser credential
// channel, and the rule that the response must not repeat it.
func TestSubprotocolCarriesTheTokenAndIsNeverEchoedBack(t *testing.T) {
	h := http.Header{}
	h.Add("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth.SECRETTOKEN")
	if !OffersSubprotocol(h) {
		t.Error("magmux.v1 was offered")
	}
	if got := TokenFromSubprotocols(h); got != "SECRETTOKEN" {
		t.Errorf("token = %q", got)
	}

	// Split across two headers, which is equally legal.
	h2 := http.Header{}
	h2.Add("Sec-WebSocket-Protocol", "magmux.v1")
	h2.Add("Sec-WebSocket-Protocol", "magmux.auth.OTHER")
	if !OffersSubprotocol(h2) || TokenFromSubprotocols(h2) != "OTHER" {
		t.Error("a client may split Sec-WebSocket-Protocol across headers")
	}

	var buf bytes.Buffer
	if err := WriteHandshake(&buf, "dGhlIHNhbXBsZSBub25jZQ==", Subprotocol); err != nil {
		t.Fatal(err)
	}
	resp := buf.String()
	if !strings.Contains(resp, "HTTP/1.1 101 Switching Protocols\r\n") {
		t.Errorf("bad status line:\n%s", resp)
	}
	if !strings.Contains(resp, "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n") {
		t.Errorf("bad accept:\n%s", resp)
	}
	if !strings.Contains(resp, "Sec-WebSocket-Protocol: magmux.v1\r\n") {
		t.Errorf("magmux.v1 must be echoed:\n%s", resp)
	}
	if strings.Contains(resp, "SECRETTOKEN") || strings.Contains(resp, "magmux.auth.") {
		t.Errorf("the credential must NEVER be echoed into a response header:\n%s", resp)
	}
	if !strings.HasSuffix(resp, "\r\n\r\n") {
		t.Errorf("the handshake must end with a blank line:\n%q", resp)
	}
}

// nopConn is a net.Conn that does nothing, for the reader-only Conn tests.
type nopConn struct{ net.Conn }

func (nopConn) Write(p []byte) (int, error) { return len(p), nil }
func (nopConn) Close() error                { return nil }
func (nopConn) Read([]byte) (int, error)    { return 0, io.EOF }
