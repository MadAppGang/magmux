// Package claude is THE CONTRACT: every decoder of what Claude Code writes
// under its root, and the vendor's own rule for a background session's status.
//
// It is pure. It opens nothing: it decodes bytes and readers it is handed, and
// touches an fs.FS only through fs.Stat and fs.ReadDir (LocateTranscript,
// CountSubagents). It imports no os and no ports (§2.3), which a guard checks.
//
// Every Claude-owned string is a constant in schema.go and nowhere else in the
// module (guard G1). Everything that leaves this package is in model's
// vocabulary, and nothing that enters it is trusted: an unrecognised value
// decodes to the enum's Unknown with the raw string kept, an implausible
// instant is refused rather than rescaled, and a value of the wrong JSON type
// is reported as a Problem and left zero. A guess would be worse than silence,
// because a plausible guess is indistinguishable from an observation.
//
// Strings are kept exactly as the tool wrote them (NFR11). Session names are
// attacker-influenced text, and neutralising them is each host's renderer's
// job, never the decoder's.
package claude
