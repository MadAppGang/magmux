// Package proc looks up a process's parent pid. The MCP server's self-pane
// guard walks that chain to tell "that pane is me" from "that pane is another
// agent". The platform halves are proc_darwin.go and proc_linux.go.
//
// It is an implementation detail of magmux, with no API stability before v1.
package proc
