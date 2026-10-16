package mux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLexerTrailingBackslashInExpansion (review LOW): a backslash that ends
// the line inside $(…) or ${…} is unparsed, exactly as readWord treats one
// outside an expansion — refused at the backslash, not by running off the end.
func TestLexerTrailingBackslashInExpansion(t *testing.T) {
	for _, line := range []string{`claude $(echo \`, `claude ${x\`, `claude "$(a \`} {
		if _, err := lexShell(line); err != errUnparsed {
			t.Errorf("lexShell(%q) = %v, want errUnparsed", line, err)
		}
		if skipNestedEnd, err := skipNested(line, strings.IndexByte(line, '\\'), '(', ')'); err != errUnparsed {
			t.Errorf("skipNested at the trailing backslash of %q = %d, %v; want errUnparsed", line, skipNestedEnd, err)
		}
	}
}

// TestReadRegEntryBounds (review LOW): a registry file is read whole up to
// regFileMax, and one past the bound is refused rather than half-parsed.
func TestReadRegEntryBounds(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "1.json")
	if err := os.WriteFile(small, []byte(`{"status":"waiting","waitingFor":"permission","sessionId":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if e, ok := readRegEntry(small); !ok || e.Status != "waiting" || e.SessionID != "s" {
		t.Errorf("readRegEntry = %+v, %v", e, ok)
	}
	big := filepath.Join(dir, "2.json")
	pad := strings.Repeat(" ", regFileMax)
	if err := os.WriteFile(big, []byte(`{"status":"waiting",`+pad+`"sessionId":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readRegEntry(big); ok {
		t.Error("a registry file past regFileMax was parsed")
	}
}
