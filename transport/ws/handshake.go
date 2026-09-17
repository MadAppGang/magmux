package ws

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GUID is RFC 6455 §1.3's magic value. It exists so a cache or a proxy that
// replays an ordinary GET cannot accidentally produce a valid handshake.
const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Subprotocol is the one magmux speaks. A client must offer it; magmux echoes
// only this, never the `magmux.auth.<token>` entry beside it, because echoing
// the credential would put it in a response header and therefore in every proxy
// log the response passes through.
const Subprotocol = "magmux.v1"

// AuthPrefix is how a browser presents a token: as a second, fake subprotocol.
// It is the only credential channel a browser WebSocket has — the API has no
// header argument — and it beats a query parameter because response headers are
// logged less often than URLs and because magmux never echoes it back.
const AuthPrefix = "magmux.auth."

// Accept computes Sec-WebSocket-Accept from the client's key (§4.2.2).
func Accept(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, GUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// CheckUpgrade validates the handshake request. The error's text is written
// into an HTTP 400, because the connection is not a WebSocket yet and there is
// no close code to send.
func CheckUpgrade(r *http.Request) error {
	if r.Method != http.MethodGet {
		return fmt.Errorf("a websocket handshake must be a GET")
	}
	if r.ProtoMajor == 1 && r.ProtoMinor < 1 {
		return fmt.Errorf("a websocket handshake needs HTTP/1.1")
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		return fmt.Errorf("Connection must include \"upgrade\"")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return fmt.Errorf("Upgrade must be \"websocket\"")
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		return fmt.Errorf("Sec-WebSocket-Version must be 13")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return fmt.Errorf("Sec-WebSocket-Key is missing")
	}
	// §4.1: the key is a base64 encoding of 16 random bytes. Checking it is not
	// security — the value is not a secret — but a client that sends something
	// else has a broken handshake and a clear 400 beats a mysterious upgrade.
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 16 {
		return fmt.Errorf("Sec-WebSocket-Key must be base64 of 16 bytes")
	}
	return nil
}

// Subprotocols lists what the client offered, in order, across however many
// Sec-WebSocket-Protocol headers it split them over.
func Subprotocols(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(line, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// TokenFromSubprotocols returns the token a browser smuggled in as
// `magmux.auth.<token>`, or "".
func TokenFromSubprotocols(h http.Header) string {
	for _, p := range Subprotocols(h) {
		if strings.HasPrefix(p, AuthPrefix) {
			return strings.TrimPrefix(p, AuthPrefix)
		}
	}
	return ""
}

// OffersSubprotocol reports whether the client offered magmux.v1.
func OffersSubprotocol(h http.Header) bool {
	for _, p := range Subprotocols(h) {
		if p == Subprotocol {
			return true
		}
	}
	return false
}

// WriteHandshake writes the 101 response onto a hijacked connection.
//
// It is written by hand rather than through http.ResponseWriter because the
// connection has already been taken away from the server. echoProto is either
// Subprotocol or "": magmux echoes magmux.v1 when it was offered and NEVER the
// auth entry.
func WriteHandshake(w io.Writer, key, echoProto string) error {
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: ")
	b.WriteString(Accept(key))
	b.WriteString("\r\n")
	if echoProto != "" {
		b.WriteString("Sec-WebSocket-Protocol: ")
		b.WriteString(echoProto)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// headerHasToken reports whether a comma-separated header contains a token,
// case-insensitively. Connection is the reason it exists: a browser may send
// "keep-alive, Upgrade".
func headerHasToken(h http.Header, name, token string) bool {
	for _, line := range h.Values(name) {
		for _, part := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
