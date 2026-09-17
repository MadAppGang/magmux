// Command magmux is a minimal terminal multiplexer.
//
// This is a thin shim: the terminal core, flag parsing and the process
// lifecycle all live in package mux, and mux.Main returns the exit status.
//
// The `magmux mcp` dispatch MUST stay the first action of the process: it
// runs before the --version/--help scan (which would claim a --help meant for
// `magmux mcp`) and before anything puts the tty in raw mode or writes to
// stdout, because one stray byte on stdout desynchronises a JSON-RPC client.
// The MCP server lives in package mcp, so the dispatch is the first statement
// here, above the mux.Main call, and mux.Main never sees an `mcp` argument.
package main

import (
	"os"

	"github.com/MadAppGang/magmux/mcp"
	"github.com/MadAppGang/magmux/mux"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(mcp.Run(os.Args[2:]))
	}
	os.Exit(mux.Main(os.Args[1:]))
}
