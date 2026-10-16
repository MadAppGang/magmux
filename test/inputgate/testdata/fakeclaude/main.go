// Command fakeclaude imitates the EXTERNALLY OBSERVABLE terminal behaviour of
// Claude Code 2.1.301 as measured in the session's facts files
// (facts/claude-code-behaviour.md, facts/claude-screen-captures.md). It knows
// nothing about magmux. The black-box tests in test/inputgate launch it as a
// pane and read what it RECEIVED from the log it writes, which is the child's
// own truth about which bytes reached it and when.
//
// Behaviour (all timings in ms from process start, configured by env vars,
// because magmux may append arguments such as --settings to a Claude command
// line and every argument is therefore ignored, only logged):
//
//	FAKE_LOG            path of a JSON-lines event log (required for assertions)
//	FAKE_RAW_MS         first switch to raw mode               (default 20)
//	FAKE_COOKED=1       never leave cooked mode
//	FAKE_BOX_MS         input box appears; -1 = never          (default 600)
//	FAKE_RESET_MS       stdin reset (cooked→raw blip); -1 none (default 900)
//	FAKE_RESET_LEN_MS   length of the reset blip               (default 200)
//	FAKE_DIALOG         "permission" | "question": shown INSTEAD of the box at
//	                    box time; Escape closes it and returns the box; a digit
//	                    or Enter "approves" it (logged as dialog-approved)
//	FAKE_VIM=1          footer shows `-- INSERT --`
//	FAKE_PREFILL        text already in the box when it appears
//	FAKE_LOSE_ON_INPUT=1  the FIRST input chunk triggers a stdin reset and
//	                    everything received during it is discarded (lost text)
//	FAKE_LOSE_LEN_MS    length of that reset                   (default 400)
//	FAKE_NO_HISTORY=1   Enter clears the box but never adds a history entry
//	FAKE_IGNORE_ENTERS  number of Enter presses ignored before one submits
//	FAKE_EXIT_MS        exit(0) at this time; -1 never         (default -1)
//	FAKE_RENDER_DELAY_MS  input is applied at once but the screen is redrawn
//	                    this long after it (Ink's frame lag under load); 0 = at
//	                    once                                   (default 0)
//
// Screen shape (F7): the 2.1.301 welcome banner at the top, a `❯ <text>` box row at column 0 between two full-width
// `─` rules, wrapped rows indented two spaces, an empty box shows a FAINT
// (SGR 2) `Try "…"` placeholder; a submitted prompt moves above the box as a
// `❯ <text>` history row with markdown delimiters ` * ~ removed (F10).
package main

