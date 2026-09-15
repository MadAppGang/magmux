// Package mcp is `magmux mcp`: an MCP server over stdio that drives magmux
// sessions through their Unix sockets. Run is the entry point, and cmd/magmux
// calls it before anything else runs, because a single stray byte on stdout
// desynchronises a JSON-RPC client for the rest of the session.
//
// It is an implementation detail of magmux, with no API stability before v1.
package mcp
