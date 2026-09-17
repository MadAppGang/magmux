// Package auth is magmux's bearer-token layer: where a token comes from, what
// a token file is allowed to look like, how a candidate is compared, and the
// single-use tickets that keep a raw token out of every URL.
//
// A pane is a shell, so a token here is remote code execution. Three rules
// follow from that and are enforced nowhere else:
//
//   - A token ALWAYS exists once magmux listens. It comes from the environment,
//     from a file, or it is generated; there is no "no token" mode and no
//     --token flag, because a value on a command line is in every ps listing on
//     the machine.
//   - Comparison is over SHA-256 digests with subtle.ConstantTimeCompare, and
//     both candidates are compared before either result is read, so neither the
//     time nor the branch says which token was closer.
//   - A token file is refused unless it is a regular file, owned by this euid,
//     with no group or other permission bit set. A token another user can read
//     is not a secret, and a symlink is somebody else's file.
//
// The view token (--view-token-file / MAGMUX_VIEW_TOKEN) is the read-only half
// of D1. It is never generated: a viewer capability that appears by itself is a
// capability nobody asked for.
//
// It is an implementation detail of magmux, with no API stability before v1.
package auth