import (
	"encoding/json"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const placeholder = `Try "write a test for parser.go"`

var start = time.Now()

func ms() int64 { return time.Since(start).Milliseconds() }

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ── log ─────────────────────────────────────────────────────────────────────

var (
	logMu sync.Mutex
	logF  *os.File
)

func logEv(ev string, kv map[string]any) {
	logMu.Lock()
	defer logMu.Unlock()
	if logF == nil {
		return
	}
	m := map[string]any{"t": ms(), "ev": ev}
	for k, v := range kv {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	b = append(b, '\n')
	_, _ = logF.Write(b)
	_ = logF.Sync()
}

// ── termios ─────────────────────────────────────────────────────────────────

var cooked *unix.Termios

func setRaw() {
	if cooked == nil {
		return
	}
	t := *cooked
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	_ = unix.IoctlSetTermios(0, ioctlSetFlush, &t)
	logEv("raw", nil)
}

func setCooked() {
	if cooked == nil {
		return
	}
	_ = unix.IoctlSetTermios(0, ioctlSetFlush, cooked)
	logEv("cooked", nil)
}

// ── state ───────────────────────────────────────────────────────────────────

type state struct {
	mu        sync.Mutex
	boxShown  bool
	dialog    string // "" | permission | question
	box       []rune
	history   []string
	inPaste   bool
	dropUntil int64 // input received before this time is discarded
	lostDone  bool
	enters    int
	pending   []byte // incomplete UTF-8 / escape sequence
}

var (
	st         state
	vim        = os.Getenv("FAKE_VIM") == "1"
	noHistory  = os.Getenv("FAKE_NO_HISTORY") == "1"
	loseOnIn   = os.Getenv("FAKE_LOSE_ON_INPUT") == "1"
	ignoreEnts = envInt("FAKE_IGNORE_ENTERS", 0)
	renderLag  = time.Duration(envInt("FAKE_RENDER_DELAY_MS", 0)) * time.Millisecond
	outMu      sync.Mutex
)

func size() (rows, cols int) {
	ws, err := unix.IoctlGetWinsize(1, unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 40, 100
	}
	return int(ws.Row), int(ws.Col)
}

// wrap lays text out as Claude does: first row "❯ ", continuation rows "  ".
func wrap(text string, cols int) []string {
	w := cols - 2
	if w < 1 {
		w = 1
	}
	var out []string
	for i, line := range strings.Split(text, "\n") {
		r := []rune(line)
		first := true
		for first || len(r) > 0 {
			n := min(w, len(r))
			prefix := "  "
			if i == 0 && first {
				prefix = "❯ "
			}
			out = append(out, prefix+string(r[:n]))
			r = r[n:]
			first = false
		}
	}
	return out
}

func stripMarkdown(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && i+1 < len(rs) && rs[i+1] == '*' {
			b.WriteString(`\*`)
			i++
			continue
		}
		if c == '`' || c == '*' || c == '~' {
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func rule(cols int) string { return strings.Repeat("─", cols) }

// renderLocked redraws the whole screen. Caller holds st.mu.
func renderLocked() {
	rows, cols := size()
	var lines []string
	// The welcome banner, as 2.1.301 draws it at the top of every
	// conversation (s1): magmux's proof that the conversation's top is on
	// screen, so every user-prompt entry under it is accounted for.
	lines = append(lines,
		" ▐▛███▛█   Claude Code v2.1.301",
		"▝▜██████▀  Haiku 5.5 · Claude Max",
		" ▝▝   ▝▝   ~/fake",
		"")
	for _, h := range st.history {
		lines = append(lines, wrap(h, cols)...)
		lines = append(lines, "⏺ ack", "")
	}
	cursorRow, cursorCol := 0, 0
	switch {
	case st.dialog == "permission":
		lines = append(lines,
			rule(cols),
			" Bash command",
			"",
			"   sleep 20 && echo DONE-ONE",
			"",
			" Do you want to proceed?",
			" ❯ 1. Yes",
			"   2. Yes, and always allow access to",
			"   3. Yes, and switch to auto mode · auto mode handles these prompts for you",
			"   4. No",
			"",
			" Esc to cancel · Tab to amend")
	case st.dialog == "question":
		lines = append(lines,
			" ☐ Colour",
			"",
			"Which colour should the button be?",
			rule(cols),
			"❯ 1. Red",
			"  2. Blue",
			rule(cols),
			"Enter to select · ↑/↓ to navigate · Esc to cancel")
	case st.boxShown:
		lines = append(lines, rule(cols))
		if len(st.box) == 0 {
			lines = append(lines, "❯ \x1b[2m"+placeholder+"\x1b[22m")
			cursorRow, cursorCol = len(lines), 3
		} else {
			bl := wrap(string(st.box), cols)
			lines = append(lines, bl...)
			cursorRow = len(lines)
			cursorCol = utf8.RuneCountInString(bl[len(bl)-1]) + 1
		}
		lines = append(lines, rule(cols))
		if vim {
			lines = append(lines, "  -- INSERT --")
		} else {
			lines = append(lines, "  ? for shortcuts")
		}
	default:
		lines = append(lines, "  Loading…")
	}
	// Keep the bottom of the document on screen.
	drop := 0
	if len(lines) > rows {
		drop = len(lines) - rows
		lines = lines[drop:]
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l\x1b[H\x1b[2J")
	for i, l := range lines {
		b.WriteString("\x1b[" + strconv.Itoa(i+1) + ";1H" + l)
	}
	if cursorRow > drop {
		b.WriteString("\x1b[" + strconv.Itoa(cursorRow-drop) + ";" + strconv.Itoa(cursorCol) + "H")
	}
	b.WriteString("\x1b[?25h")
	outMu.Lock()
	_, _ = os.Stdout.WriteString(b.String())
	outMu.Unlock()
}

func render() {
	st.mu.Lock()
	renderLocked()
	st.mu.Unlock()
}

// ── input ───────────────────────────────────────────────────────────────────

func enter() {
	if st.inPaste {
		st.box = append(st.box, '\n')
		return
	}
	if st.dialog != "" {
		logEv("dialog-approved", map[string]any{"dialog": st.dialog, "key": "enter"})
		st.dialog = ""
		return
	}
	if !st.boxShown {
		return
	}
	st.enters++
	if st.enters <= ignoreEnts {
		logEv("enter-ignored", nil)
		return
	}
	text := string(st.box)
	st.box = st.box[:0]
	if text == "" {
		logEv("enter-empty", nil)
		return
	}
	logEv("submit", map[string]any{"text": text})
	if !noHistory {
		st.history = append(st.history, stripMarkdown(text))
	}
}

func escape() {
	if st.dialog != "" {
		logEv("dialog-closed", map[string]any{"dialog": st.dialog})
		st.dialog = ""
		st.boxShown = true
		st.history = append(st.history, "  ⎿  User rejected the request")
		return
	}
}

func printable(r rune) {
	if st.dialog != "" {
		if r >= '1' && r <= '9' {
			logEv("dialog-approved", map[string]any{"dialog": st.dialog, "key": string(r)})
			st.dialog = ""
			st.boxShown = true
		}
		return
	}
	if !st.boxShown {
		return
	}
	st.box = append(st.box, r)
}

// feed interprets a chunk of input. Caller holds st.mu.
func feed(p []byte) {
	buf := append(st.pending, p...)
	st.pending = nil
	for i := 0; i < len(buf); {
		c := buf[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(buf) {
				escape()
				i++
				continue
			}
			if buf[i+1] != '[' && buf[i+1] != 'O' {
				escape()
				i++
				continue
			}
			j := i + 2
			for j < len(buf) && (buf[j] < 0x40 || buf[j] > 0x7e) {
				j++
			}
			if j >= len(buf) {
				st.pending = append([]byte(nil), buf[i:]...)
				return
			}
			seq := string(buf[i : j+1])
			switch seq {
			case "\x1b[200~":
				st.inPaste = true
			case "\x1b[201~":
				st.inPaste = false
			}
			i = j + 1
		case c == '\r' || c == '\n':
			enter()
			i++
		case c == 0x7f || c == 0x08:
			if len(st.box) > 0 {
				st.box = st.box[:len(st.box)-1]
			}
			i++
		case c == 0x15:
			st.box = st.box[:0]
			i++
		case c < 0x20:
			i++
		default:
			if !utf8.FullRune(buf[i:]) {
				st.pending = append([]byte(nil), buf[i:]...)
				return
			}
			r, n := utf8.DecodeRune(buf[i:])
			printable(r)
			i += n
		}
	}
}

func reset(lenMs int, why string) {
	logEv("reset-start", map[string]any{"why": why})
	setCooked()
	time.Sleep(time.Duration(lenMs) * time.Millisecond)
	setRaw()
	logEv("reset-end", map[string]any{"why": why})
}

func readLoop() {
	buf := make([]byte, 4096)
	loseLen := envInt("FAKE_LOSE_LEN_MS", 400)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			st.mu.Lock()
			now := ms()
			dropped := now < st.dropUntil
			trigger := false
			if loseOnIn && !st.lostDone {
				st.lostDone = true
				dropped = true
				trigger = true
				st.dropUntil = now + int64(loseLen) + 50
			}
			logEv("in", map[string]any{"data": string(chunk), "dropped": dropped})
			if !dropped {
				feed(chunk)
				if renderLag > 0 {
					time.AfterFunc(renderLag, render)
				} else {
					renderLocked()
				}
			}
			st.mu.Unlock()
			if trigger {
				go reset(loseLen, "input")
			}
		}
		if err != nil {
			return
		}
	}
}

func main() {
	if p := os.Getenv("FAKE_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			logF = f
		}
	}
	logEv("argv", map[string]any{"args": os.Args[1:]})
	if t, err := unix.IoctlGetTermios(0, ioctlGet); err == nil {
		cooked = t
	}

	rawMs := envInt("FAKE_RAW_MS", 20)
	boxMs := envInt("FAKE_BOX_MS", 600)
	resetMs := envInt("FAKE_RESET_MS", 900)
	resetLen := envInt("FAKE_RESET_LEN_MS", 200)
	exitMs := envInt("FAKE_EXIT_MS", -1)
	dialog := os.Getenv("FAKE_DIALOG")

	render()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			render()
		}
	}()

	if os.Getenv("FAKE_COOKED") != "1" {
		time.AfterFunc(time.Duration(rawMs)*time.Millisecond, func() {
			setRaw()
			outMu.Lock()
			_, _ = os.Stdout.WriteString("\x1b[?2004h")
			outMu.Unlock()
		})
	}
	if boxMs >= 0 {
		time.AfterFunc(time.Duration(boxMs)*time.Millisecond, func() {
			st.mu.Lock()
			if dialog != "" {
				st.dialog = dialog
				logEv("dialog-shown", map[string]any{"dialog": dialog})
			} else {
				st.boxShown = true
				st.box = []rune(os.Getenv("FAKE_PREFILL"))
				logEv("box", nil)
			}
			renderLocked()
			st.mu.Unlock()
		})
	}
	if resetMs >= 0 && os.Getenv("FAKE_COOKED") != "1" {
		time.AfterFunc(time.Duration(resetMs)*time.Millisecond, func() {
			st.mu.Lock()
			st.dropUntil = ms() + int64(resetLen) + 50
			st.mu.Unlock()
			reset(resetLen, "boot")
		})
	}
	if exitMs >= 0 {
		time.AfterFunc(time.Duration(exitMs)*time.Millisecond, func() {
			logEv("exit", nil)
			if cooked != nil {
				_ = unix.IoctlSetTermios(0, ioctlSetFlush, cooked)
			}
			os.Exit(0)
		})
	}

	go readLoop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	<-sig
	logEv("exit", nil)
	if cooked != nil {
		_ = unix.IoctlSetTermios(0, ioctlSetFlush, cooked)
	}
}
