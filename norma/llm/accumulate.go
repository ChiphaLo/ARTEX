package llm

import (
	"encoding/json"
	"strings"
)

// Accumulator reassembles a complete assistant Message from a stream of
// normalized StreamEvents. The harness feeds every event to Add; once the
// stream ends, Message returns the finished assistant turn (text, thinking, and
// tool_use blocks in arrival order).
type Accumulator struct {
	blocks     []ContentBlock
	toolInputs []*strings.Builder
	// toolKeys routes interleaved tool_input_delta fragments to the right call
	// (Responses item_id / Anthropic block index / chat tool_calls index).
	toolKeys   map[string]int
	lastTool   int      // index of the most recently started call
	stopText   []string // per-call authoritative arguments from tool_use_stop
	textBuf    *strings.Builder
	thinkBuf   *strings.Builder
	thinkSig   string // signature for the current thinking block
	StopReason string
	Usage      Usage
}

// NewAccumulator returns an empty Accumulator.
func NewAccumulator() *Accumulator { return &Accumulator{} }

// Add folds one event into the accumulator.
func (a *Accumulator) Add(ev StreamEvent) {
	switch ev.Type {
	case SEMessageStart:
		a.Usage.Add(ev.Usage)
	case SETextDelta:
		if a.textBuf == nil {
			a.flushThinking()
			a.textBuf = &strings.Builder{}
		}
		a.textBuf.WriteString(ev.Text)
	case SEThinkingDelta:
		if a.thinkBuf == nil {
			a.flushText()
			a.thinkBuf = &strings.Builder{}
		}
		a.thinkBuf.WriteString(ev.Text)
	case SEThinkingSignature:
		a.thinkSig = ev.Text
	case SEToolUseStart:
		a.flushText()
		a.flushThinking()
		a.blocks = append(a.blocks, ContentBlock{Type: BlockToolUse, ID: ev.ToolID, Name: ev.ToolName})
		a.toolInputs = append(a.toolInputs, &strings.Builder{})
		a.stopText = append(a.stopText, "")
		a.lastTool = len(a.toolInputs) - 1
		if ev.ItemID != "" {
			if a.toolKeys == nil {
				a.toolKeys = map[string]int{}
			}
			a.toolKeys[ev.ItemID] = a.lastTool
		}
	case SEToolInputJSON:
		if i := a.toolIndex(ev.ItemID, false); i >= 0 {
			a.toolInputs[i].WriteString(ev.Text)
		}
	case SEToolUseStop:
		if i := a.toolIndex(ev.ItemID, true); i >= 0 && ev.Text != "" && a.stopText[i] == "" {
			a.stopText[i] = ev.Text
		}
	case SEMessageDelta:
		if ev.StopReason != "" {
			a.StopReason = ev.StopReason
		}
		a.Usage.Add(ev.Usage)
	case SEMessageStop:
		a.flushText()
		a.flushThinking()
	}
}

func (a *Accumulator) flushText() {
	if a.textBuf != nil {
		if s := a.textBuf.String(); s != "" {
			a.blocks = append(a.blocks, TextBlock(s))
		}
		a.textBuf = nil
	}
}

func (a *Accumulator) flushThinking() {
	if a.thinkBuf != nil {
		if s := a.thinkBuf.String(); s != "" {
			a.blocks = append(a.blocks, ContentBlock{
				Type:      BlockThinking,
				Thinking:  s,
				Signature: a.thinkSig,
			})
		}
		a.thinkBuf = nil
		a.thinkSig = ""
	}
}

// toolIndex resolves which tool call an event belongs to: an exact ItemID
// match first, then — unless exact is required — the most recently started
// call (the legacy single-slot behavior for providers that never set ItemID).
func (a *Accumulator) toolIndex(itemID string, exact bool) int {
	if itemID != "" {
		if i, ok := a.toolKeys[itemID]; ok {
			return i
		}
		if exact {
			return -1
		}
	}
	if a.lastTool >= 0 && a.lastTool < len(a.toolInputs) {
		return a.lastTool
	}
	return -1
}

// Message finalizes and returns the assembled assistant message. Partial tool
// inputs are validated; malformed JSON falls back to "{}".
func (a *Accumulator) Message() Message {
	a.flushText()
	a.flushThinking()
	ti := 0
	out := make([]ContentBlock, len(a.blocks))
	for i, b := range a.blocks {
		if b.Type == BlockToolUse {
			raw := strings.TrimSpace(a.toolInputs[ti].String())
			if raw == "" || !json.Valid([]byte(raw)) {
				if alt := strings.TrimSpace(a.stopText[ti]); alt != "" && json.Valid([]byte(alt)) {
					raw = alt
				}
			}
			ti++
			if raw == "" || !json.Valid([]byte(raw)) {
				raw = "{}"
			}
			b.Input = json.RawMessage(raw)
		}
		out[i] = b
	}
	return Message{Role: RoleAssistant, Content: out}
}
