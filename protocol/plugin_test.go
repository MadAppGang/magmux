package protocol

import "testing"

// TestNameGrammar is a table because the grammar's whole job is to be exactly
// this narrow, and every exclusion below pays for something specific
// downstream.
func TestNameGrammar(t *testing.T) {
	plugins := []struct {
		name string
		ok   bool
		why  string
	}{
		{"ticket", true, ""},
		{"ticket-runner", true, "hyphens are allowed; they are safe in every namespace"},
		{"t", true, "one character is a name"},
		{"", false, "a plugin needs a name"},
		{"ticket_runner", false, "underscore would make a __ split ambiguous for MCP"},
		{"Ticket", false, "upper case would collide with a lower-cased RTDB key"},
		{"9lives", false, "a leading digit is not an identifier"},
		{"-lead", false, "a leading hyphen is not an identifier"},
		{"ticket.runner", false, "a dot is the qualified-name separator"},
		{"ticket/runner", false, "a slash is a path"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true, "32 is the limit"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false, "33 is past it"},
	}
	for _, c := range plugins {
		if got := ValidPluginName(c.name); got != c.ok {
			t.Errorf("ValidPluginName(%q) = %v, want %v — %s", c.name, got, c.ok, c.why)
		}
	}

	ops := []struct {
		name string
		ok   bool
		why  string
	}{
		{"run", true, ""},
		{"run_ticket", true, "underscore is allowed in an OP name; only the plugin half is split on __"},
		{"run2", true, ""},
		{"", false, "an op needs a name"},
		{"Run", false, "upper case"},
		{"run-ticket", false, "hyphen is not in the op grammar"},
		{"_run", false, "a leading underscore is not an identifier"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true, "30 is the limit"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false, "31 is past it"},
	}
	for _, c := range ops {
		if got := ValidOpName(c.name); got != c.ok {
			t.Errorf("ValidOpName(%q) = %v, want %v — %s", c.name, got, c.ok, c.why)
		}
	}
}

// TestQualifiedAndToolNamesRoundTrip: the two spellings exist because MCP tool
// names admit no dot, and both have to be reversible without a table for
// magmux's own use. The bound matters as much as the split — an MCP tool name
// is capped at 64 characters, and the two grammars are sized to fit.
func TestQualifiedAndToolNamesRoundTrip(t *testing.T) {
	const plugin, op = "ticket-runner", "run_ticket"

	q := QualifiedOp(plugin, op)
	if q != "ticket-runner.run_ticket" {
		t.Fatalf("QualifiedOp = %q", q)
	}
	if p, o, ok := SplitQualifiedOp(q); !ok || p != plugin || o != op {
		t.Errorf("SplitQualifiedOp(%q) = %q, %q, %v", q, p, o, ok)
	}

	tool := ToolName(plugin, op)
	if tool != "ticket-runner__run_ticket" {
		t.Fatalf("ToolName = %q", tool)
	}
	if p, o, ok := SplitToolName(tool); !ok || p != plugin || o != op {
		t.Errorf("SplitToolName(%q) = %q, %q, %v", tool, p, o, ok)
	}

	// The worst case both grammars allow still fits MCP's 64.
	longest := ToolName(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 32
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",   // 30
	)
	if len(longest) != 64 {
		t.Errorf("the longest possible tool name is %d characters; the grammars are sized to "+
			"make it exactly 64", len(longest))
	}

	for _, bad := range []string{"", "ticket", ".run", "run."} {
		if _, _, ok := SplitQualifiedOp(bad); ok {
			t.Errorf("SplitQualifiedOp(%q) accepted a name with no usable halves", bad)
		}
	}
}

// TestIsPluginMessage names only the directions magmux RECEIVES. A client that
// sent `invoke` would be claiming to be magmux, and it must fall through to the
// ordinary unknown-verb answer rather than reaching the plugin host.
func TestIsPluginMessage(t *testing.T) {
	for _, typ := range []string{MsgPluginRegister, MsgInvokeResult, MsgPluginEvent, MsgControllerSnapshot} {
		if !IsPluginMessage(typ) {
			t.Errorf("%q is a plugin→magmux message and must be routed to the host", typ)
		}
	}
	for _, typ := range []string{MsgInvoke, MsgInvokeCancel, "send", "call", "open_pane", ""} {
		if IsPluginMessage(typ) {
			t.Errorf("%q must not be routed to the plugin host", typ)
		}
	}
}
