// Package model is the library's vocabulary: Session, Key, Fleet, every enum,
// Problem and WatchTarget.
//
// Everything here is a plain value. There are no json tags (guard G6), no
// JSON methods, and no method that reads a clock: the wire belongs to each
// host, and every instant a session carries is one a record stated.
//
// The zero value of every enum is "no opinion", never a negative. A nil
// pointer field means "undetermined"; a pointer to zero is a determined zero.
package model
