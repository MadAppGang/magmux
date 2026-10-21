// Package obsview renders the session-observation document for people and
// for programs: the `magmux agents` table, its `--json` form, and the one rule
// for registry text reaching a terminal ([DisplaySafe], [TerminalSafeJSON]).
//
// It sits BELOW mux on purpose. The same fleet is shown by three surfaces —
// the `magmux agents` command, the socket `agents` verb and the MCP tool
// `list_agents` — and `mcp` cannot import `mux` (TestImportDirection). One
// renderer that all three call is what makes "one document, three renderings"
// a property rather than a promise; a second copy of the table, or of the
// display rule, is how two of three renderers once forgot to neutralise a
// session name.
//
// It imports internal/obs for the document's types and nothing from mux, and
// it writes nothing anywhere but the io.Writer it is handed (and os.Stderr for
// an encoding failure, as `magmux agents` always has).
package obsview
