package mux

// A socket client that hangs up before magmux has written to it.
//
// The connect-time aggregate is the first thing magmux writes, and against a
// client that has already closed that write fails with EPIPE. The writer used
// to answer by closing the whole fd, and when that landed before the reader's
// first Read the lines the client had sent — already in the kernel's receive
// buffer — were thrown away unread. TestOneShotSendSurvivesClose hit it about
// once in two hundred runs; the test here makes the order certain.

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// closeRecorder is a real unix connection that remembers how magmux ended it.
// CloseWrite is promoted from the embedded *net.UnixConn and shadowed here, so
// the sink still sees a conn that can half-close.
type closeRecorder struct {
	*net.UnixConn
	ended      chan struct{} // closed on the first Close OR CloseWrite
	endOnce    sync.Once
	closed     chan struct{} // closed on the first full Close
	closeOnce  sync.Once
	halfClosed atomic.Bool
}

func newCloseRecorder(c *net.UnixConn) *closeRecorder {
	return &closeRecorder{UnixConn: c, ended: make(chan struct{}), closed: make(chan struct{})}
}

func (c *closeRecorder) end() { c.endOnce.Do(func() { close(c.ended) }) }

func (c *closeRecorder) Close() error {
	c.end()
	c.closeOnce.Do(func() { close(c.closed) })
	return c.UnixConn.Close()
}

func (c *closeRecorder) CloseWrite() error {
	c.halfClosed.Store(true)
	c.end()
	return c.UnixConn.CloseWrite()
}

// unixPair returns the two ends of one real unix-socket connection: the
// server's (as Accept gave it) and the client's.
func unixPair(t *testing.T) (srv, cli *net.UnixConn) {
	t.Helper()
	path := filepath.Join(sockTestDir(t), "p.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	s, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return s.(*net.UnixConn), c.(*net.UnixConn)
}

// pipePaneMux is sockMux without the listener: one pane whose PTY is a pipe,
// so the test can hand handleSocketConn a connection whose peer state it has
// already fixed.
func pipePaneMux(t *testing.T) (*Magmux, *os.File) {
	t.Helper()
	m := &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel()}
	p := newScrollPane(20, 60)
	childR, childW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { childR.Close(); childW.Close() })
	p.ptmx = childW
	m.root = p
	m.focused = p
	m.allPanes = []*Pane{p}
	m.stampPaneIDs()
	return m, childR
}

// TestOneShotSendSurvivesAggregateWriteFailure pins the order that lost
// instructions: the client writes its lines and hangs up, the aggregate write
// fails with EPIPE, the writer ends the connection, and only THEN does the
// reader make its first Read. Every line the client sent before hanging up must
// still be read and admitted, and the fd must still be released afterwards.
func TestOneShotSendSurvivesAggregateWriteFailure(t *testing.T) {
	m, child := pipePaneMux(t)
	srv, cli := unixPair(t)
	rec := newCloseRecorder(srv)

	// The reader waits until the writer has ended the connection in whatever
	// way it ends it. Without that wait the reader usually wins, which is why
	// the bug hid.
	m.sockBeforeRead = func(net.Conn) {
		select {
		case <-rec.ended:
		case <-time.After(5 * time.Second):
			t.Errorf("the aggregate write against a closed peer never ended the connection")
		}
	}

	// The whole one-shot client, finished before magmux writes a byte: so the
	// aggregate write is guaranteed to meet a closed peer.
	writeLines(t, cli,
		map[string]any{"type": "send", "pane": 0, "text": "first"},
		map[string]any{"type": "send", "pane": 0, "text": "second"},
	)
	cli.Close()

	done := make(chan struct{})
	go func() { m.handleSocketConn(rec); close(done) }()

	got := childSaw(t, child, "second\r", 40*pilotSendDelay)
	if got != "first\rsecond\r" {
		t.Fatalf("the PTY saw %q, want both instructions in order", got)
	}
	// No fd leak: once the reader has drained the peer's bytes, the server end
	// is fully closed, not left half-closed.
	select {
	case <-rec.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the server end was never fully closed after the peer hung up (fd leak)")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleSocketConn never returned after the peer hung up")
	}
}

// epipeSink returns a sockSink over a connection whose peer has already gone,
// having checked that a Write meets EPIPE, which is what the aggregate meets.
func epipeSink(t *testing.T) (*sockSink, *closeRecorder) {
	t.Helper()
	srv, cli := unixPair(t)
	cli.Close()
	rec := newCloseRecorder(srv)
	s := &sockSink{conn: rec}
	if _, err := s.Write([]byte("{}\n")); err == nil || !s.peerGone.Load() {
		t.Fatalf("a write to a closed peer returned %v, want EPIPE", err)
	}
	return s, rec
}

func isClosed(c *closeRecorder) bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// TestSockSinkEPIPEHalfClosesUntilTheReaderEnds: the writer's Close after EPIPE
// must leave the fd to the reader, and the reader's end must release it.
func TestSockSinkEPIPEHalfClosesUntilTheReaderEnds(t *testing.T) {
	s, rec := epipeSink(t)
	s.Close("write failed")
	if !rec.halfClosed.Load() || isClosed(rec) {
		t.Fatalf("after EPIPE Close must only CloseWrite while the reader runs (half=%v closed=%v)",
			rec.halfClosed.Load(), isClosed(rec))
	}
	s.readerFinished()
	if !isClosed(rec) {
		t.Fatal("the reader ended and nobody closed the fd (fd leak)")
	}
}

// TestSockSinkEPIPEAfterTheReaderEndedClosesTheFD is the leak the other order
// would open: the reader reaches EOF first, and only a LATER write (a queued
// reply) meets EPIPE. Nobody is left to read, so that Close must be a full one.
func TestSockSinkEPIPEAfterTheReaderEndedClosesTheFD(t *testing.T) {
	srv, cli := unixPair(t)
	rec := newCloseRecorder(srv)
	s := &sockSink{conn: rec}
	s.readerFinished()
	if isClosed(rec) {
		t.Fatal("the reader ending must not close a connection the writer is still using")
	}
	cli.Close()
	if _, err := s.Write([]byte("{}\n")); err == nil {
		t.Fatal("a write to a closed peer succeeded")
	}
	s.Close("write failed")
	if !isClosed(rec) {
		t.Fatal("EPIPE after the reader ended left the fd open (fd leak)")
	}
}

// TestSockSinkLivePeerFailureStillClosesTheFD: a peer that is alive and not
// reading (a deadline, slow_consumer, Finalize's cut) is still ended outright,
// which is the only way to stop it holding the connection.
func TestSockSinkLivePeerFailureStillClosesTheFD(t *testing.T) {
	srv, _ := unixPair(t)
	rec := newCloseRecorder(srv)
	s := &sockSink{conn: rec}
	_ = s.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := s.Write([]byte("{}\n")); err == nil || s.peerGone.Load() {
		t.Fatalf("an expired deadline gave err=%v peerGone=%v, want a timeout that is not EPIPE",
			err, s.peerGone.Load())
	}
	s.Close("write failed")
	if rec.halfClosed.Load() || !isClosed(rec) {
		t.Fatalf("a live peer's failure must be a full Close (half=%v closed=%v)",
			rec.halfClosed.Load(), isClosed(rec))
	}
}
