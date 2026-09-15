// Command magmux is a minimal terminal multiplexer.
//
// This is a thin shim: the terminal core, flag parsing and the process
// lifecycle all live in package mux, and mux.Main returns the exit status.
//
// The `magmux mcp` dispatch MUST stay the first action of the process: it
// runs before the --version/--help scan (which would claim a --help meant for
// `magmux mcp`) and before anything puts the tty in raw mode or writes to
// stdout, because one stray byte on stdout desynchronises a JSON-RPC client.
// While the MCP server still lives in package mux, that dispatch is the first
// statement of mux.Main; when the MCP server moves to its own package, the
// dispatch moves here, above the mux.Main call.
package main

import (
	"os"

	"github.com/MadAppGang/magmux/mux"
)

func main() {
	os.Exit(mux.Main(os.Args[1:]))
}
