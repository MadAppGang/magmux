package ws

import (
	"bufio"
	"bytes"
	"testing"
)

// FuzzWSReadFrame drives the whole frame parser from arbitrary bytes.
//
// It exists because this is a HAND-WRITTEN parser of a binary format on the
// far side of an authentication boundary, reading lengths a peer chose. The
// three failure modes that matter are exactly the three a fuzzer finds and a
// table test does not: a panic on a slice bound, an allocation sized by an
// attacker's 64-bit length field, and a frame accepted that should have been
// refused.
//
// The invariants asserted on every input:
//
//   - ReadFrame never panics, whatever the bytes.
//   - A frame it ACCEPTS never exceeds the limit it was given, so the cap is
//     enforced on the path that allocates and not merely near it.
//   - An accepted frame's opcode is one of the six magmux handles, and a
//     control frame is final and short — the rules that let the caller assume
//     it never has to fragment a pong.
//   - An accepted frame RE-ENCODES to the same fin/opcode/payload, so the
//     decoder and the encoder cannot drift apart silently.
//   - An error either carries a close code magmux may send, or is a plain I/O
//     error from a truncated stream. Nothing is left for a caller to guess.
func FuzzWSReadFrame(f *testing.F) {
	// Seeds: one of every shape the table test covers, so the corpus starts
	// somewhere the parser actually accepts and the fuzzer mutates outward.
	f.Add(clientFrame(true, OpText, []byte("hello")), int64(1<<20))
	f.Add(clientFrame(true, OpText, nil), int64(1<<20))
	f.Add(clientFrame(false, OpBinary, bytes.Repeat([]byte("x"), 200)), int64(1<<20))
	f.Add(clientFrame(true, OpPing, []byte("p")), int64(1<<20))
	f.Add(clientFrame(true, OpClose, ClosePayload(CloseNormal, "bye")), int64(1<<20))
	f.Add(clientFrame(true, OpContinuation, []byte("more")), int64(64))
	f.Add([]byte{0x81, 0x01, 'a'}, int64(1<<20))         // unmasked
	f.Add([]byte{0xC1, 0x81, 0, 0, 0, 0, 0}, int64(128)) // RSV set
	f.Add([]byte{0x82, 0xFF, 0x80, 0, 0, 0, 0, 0, 0, 0}, int64(1<<20))
	f.Add([]byte{}, int64(1<<20))
	f.Add([]byte{0x88}, int64(1<<20))

	f.Fuzz(func(t *testing.T, data []byte, max int64) {
		// Bound the limit itself. A negative or absurd max is not a peer input
		// — magmux always passes MaxMessage — and letting the fuzzer pick
		// 1<<62 would only test make()'s own OOM behaviour.
		if max <= 0 || max > 1<<16 {
			max = 1 << 16
		}
		fr, err := ReadFrame(bufio.NewReader(bytes.NewReader(data)), max)
		if err != nil {
			if code := CloseCodeOf(err); code != 0 {
				switch code {
				case CloseProtocolError, CloseMessageTooBig, CloseInvalidPayload:
				default:
					t.Fatalf("close code %d is not one this parser may report (%v)", code, err)
				}
			}
			return
		}

		if int64(len(fr.Payload)) > max {
			t.Fatalf("accepted a %d-byte payload against a %d-byte limit", len(fr.Payload), max)
		}
		switch fr.Opcode {
		case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
		default:
			t.Fatalf("accepted reserved opcode 0x%x", byte(fr.Opcode))
		}
		if fr.Opcode.IsControl() {
			if !fr.Fin {
				t.Fatal("accepted a fragmented control frame")
			}
			if len(fr.Payload) > 125 {
				t.Fatalf("accepted a %d-byte control frame", len(fr.Payload))
			}
		}

		// Re-encode and re-read. The server encoding is unmasked, so this also
		// exercises the branch a client frame never reaches.
		var out bytes.Buffer
		if _, err := WriteFrame(&out, fr.Fin, fr.Opcode, fr.Payload); err != nil {
			t.Fatalf("WriteFrame on an accepted frame: %v", err)
		}
		// Mask it back into client shape, because ReadFrame requires a mask.
		again, err := ReadFrame(bufio.NewReader(bytes.NewReader(clientFrame(fr.Fin, fr.Opcode, fr.Payload))), max)
		if err != nil {
			t.Fatalf("re-reading an accepted frame failed: %v", err)
		}
		if again.Fin != fr.Fin || again.Opcode != fr.Opcode || !bytes.Equal(again.Payload, fr.Payload) {
			t.Fatalf("round trip changed the frame: %+v -> %+v", fr, again)
		}
	})
}

// FuzzClosePayload pins the close-body parser, which reads a length-prefixed
// code and then UTF-8 text a peer chose.
func FuzzClosePayload(f *testing.F) {
	f.Add(ClosePayload(CloseNormal, "bye"))
	f.Add([]byte{})
	f.Add([]byte{0x03})
	f.Add([]byte{0x03, 0xE8, 0xff, 0xfe})

	f.Fuzz(func(t *testing.T, data []byte) {
		code, reason, err := ParseClose(data)
		if err != nil {
			switch CloseCodeOf(err) {
			case CloseProtocolError, CloseInvalidPayload:
			default:
				t.Fatalf("close code %d is not one this parser may report (%v)", CloseCodeOf(err), err)
			}
			return
		}
		if code != CloseNoStatus && !validPeerCloseCode(code) {
			t.Fatalf("accepted close code %d", code)
		}
		if len(reason) > len(data) {
			t.Fatalf("reason %q is longer than the body it came from", reason)
		}
	})
}
