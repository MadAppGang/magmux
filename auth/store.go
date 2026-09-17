package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"

	"github.com/MadAppGang/magmux/protocol"
)

// Kind is what a presented credential turned out to be.
type Kind int

const (
	// None is "not a token of ours". It is the zero value on purpose: every
	// path that forgets to look at the bool gets the refusal.
	None Kind = iota
	// Full is the session token: every op, every transport.
	Full
	// View is the read-only token (D1): read-class BUILT-IN ops and watching,
	// plus whatever --view-op granted by name.
	View
)

// ReadOnly reports whether this kind must be refused anything that is not read.
func (k Kind) ReadOnly() bool { return k == View }

// Store holds the digests of the tokens this magmux accepts, and the op names
// a viewer was explicitly granted.
//
// The raw tokens are NOT kept. Nothing in magmux needs to reproduce a token
// after startup — the only operation is "is this candidate one of ours" — and a
// digest cannot be leaked into a log line, a panel row or a core dump as a
// usable credential.
type Store struct {
	full    [sha256.Size]byte
	view    [sha256.Size]byte
	hasView bool
	// viewOps is --view-op, by fully-qualified name. It widens the view token
	// and nothing else, and it can only ever name a PLUGIN op: every name here
	// must contain a dot, and no built-in has one. A plugin's self-declared
	// class therefore never widens the view token — only the operator does.
	viewOps map[string]bool
}

// NewStore builds the store. view may be empty, which disables viewers
// entirely; viewOps entries are expected to be fully qualified (`plugin.op`)
// and anything else is ignored, because granting a name that could match a
// built-in would let this flag hand a viewer a shell.
func NewStore(full, view string, viewOps []string) *Store {
	s := &Store{full: sha256.Sum256([]byte(full))}
	if view != "" {
		s.view = sha256.Sum256([]byte(view))
		s.hasView = true
	}
	for _, name := range viewOps {
		if !ValidViewOp(name) {
			continue
		}
		if s.viewOps == nil {
			s.viewOps = make(map[string]bool, len(viewOps))
		}
		s.viewOps[name] = true
	}
	return s
}

// ValidViewOp reports whether a --view-op value names something this flag is
// allowed to grant.
//
// It must be `<plugin>.<op>` with both halves non-empty. The dot is the whole
// check: built-in op names have none, so a malformed or over-broad value can
// never reach `send`, `open_pane` or `input`. A value that fails here is
// IGNORED rather than fatal, which is the safe direction — ignoring grants
// nothing.
func ValidViewOp(name string) bool {
	i := strings.IndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return false
	}
	return strings.IndexByte(name[i+1:], '.') < 0
}

// Check classifies a presented token in constant time.
//
// Both comparisons are made before either result is read. A short-circuit would
// make "this is not even the full token" measurably cheaper than "this is close
// to the view token", which is the leak the constant-time compare exists to
// prevent.
func (s *Store) Check(tok string) Kind {
	if s == nil || tok == "" {
		return None
	}
	sum := sha256.Sum256([]byte(tok))
	fullOK := subtle.ConstantTimeCompare(sum[:], s.full[:])
	viewOK := 0
	if s.hasView {
		viewOK = subtle.ConstantTimeCompare(sum[:], s.view[:])
	}
	switch {
	case fullOK == 1:
		return Full
	case viewOK == 1:
		return View
	}
	return None
}

// sameToken is the constant-time equality used away from the Store: the
// exit-time "is this still my token file" check.
func sameToken(a, b string) bool {
	sa := sha256.Sum256([]byte(a))
	sb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(sa[:], sb[:]) == 1
}

// HasView reports whether viewers are enabled at all.
func (s *Store) HasView() bool { return s != nil && s.hasView }

// GrantedToViewers reports whether --view-op named this op.
func (s *Store) GrantedToViewers(op string) bool {
	return s != nil && s.viewOps[op]
}

// Authorize answers "may a caller of this kind run this op", given the op's own
// advertisement.
//
// A full-token caller may run anything. A viewer may run a BUILT-IN op of class
// read, or any op --view-op named, and nothing else. The source check is what
// keeps a plugin from declaring its `deploy` op class read and thereby handing
// itself to every viewer; the operator's grant list is the only way a plugin op
// reaches one.
//
// An op that is not registered returns nil: the answer to "unknown op" is
// unknown_verb from the registry, not forbidden from here, and a viewer that
// could tell the two apart would have an op-name oracle.
func (s *Store) Authorize(k Kind, spec protocol.OpSpec, registered bool) error {
	if !k.ReadOnly() || !registered {
		return nil
	}
	if s.GrantedToViewers(spec.Name) {
		return nil
	}
	if spec.Source == protocol.SourceBuiltin && spec.Class == protocol.ClassRead {
		return nil
	}
	return protocol.Errf(protocol.CodeForbidden,
		"op %q is not available to the view token (class %s, source %s)", spec.Name, spec.Class, spec.Source)
}

// EffectiveReadOnly is the ReadOnly flag to put on the hub Caller for one op.
//
// It is the caller's kind, EXCEPT for an op --view-op granted by name: the hub
// enforces the class rule on its own, so a granted control-class plugin op
// would otherwise be refused twice — once by Authorize, which just allowed it,
// and again by the registry. Authorize has already decided; this is that
// decision being carried across the boundary rather than argued again.
func (s *Store) EffectiveReadOnly(k Kind, op string) bool {
	return k.ReadOnly() && !s.GrantedToViewers(op)
}
