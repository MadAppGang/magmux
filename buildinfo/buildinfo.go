// Package buildinfo holds the version stamp of a magmux build.
//
// It is an implementation detail of magmux, with no API stability before v1.
//
// GoReleaser sets both variables at link time:
//
//	-X github.com/MadAppGang/magmux/buildinfo.Version={{.Version}}
//	-X github.com/MadAppGang/magmux/buildinfo.Commit={{.ShortCommit}}
//
// A local `go build` keeps the defaults, which is how `magmux --version` tells
// a release binary from a development one.
package buildinfo

// Version and Commit are overwritten by -ldflags -X; the defaults mark a
// local build.
var (
	Version = "dev"
	Commit  = "none"
)
