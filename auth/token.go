package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// TokenBytes is the entropy of a generated token. 32 bytes is 256 bits,
	// which is what the rest of the stack (SHA-256 digests, HMAC keys) is sized
	// for, and it survives being read aloud over a call exactly as badly as any
	// other length.
	TokenBytes = 32

	// MinTokenLen / MaxTokenLen bound a token that came from somewhere else.
	// The floor is the length of a generated one: a 6-character token in a file
	// is a typo or a placeholder, not a decision. The ceiling keeps a header and
	// a WebSocket subprotocol value inside what every intermediary accepts.
	MinTokenLen = 32
	MaxTokenLen = 256
)

// ValidToken reports whether s is usable as a magmux token.
//
// The alphabet is [A-Za-z0-9._~-], a subset of RFC 7230 `tchar` that is also
// legal in a WebSocket subprotocol value and needs no escaping in a header. It
// is deliberately NARROWER than what HTTP would accept: the token travels as
// `magmux.auth.<token>` in Sec-WebSocket-Protocol, where a comma or a space
// would split it into two protocol names, and browsers silently drop the
// request rather than reporting it.
func ValidToken(s string) bool {
	if len(s) < MinTokenLen || len(s) > MaxTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '~' || c == '-':
		default:
			return false
		}
	}
	return true
}

// Generate returns a fresh token: 32 random bytes as unpadded base64url, which
// is 43 characters of the alphabet above.
func Generate() (string, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// LoadFile reads a token from path, refusing anything that would make it not a
// secret.
//
// The checks are made with Lstat and on the OPENED file's own metadata, in that
// order, so a file swapped between the check and the read is caught by the
// second one. Refused:
//
//   - a symlink (Lstat), because the target is somebody else's file and its
//     permissions say nothing about the link;
//   - anything that is not a regular file — a fifo blocks the read, a directory
//     is a mistake;
//   - a file not owned by this euid;
//   - any group or other permission bit, read included. "Nobody has bothered to
//     use it yet" is not a security property.
//
// The content is trimmed of surrounding whitespace, because every way of
// writing a file adds a trailing newline and refusing that would be a riddle.
func LoadFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("token file %s is a symlink; magmux will not follow one to a secret", path)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if err := checkPerm(path, st); err != nil {
		return "", err
	}
	// A token is at most MaxTokenLen bytes plus whatever whitespace an editor
	// added; anything larger is not a token file and must not be read into
	// memory just to be rejected.
	buf := make([]byte, MaxTokenLen+64)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(buf[:n]))
	if !ValidToken(tok) {
		return "", fmt.Errorf("token file %s: a token must be %d-%d characters of [A-Za-z0-9._~-]",
			path, MinTokenLen, MaxTokenLen)
	}
	return tok, nil
}

// checkPerm is the ownership and mode half of LoadFile, split out because it is
// the half a test can exercise without a second uid.
func checkPerm(path string, fi os.FileInfo) error {
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("token file %s has mode %#o; it must be 0600 (no group or other bits)", path, perm)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// An unfamiliar filesystem. The mode check above still applied, and
		// refusing every such file would make magmux unusable somewhere nobody
		// has looked at yet.
		return nil
	}
	if uint32(st.Uid) != uint32(os.Geteuid()) {
		return fmt.Errorf("token file %s is owned by uid %d, not by this process (uid %d)",
			path, st.Uid, os.Geteuid())
	}
	return nil
}

// TempName is the name WriteFile writes through before renaming. It is exported
// because the startup sweep has to recognise one left behind by a magmux that
// died mid-write, and a pattern stated twice is a pattern that drifts.
func TempName(path string, pid int) string {
	return fmt.Sprintf("%s.%d.tmp", path, pid)
}

// WriteFile puts token at path, atomically and never through a symlink.
//
// It writes `<path>.<pid>.tmp` with O_CREATE|O_EXCL|O_NOFOLLOW and mode 0600,
// then renames it over the final name. The rename is what makes the path go
// from "the previous token" to "this token" with no instant in between where it
// is missing or half-written — the same unconditional replace the socket does
// (main.go:5340), and rename replaces a planted symlink rather than following
// it.
func WriteFile(path, token string) error {
	if !ValidToken(token) {
		return fmt.Errorf("refusing to write a malformed token to %s", path)
	}
	tmp := TempName(path, os.Getpid())
	// O_EXCL so a planted file is an error rather than a target, O_NOFOLLOW so a
	// planted symlink is an error rather than a redirection.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("token file %s: %w", tmp, err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("token file %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("token file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("token file %s: %w", path, err)
	}
	return nil
}

// RemoveIfOurs deletes path only if it still holds token.
//
// magmux removes the token file it generated, and only that one: a
// --token-file the operator wrote is never deleted. The content check is what
// makes the removal safe against a second magmux that took the same --id and
// replaced the file in between — an exiting process must never delete a token
// another process is serving.
func RemoveIfOurs(path, token string) {
	if path == "" || token == "" {
		return
	}
	// Deliberately not LoadFile: this runs at exit, and a file whose mode a
	// third party has meddled with should still be removed if its CONTENT is
	// ours. The comparison is constant-time for the same reason every other one
	// is.
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if !sameToken(strings.TrimSpace(string(b)), token) {
		return
	}
	os.Remove(path)
}

// DefaultPath is where a generated token lives: beside the socket, under the
// same id, so one --id names both files and the startup sweep can reason about
// both from one directory listing.
func DefaultPath(dir, id string) string {
	return filepath.Join(dir, "magmux-"+id+".token")
}
