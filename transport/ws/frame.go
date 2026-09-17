package ws

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Opcode is a frame's type (RFC 6455 §5.2).
type Opcode byte

// The opcodes magmux handles. Everything else is a protocol error: the reserved
// ranges (0x3-0x7, 0xB-0xF) exist for extensions, and magmux negotiates none,
// so a frame carrying one is a peer talking to a different server.
const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA
)

// IsControl reports whether op is a control frame (§5.5): never fragmented,
// never longer than 125 bytes, and allowed to arrive in the middle of a
// fragmented data message.
func (o Opcode) IsControl() bool { return o&0x8 != 0 }

func (o Opcode) String() string {
	switch o {
	case OpContinuation:
		return "continuation"
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpClose:
		return "close"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	}
	return fmt.Sprintf("opcode(0x%x)", byte(o))
}

// Close codes. The ones magmux actually sends are Normal, GoingAway (teardown),
// ProtocolError, InvalidPayload, PolicyViolation (a Sub refused for policy),
// MessageTooBig and TryAgainLater (slow_consumer).
//
// Auth NEVER appears here. Every authentication and authorisation decision is
// made before the upgrade, as an HTTP 401 or 403, so a client never has to read
// a close code to learn its credential was wrong — and a browser cannot read
// one reliably anyway.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseUnsupportedData = 1003
	CloseNoStatus        = 1005 // never sent, never received on the wire
	CloseAbnormal        = 1006 // never sent, never received on the wire
	CloseInvalidPayload  = 1007
	ClosePolicyViolation = 1008
	CloseMessageTooBig   = 1009
	CloseInternalError   = 1011
	CloseTryAgainLater   = 1013
)

// Error is a protocol failure that already knows how it must be reported.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("websocket: %s (close %d)", e.Msg, e.Code) }

func protoErr(code int, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// CloseCodeOf extracts the close code an error should be reported with. An
// error that is not a protocol failure — a read that timed out, a peer that
// vanished — has none, and returns 0.
func CloseCodeOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// Frame is one frame off the wire, already unmasked.
type Frame struct {
	Fin     bool
	Opcode  Opcode
	Payload []byte
}

// ReadFrame reads exactly one frame from r, applying every server-side rule in
// §5 that can be checked on a frame in isolation.
//
// max bounds ONE frame's payload. A frame past it is refused with 1009 WITHOUT
// reading the payload, which is the point: a peer that announces a 2 GB frame
// must not be able to make magmux allocate for it.
//
// Refused here:
//   - any RSV bit set (1002): they mean an extension, and magmux negotiates none
//   - an opcode outside the six above (1002)
//   - an unmasked frame (1002): every client-to-server frame MUST be masked
//   - a control frame that is fragmented or longer than 125 bytes (1002)
//   - a non-minimal length encoding (1002): 126 used for a length under 126, or
//     127 for one under 65536, is a peer that is not speaking the framing
//   - a 64-bit length with the high bit set (1002)
func ReadFrame(r io.Reader, max int64) (Frame, error) {
	return readFrame(r, max, true)
}

// ReadServerFrame is ReadFrame with the masking rule inverted: a SERVER frame
// must NOT be masked (§5.1). It exists for the client end — magmux's own
// end-to-end harness — and is the one place the direction of the rule differs,
// so it is a parameter rather than a second parser.
func ReadServerFrame(r io.Reader, max int64) (Frame, error) {
	return readFrame(r, max, false)
}

func readFrame(r io.Reader, max int64, wantMask bool) (Frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	fin := hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		return Frame{}, protoErr(CloseProtocolError, "RSV bits are set and no extension was negotiated")
	}
	op := Opcode(hdr[0] & 0x0F)
	switch op {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
	default:
		return Frame{}, protoErr(CloseProtocolError, "reserved opcode 0x%x", byte(op))
	}
	masked := hdr[1]&0x80 != 0
	if masked != wantMask {
		if wantMask {
			return Frame{}, protoErr(CloseProtocolError, "a client frame must be masked")
		}
		return Frame{}, protoErr(CloseProtocolError, "a server frame must not be masked")
	}

	n := int64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
		if n < 126 {
			return Frame{}, protoErr(CloseProtocolError, "length %d is not minimally encoded", n)
		}
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u&(1<<63) != 0 {
			return Frame{}, protoErr(CloseProtocolError, "the high bit of a 64-bit length must be 0")
		}
		n = int64(u)
		if n < 1<<16 {
			return Frame{}, protoErr(CloseProtocolError, "length %d is not minimally encoded", n)
		}
	}

	if op.IsControl() {
		if !fin {
			return Frame{}, protoErr(CloseProtocolError, "a %s frame must not be fragmented", op)
		}
		if n > 125 {
			return Frame{}, protoErr(CloseProtocolError, "a %s frame must be 125 bytes or fewer (got %d)", op, n)
		}
	}
	if max > 0 && n > max {
		// Refused BEFORE the payload is read, so the announced length cannot
		// make magmux allocate. The connection is finished either way: the
		// caller sends 1009 and closes rather than trying to skip n bytes it has
		// just declined to trust.
		return Frame{}, protoErr(CloseMessageTooBig, "frame of %d bytes exceeds the %d-byte limit", n, max)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return Frame{}, err
		}
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i&3]
			}
		}
	}
	return Frame{Fin: fin, Opcode: op, Payload: payload}, nil
}

