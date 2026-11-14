// Package difftest is the differential check's comparison model (architecture
// §8.1): both tt's `tt sessions --json --all` document and the library's
// model.Fleet are projected onto one Row shape, keyed by RowKey (which mirrors
// model.Key field for field), compared as KEYED SETS, and every difference is
// attributed to exactly one D-row predicate or reported as uncited.
//
// It is internal: only test authors inside the module, and the _diff driver,
// use it. It decodes tt's WIRE, never a Claude file, which is why guard G1
// carries it on its one cited allowlist entry and G6 skips it.
//
// Nothing here reads the filesystem, a clock or a process. Every input is a
// byte slice or a value, so the predicates can be tested from both sides.
package difftest
