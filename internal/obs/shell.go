package obs

// This file is the GENERIC tier's vocabulary: what can be known about a pane
// running something nobody wrote an adapter for — a shell, a build, a dev
// server, a tool magmux has never heard of.
//
// Everything else in this package describes a SESSION, which is a thing a
// harness wrote a record about. The two facts here are the only ones magmux
// can observe about a pane with no harness at all, and they come from opposite
// directions: one is a kernel fact about who owns the terminal, the other is a
// shell volunteering a mark about itself. Both answer the same question — is
// there a command running in there, or is the shell sitting at a prompt — and
// both can decline to answer, which is why neither is a bool.

// ShellMark is what a shell's OSC 133 marks last said about its own foreground.
//
// OSC 133 is the shell-integration protocol every modern shell can be made to
// emit: `133;A` when it starts drawing a prompt, `133;B` when the command line
// begins, `133;C` just before it runs what was typed, and `133;D` — optionally
// carrying an exit status — when that command finishes. magmux's parser
// collapses the four into the two states that matter to an observer, because
// "drawing a prompt" and "waiting at a prompt" are the same fact about whether
// work is happening.
//
// # It is COOPERATIVE, and that is its strength and its whole limitation
//
// A mark arrives only because the shell was configured to send one. Nothing
// here can make a shell speak, and the absence of marks is not evidence of
// anything: [ShellMarkUnknown] is by far the commonest value on a real
// machine and means "this shell does not do shell integration", never "no
// command is running". The counterpart signal, [ShellState.AtPrompt], needs no
// cooperation at all and is ambiguous instead — see there. They are kept as
// separate fields rather than reconciled here because a consumer that can see
// both can see WHY the tier concluded what it did.
//
// # Not a wire value
//
// Unlike [Status], [WaitingReason] and [Liveness], this enum has no
// MarshalJSON: nothing publishes it. It is evidence the inferred tier weighs,
// and what reaches a consumer is the conclusion — the session's status, its
// confidence, its conflict and the words in its attention reason. Adding a
// `shell` object to the document would be a fourth vocabulary for a consumer
// to learn in order to re-derive a verdict this layer already took
// responsibility for. The day something needs the raw marks, the enum already
// has its wire words below.
type ShellMark uint8

const (
	// ShellMarkUnknown is the zero value: this shell emits no marks, or none
	// has arrived yet. It is NOT "nothing is running".
	ShellMarkUnknown ShellMark = iota
	// ShellMarkPrompt is a shell at a prompt: the last mark was 133;A, 133;B
	// or 133;D, so whatever was running has finished and the terminal is the
	// shell's again.
	ShellMarkPrompt
	// ShellMarkRunning is a command executing: 133;C has been seen and no
	// 133;D has closed it.
	ShellMarkRunning
)

// The names follow the shape every enum here has — index 0 is "no opinion" —
// even though this one never marshals. A value printed in a failing test and
// the same value in a log line then read identically.
var shellMarkEnum = enumSpec{"ShellMark", []string{
	"unknown",
	"prompt",
	"running",
}}

// String returns the display word.
func (m ShellMark) String() string { return shellMarkEnum.name(int(m)) }

