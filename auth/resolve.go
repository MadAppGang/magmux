package auth

import (
	"fmt"
	"os"
)

// Config is where the tokens may come from, in the order Resolve consults them.
type Config struct {
	// EnvToken is MAGMUX_TOKEN. It wins over TokenFile and is NEVER written to
	// disk: a token the operator put in the environment is theirs to store, and
	// magmux writing a copy of it into /tmp would quietly create a second place
	// it can leak from.
	EnvToken string
	// TokenFile is --token-file. It must already exist and satisfy LoadFile's
	// rules; magmux does not create it. A path that cannot be read is fatal
	// rather than a fallback to generation, because silently listening on a
	// token the operator has never seen is worse than not listening.
	TokenFile string

	// EnvViewToken / ViewTokenFile are the read-only half (D1), in the same
	// order. There is no third step: a view token is never generated, because a
	// viewer capability nobody asked for is a capability nobody is tracking.
	EnvViewToken  string
	ViewTokenFile string

	// DefaultPath is where a GENERATED token is written, normally
	// {sockdir}/magmux-{id}.token. Used only when neither env nor file supplied
	// one.
	DefaultPath string

	// ViewOps is --view-op: plugin ops (`plugin.op`) a viewer may call despite
	// their class.
	ViewOps []string
}

// Resolved is the outcome: the tokens, the store that checks them, and whether
// magmux owns the file it wrote.
type Resolved struct {
	Token     string
	ViewToken string
	Store     *Store
	// GeneratedPath is the file magmux created, or "" when the token came from
	// the environment or from an operator's own file. It is the ONLY file
	// magmux will remove at exit, and even then only if it still holds this
	// process's token.
	GeneratedPath string
}

// Resolve settles both tokens, writing a generated one to disk before it
// returns.
//
// The write happens HERE rather than after the listener binds, and that
// ordering is the readiness signal: everything in this function runs before the
// HTTP listener is created, so an accepting port implies a final token file at
// its final name. A harness can poll for the port and then read the token with
// no window in which the file is absent or half-written.
func Resolve(cfg Config) (*Resolved, error) {
	out := &Resolved{}

	switch {
	case cfg.EnvToken != "":
		if !ValidToken(cfg.EnvToken) {
			return nil, fmt.Errorf("MAGMUX_TOKEN must be %d-%d characters of [A-Za-z0-9._~-]",
				MinTokenLen, MaxTokenLen)
		}
		out.Token = cfg.EnvToken
	case cfg.TokenFile != "":
		tok, err := LoadFile(cfg.TokenFile)
		if err != nil {
			return nil, err
		}
		out.Token = tok
	default:
		if cfg.DefaultPath == "" {
			return nil, fmt.Errorf("no token source and nowhere to write one")
		}
		tok, err := Generate()
		if err != nil {
			return nil, err
		}
		if err := WriteFile(cfg.DefaultPath, tok); err != nil {
			return nil, err
		}
		out.Token = tok
		out.GeneratedPath = cfg.DefaultPath
	}

	switch {
	case cfg.EnvViewToken != "":
		if !ValidToken(cfg.EnvViewToken) {
			// Every failure past the generation step takes the file with it: a
			// token file naming a credential for a port nothing will serve is
			// litter that looks like a secret.
			RemoveIfOurs(out.GeneratedPath, out.Token)
			return nil, fmt.Errorf("MAGMUX_VIEW_TOKEN must be %d-%d characters of [A-Za-z0-9._~-]",
				MinTokenLen, MaxTokenLen)
		}
		out.ViewToken = cfg.EnvViewToken
	case cfg.ViewTokenFile != "":
		tok, err := LoadFile(cfg.ViewTokenFile)
		if err != nil {
			RemoveIfOurs(out.GeneratedPath, out.Token)
			return nil, err
		}
		out.ViewToken = tok
	}

	if out.ViewToken != "" && sameToken(out.ViewToken, out.Token) {
		RemoveIfOurs(out.GeneratedPath, out.Token)
		return nil, fmt.Errorf("the view token and the session token are the same value; " +
			"a read-only credential that is also the full one grants nothing and hides that it does not")
	}

	out.Store = NewStore(out.Token, out.ViewToken, cfg.ViewOps)
	return out, nil
}

// Cleanup removes the token file magmux generated, if it still holds this
// process's token. A --token-file the operator wrote is never touched.
func (r *Resolved) Cleanup() {
	if r == nil {
		return
	}
	RemoveIfOurs(r.GeneratedPath, r.Token)
	// A temp file survives only if the process died between create and rename;
	// this covers the ordinary failure path, and the startup sweep covers the
	// one where nothing ran at all.
	if r.GeneratedPath != "" {
		os.Remove(TempName(r.GeneratedPath, os.Getpid()))
	}
}
