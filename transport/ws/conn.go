package ws

import (
	"bufio"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultMaxMessage is the reassembly cap: one message, however many
// continuation frames it arrived in. 1 MB matches the HTTP body limit, so a
// client cannot get a larger payload in by choosing the other transport.
const DefaultMaxMessage = 1 << 20

// Conn is one server-side WebSocket connection.
//
// It owns two things the pure codec deliberately does not: continuation
// reassembly with a cap, and a write mutex. The mutex is why magmux can answer
// a ping from the read goroutine while the hub's Sub writer is mid-message —
// the two serialise on it, and the write deadline bounds how long the control
// frame waits.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader

	maxMessage int64

	// fragOp is the opcode of the message being reassembled, or 0 when none is.
	// frag is what has arrived of it.
	fragOp   Opcode
	inFrag   bool
	frag     []byte
	gotClose bool

	// wmu serialises every write: the hub's Sub writer sending a data frame,
	// and the read goroutine or the pinger sending a control frame. A control
	// frame spliced into a data frame is not a WebSocket stream any more.
	wmu sync.Mutex
}

// NewConn wraps a hijacked connection. br must be the buffered reader the
// hijack returned: it may already hold bytes the client pipelined after the
// handshake, and reading past it would lose them.
func NewConn(c net.Conn, br *bufio.Reader, maxMessage int64) *Conn {
	if maxMessage <= 0 {
		maxMessage = DefaultMaxMessage
	}
	if br == nil {
		br = bufio.NewReader(c)
	}
	return &Conn{conn: c, br: br, maxMessage: maxMessage}
}

// NetConn is the underlying connection, for deadlines and for closing.
func (c *Conn) NetConn() net.Conn { return c.conn }

// ReadMessage returns the next COMPLETE message, or the next control frame.
//
// Data messages come back as OpText or OpBinary with the fragments already
// joined; control frames come back as themselves, because what to do about a
// ping, a pong or a close is the caller's policy and not the codec's.
//
// The rules enforced across frames, which ReadFrame cannot see on its own:
//
//   - a continuation with no message in progress is 1002;
//   - a new data frame while a message is in progress is 1002 (interleaving
//     data messages is what continuation frames exist to avoid);
//   - reassembly past maxMessage is 1009;
//   - a text message that is not valid UTF-8 is 1007, checked on the WHOLE
//     message, because a multi-byte rune may be split across two frames.
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	for {
		f, err := ReadFrame(c.br, c.maxMessage)
		if err != nil {
			return 0, nil, err
		}
		if f.Opcode.IsControl() {
			if f.Opcode == OpClose {
				c.gotClose = true
			}
			return f.Opcode, f.Payload, nil
		}
		switch {
		case f.Opcode == OpContinuation:
			if !c.inFrag {
				return 0, nil, protoErr(CloseProtocolError, "continuation frame with no message in progress")
			}
		default:
			if c.inFrag {
				return 0, nil, protoErr(CloseProtocolError, "a new %s frame arrived inside a fragmented message", f.Opcode)
			}
			c.inFrag = true
			c.fragOp = f.Opcode
			c.frag = c.frag[:0]
		}
		if int64(len(c.frag))+int64(len(f.Payload)) > c.maxMessage {
			return 0, nil, protoErr(CloseMessageTooBig,
				"message exceeds the %d-byte limit", c.maxMessage)
		}
		c.frag = append(c.frag, f.Payload...)
		if !f.Fin {
			continue
		}
		op, msg := c.fragOp, c.frag
		c.inFrag = false
		c.fragOp = 0
		// The buffer is handed out, so the next message starts a fresh one
		// rather than overwriting bytes the caller still holds.
		c.frag = nil
		if op == OpText && !utf8.Valid(msg) {
			return 0, nil, protoErr(CloseInvalidPayload, "a text message must be valid UTF-8")
		}
		return op, msg, nil
	}
}

// ReceivedClose reports whether the peer has sent a close frame. A close magmux
// sends after one it received is an ECHO and must not be followed by anything.
func (c *Conn) ReceivedClose() bool { return c.gotClose }

// WriteMessage writes one whole message as a single unfragmented frame, and
// reports how many bytes reached the connection.
//
// magmux never fragments what it sends. A frame is built in memory before it is
// written, so fragmenting would buy nothing but a second way for a write to
// fail halfway.
func (c *Conn) WriteMessage(op Opcode, payload []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	cw := &countingWriter{w: c.conn}
	_, err := WriteFrame(cw, true, op, payload)
	return cw.n, err
}

// WriteControl writes a ping, a pong or a close. It takes the same lock as
// WriteMessage, so a control frame is never spliced into a data frame; its own
// deadline is the connection's, which the caller sets.
func (c *Conn) WriteControl(op Opcode, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := WriteFrame(c.conn, true, op, payload)
	return err
}

// WriteClose sends a close frame with a code and a reason.
func (c *Conn) WriteClose(code int, reason string) error {
	return c.WriteControl(OpClose, ClosePayload(code, reason))
}

// SetWriteDeadline bounds the write in progress and every write after it, and
// interrupts one that is ALREADY blocked. That is the net.Conn rule the hub's
// Finalize depends on to cut a stalled peer short.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// SetReadDeadline bounds the next read. The ping loop uses it to notice a peer
// that has stopped answering.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close drops the connection without a close frame. The orderly path is
// WriteClose followed by this.
func (c *Conn) Close() error { return c.conn.Close() }

// countingWriter reports how much of a multi-part write reached the wire. The
// hub needs it to tell a refused write (nothing left the process, a final may
// still follow) from a torn one (a partial frame is out there, and nothing may
// follow it).
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}