// WriteFrame writes one server frame. Server frames are never masked (§5.1), so
// there is no mask parameter and no way to add one by mistake.
//
// It returns the number of bytes actually handed to w, which is what the hub's
// Sink contract needs: a partial write means bytes are on the wire and no final
// may follow them.
func WriteFrame(w io.Writer, fin bool, op Opcode, payload []byte) (int, error) {
	var hdr [10]byte
	hdr[0] = byte(op)
	if fin {
		hdr[0] |= 0x80
	}
	n := len(payload)
	var hn int
	switch {
	case n < 126:
		hdr[1] = byte(n)
		hn = 2
	case n < 1<<16:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hn = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hn = 10
	}
	written, err := w.Write(hdr[:hn])
	if err != nil {
		return written, err
	}
	if n == 0 {
		return written, nil
	}
	m, err := w.Write(payload)
	return written + m, err
}

// ClosePayload builds a close frame's body: the code, then a UTF-8 reason
// truncated to fit the 125-byte control-frame limit.
func ClosePayload(code int, reason string) []byte {
	if code == 0 {
		return nil
	}
	b := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(b, uint16(code))
	// 123 = 125 minus the two code bytes. Truncated on a rune boundary, because
	// a close reason that is not valid UTF-8 is itself a protocol error and the
	// peer would be right to answer 1007.
	for len(reason) > 123 {
		_, size := utf8.DecodeLastRuneInString(reason)
		reason = reason[:len(reason)-size]
	}
	return append(b, reason...)
}

// ParseClose reads a close frame's body (§5.5.1).
//
// An empty body is "no status", which is legal. A one-byte body is not. A code
// outside the ranges a peer may send, or a reason that is not valid UTF-8, is a
// protocol error, and the two are reported differently — 1002 for the code,
// 1007 for the text — because they say different things about the peer.
func ParseClose(payload []byte) (code int, reason string, err error) {
	switch {
	case len(payload) == 0:
		return CloseNoStatus, "", nil
	case len(payload) == 1:
		return 0, "", protoErr(CloseProtocolError, "a close frame body must be empty or at least 2 bytes")
	}
	code = int(binary.BigEndian.Uint16(payload[:2]))
	if !validPeerCloseCode(code) {
		return 0, "", protoErr(CloseProtocolError, "close code %d is not one a peer may send", code)
	}
	reason = string(payload[2:])
	if !utf8.ValidString(reason) {
		return 0, "", protoErr(CloseInvalidPayload, "a close reason must be valid UTF-8")
	}
	return code, reason, nil
}

// validPeerCloseCode is the set a peer is allowed to put ON THE WIRE. 1005 and
// 1006 are library-internal values for "no code" and "connection dropped" and
// are explicitly not sendable; 1015 is TLS-internal. 3000-4999 are registered
// and private use.
func validPeerCloseCode(c int) bool {
	switch {
	case c >= 3000 && c <= 4999:
		return true
	case c >= 1000 && c <= 1003:
		return true
	case c >= 1007 && c <= 1014:
		return true
	}
	return false
}
