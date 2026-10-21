package claude

import (
	"bufio"
	"io"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// lineBuffer is the 4 MB line budget every transcript reader here uses. A
// single tool result carrying a whole file exceeds bufio's 64 KB default, and
// an oversized token is not skipped: Scan returns false and the rest of the
// transcript silently vanishes.
const lineBuffer = 4 << 20

// ParseTurns reads a transcript and returns its last maxTurns turns, oldest
// first, holding no more than byteBudget bytes of them; at least one turn is
// kept whatever its size. Claude writes one assistant reply as several
// records, and they are one turn. A user record with array content is a tool
// RESULT and belongs to the call it answers, never to a turn of its own.
//
// Whatever was parsed before a read error is returned with the error.
func ParseTurns(r io.Reader, maxTurns, byteBudget int) ([]model.Turn, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), lineBuffer)

	var (
		turns []model.Turn
		sizes []int
		total int
		open  bool
	)
	trim := func() {
		for len(turns) > 1 && (len(turns) > maxTurns || total > byteBudget) {
			total -= sizes[0]
			turns, sizes = turns[1:], sizes[1:]
		}
	}
	push := func(t model.Turn, size int) {
		turns = append(turns, t)
		sizes = append(sizes, size)
		total += size
		trim()
	}
	grow := func(i, n int) {
		if i < 0 || i >= len(sizes) || n == 0 {
			return
		}
		sizes[i] += n
		total += n
		trim()
	}
	stamp := func(t *time.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return *t
	}

	for sc.Scan() {
		l, ok := DecodeLine(sc.Bytes())
		if !ok {
			continue
		}
		switch l.Kind {
		case LineUser:
			if text, isPrompt := PromptText(l); isPrompt {
				open = false
				push(model.Turn{Role: model.TurnUser, Text: text, Timestamp: stamp(l.Time)}, len(text))
				continue
			}
			for _, b := range l.Blocks {
				if b.Kind != BlockToolResult {
					continue
				}
				if i := model.AttachToolResult(turns, b.ToolID, b.Text); i >= 0 {
					grow(i, len(b.Text))
				}
			}
		case LineAssistant:
			if !open {
				push(model.Turn{Role: model.TurnAssistant, Timestamp: stamp(l.Time)}, 0)
				open = true
			}
			cur := &turns[len(turns)-1]
			added := 0
			for _, b := range l.Blocks {
				switch b.Kind {
				case BlockText:
					txt := strings.TrimSpace(b.Text)
					if txt == "" {
						continue
					}
					if cur.Text != "" {
						cur.Text += "\n\n"
						added += 2
					}
					cur.Text += txt
					added += len(txt)
				case BlockToolUse:
					cur.Tools = append(cur.Tools, model.NewToolCall(b.ToolName, b.ToolInput, b.ToolID))
					added += len(b.ToolName) + len(b.ToolInput)
				}
			}
			grow(len(turns)-1, added)
		}
	}
	return turns, sc.Err()
}