// ShellState is everything the generic tier knows about ONE pane's shell: who
// owns its terminal, what its own marks last said, and how the last command
// ended.
//
// It is a plain value with no methods. Reconciling the two sources is the
// verdict package's job and is written out there, in one place, beside the
// staleness rule it has to be weighed against.
type ShellState struct {
	// AtPrompt is the tcgetpgrp answer, BEHIND THE LATCH, and the latch is
	// the only reason this field can be trusted.
	//
	// The raw comparison is foreground process group against session leader,
	// and it is ambiguous in exactly one direction. An interactive shell at a
	// prompt IS the foreground group — and so is a non-interactive `zsh -l -c
	// 'make'` for the entire run of make, because a shell with no job control
	// never hands the terminal to anything. Measured on this machine,
	// architecture v4 §0.2: every `-e` pane would report a permanently idle
	// shell. A wrong "idle" is worse than "undetermined" because a user acts
	// on it.
	//
	// So an implementation must not answer [LivenessYes] until it has watched
	// that pane's foreground group LEAVE the session leader and come back —
	// the flip that proves this shell has job control and therefore that
	// equality means something. Before that flip, equality is
	// [LivenessUnknown] and stays there forever if the flip never comes,
	// which is correct: a pane that never proved job control never earns the
	// stronger answer.
	//
	// [LivenessNo] needs no latch. A foreground group that is NOT the session
	// leader is unambiguous — something else holds the terminal — and it is
	// what sets the latch's first half.
	//
	// What it does NOT mean: LivenessNo is "the shell is not at a prompt",
	// not "work is happening". A pane running vim, htop or an agent reports
	// LivenessNo for hours while its program sits waiting for a human. The
	// verdict package weighs it against silence for exactly that reason.
	AtPrompt Liveness

	// Mark is what OSC 133 last said. See [ShellMark]: unknown is the common
	// case and is not a negative.
	Mark ShellMark

	// Exit is the status the last 133;D carried, and it is the ONLY generic
	// signal in this whole layer that distinguishes a command that FAILED
	// from one that merely ended.
	//
	// Three states, all reachable and all different: nil is "no exit status
	// was reported" — either no command has finished under observation, or
	// the shell sent a bare `133;D` with no status, which is legal and
	// common. A determined 0 is a command that succeeded. Anything else is
	// the status it failed with. Collapsing nil into 0 would report every
	// shell that does not send statuses as a machine where everything always
	// works.
	//
	// It is NOT cleared when the next command starts: it is the last
	// COMPLETED command's status, and a consumer reading it beside
	// [ShellMarkRunning] is reading the previous command's outcome, which is
	// the only thing it could possibly mean.
	Exit *int
}

// PaneShell answers what the terminal itself says about a pane's shell: who
// owns the terminal right now, and what the shell's own OSC 133 marks last
// reported.
//
// # A fourth port rather than fields on [PaneActivity]
//
// The same segregation argument [PaneActivity] makes against [PaneLocator],
// with one addition specific to this port: an implementation of this one has
// to SAMPLE — it asks the kernel a question whose answer changes — and the
// latch described in [ShellState.AtPrompt] can only advance when somebody
// asks. A port that must be called to work is a different kind of thing from
// one that reports two stamps, and folding it into the activity port would
// hide that from every implementer of it.
//
// # It is sampled on a cycle that already exists, and starts no cycle of its
// own
//
// The layer has no ticker and no poll loop, and this port does not get to be
// the exception. It is called once per observation from the same actor
// goroutine, at the same moment, as [PaneActivity.Activity] — which is
// whenever a record changed or a staleness deadline came due. An observation
// that never happens is a sample that is never taken, and the consequence is
// bounded and stated: a flip that happens entirely between two observations is
// not seen, so the latch stays unproven and the answer stays undetermined.
// That is the direction an inference is allowed to be wrong in.
//
// # It must not block
//
// Like every other port here, and with a sharper edge: the real
// implementation is an ioctl on a pty master. That is a non-blocking kernel
// call, but an implementation that took a lock magmux holds across I/O would
// put every session's chain behind that I/O. magmux's own resolves the pane
// under the pane table's read lock, RELEASES IT, and then makes the syscall.
//
// # One implementation serves every session, and this one MUTATES
//
// Unlike the other three ports, a call here advances a per-pane latch. That
// state belongs to the PANE and not to the port, which is what keeps the port
// itself shareable — but it does mean an implementation needs its own
// synchronisation for it. magmux's own takes the pane's lock for the two
// fields and holds nothing across the syscall.
//
// A nil PaneShell means "nothing in this process can look at a terminal",
// which is the honest answer for anything running outside a magmux — `magmux
// obs` is exactly that — and leaves the generic tier with the staleness
// cascade alone, exactly as it was before this port existed.
type PaneShell interface {
	// Shell reports the terminal facts for paneID.
	//
	// ok is false when there is no such pane, or when it has no terminal to
	// ask about — the control panel has no PTY and never will. It is not a
	// [Liveness] for the same reason [PaneActivity.Activity]'s flag is not:
	// "no such pane" and "could not tell" leave the caller with the same
	// nothing, and the fields inside [ShellState] already carry their own
	// undetermined states for the case where the pane exists and the question
	// could not be answered.
	Shell(paneID int) (ShellState, bool)
}
