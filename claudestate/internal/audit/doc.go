// Package audit holds the library's guard tests (architecture §6: G2–G6,
// G8, G10, G13 at this commit). It has no production code: this file exists
// only to carry the package's documentation, and every scanner lives in a
// _test.go file.
//
// Every guard asserts two things, and the second is what makes the first mean
// something: zero findings over the real module, and at least one finding over
// a deliberately violating snippet. A scanner that finds nothing is
// indistinguishable from a scanner that looks at nothing.
//
// The loader is a port of tt's audit.LoadModule with one change: it walks
// _probe/ and _diff/. tt's loader skips every "_" directory, which would
// silently exempt both commands from every guard.
package audit
