package ws

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The client end.
//
// magmux itself never opens a WebSocket, so nothing in the binary calls any of
// this. It exists because the end-to-end gate for this transport is "a real
// client watches a pane, types, and sees the character come back in a frame",
// and a test that drove the server through the server's own decoder would be
// testing one direction twice. The masking, the handshake and the
// must-not-be-masked rule are all direction-specific, and this is where the
// other direction lives.

// ClientConn is a client-side WebSocket: it masks what it sends, and refuses a
// masked frame from the server.
type ClientConn struct {
	conn net.Conn
	br   *bufio.Reader

	maxMessage int64
	fragOp     Opcode
	inFrag     bool
	frag       []byte

	wmu sync.Mutex
}

// ClientHandshake performs the client half of the handshake over an already
// connected net.Conn and returns the upgraded connection.
//
// It returns the server's response whenever one was parseable, INCLUDING a
// refusal: every credential decision magmux makes happens before the upgrade
// and arrives as an ordinary status, so a caller that discarded the response on
// error would throw away the 401 it needs to read.
func ClientHandshake(conn net.Conn, host, path string, hdr http.Header) (*ClientConn, *http.Response, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString(raw)

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	for name, values := range hdr {
		for _, v := range values {
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		return nil, nil, err
	}

	br := bufio.NewReader(conn)
	req, _ := http.NewRequest("GET", "http://"+host+path, nil)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, resp, fmt.Errorf("websocket: server answered %s", resp.Status)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != Accept(key) {
		return nil, resp, fmt.Errorf("websocket: Sec-WebSocket-Accept is %q, want %q", got, Accept(key))
	}
	return &ClientConn{conn: conn, br: br, maxMessage: DefaultMaxMessage}, resp, nil
}

// Write sends one masked message. A client frame that is not masked is a
// protocol error, so the mask is not optional and there is no way to skip it.
func (c *ClientConn) Write(op Opcode, payload []byte) error {
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i&3]
	}

	var hdr [14]byte
	hdr[0] = 0x80 | byte(op)
	n := len(payload)
	var hn int
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n)
		hn = 2
	case n < 1<<16:
		hdr[1] = 0x80 | 126
		hdr[2], hdr[3] = byte(n>>8), byte(n)
		hn = 4
	default:
		hdr[1] = 0x80 | 127
		for i := 0; i < 8; i++ {
			hdr[2+i] = byte(n >> (56 - 8*i))
		}
		hn = 10
	}
	copy(hdr[hn:hn+4], mask[:])
	hn += 4

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(hdr[:hn]); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err := c.conn.Write(masked)
	return err
}

// WriteText is the shape magmux's WebSocket API takes.
func (c *ClientConn) WriteText(s string) error { return c.Write(OpText, []byte(s)) }

// Read returns the next complete message, or the next control frame, with the
// same reassembly rules the server side applies.
func (c *ClientConn) Read() (Opcode, []byte, error) {
	for {
		f, err := ReadServerFrame(c.br, c.maxMessage)
		if err != nil {
			return 0, nil, err
		}
		if f.Opcode.IsControl() {
			return f.Opcode, f.Payload, nil
		}
		if f.Opcode == OpContinuation {
			if !c.inFrag {
				return 0, nil, protoErr(CloseProtocolError, "continuation with no message in progress")
			}
		} else {
			if c.inFrag {
				return 0, nil, protoErr(CloseProtocolError, "a new data frame inside a fragmented message")
			}
			c.inFrag, c.fragOp, c.frag = true, f.Opcode, nil
		}
		if int64(len(c.frag))+int64(len(f.Payload)) > c.maxMessage {
			return 0, nil, protoErr(CloseMessageTooBig, "message exceeds %d bytes", c.maxMessage)
		}
		c.frag = append(c.frag, f.Payload...)
		if !f.Fin {
			continue
		}
		op, msg := c.fragOp, c.frag
		c.inFrag, c.frag = false, nil
		if op == OpText && !utf8.Valid(msg) {
			return 0, nil, protoErr(CloseInvalidPayload, "a text message must be valid UTF-8")
		}
		return op, msg, nil
	}
}

// SetReadDeadline bounds the next Read, so a harness that is waiting for
// something that never arrives fails with a message instead of hanging.
func (c *ClientConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetWriteDeadline bounds the next Write.
func (c *ClientConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// Close sends a close frame and drops the connection.
func (c *ClientConn) Close() error {
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	_ = c.Write(OpClose, ClosePayload(CloseNormal, ""))
	return c.conn.Close()
}
