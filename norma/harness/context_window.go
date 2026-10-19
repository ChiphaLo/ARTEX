package harness

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Autumn-27/norma/llm"
)

// An estimate, not a model tokenizer. Include serialized UTF-8 bytes and framing.
const contextBytesPerToken = 3

var ErrContextWindow = errors.New("prompt is too long for the configured context window")

type ContextUsage struct {
	UsedTokens      int  `json:"used_tokens"`
	WindowTokens    int  `json:"window_tokens"`
	InputBudget     int  `json:"input_budget"`
	ReservedTokens  int  `json:"reserved_tokens"`
	TrimmedMessages int  `json:"trimmed_messages"`
	Estimated       bool `json:"estimated"`
}

type contextGroup struct {
	start, end, bytes int
	pinned            bool
}

// Project only: retain fixed instructions/tools, the reminder, newest user input
// and latest exchange. Never cut tool arguments or retain only half an exchange.
func trimContextRequest(req llm.CompletionRequest, window, protectedPrefix int) (llm.CompletionRequest, ContextUsage, error) {
	usage := ContextUsage{WindowTokens: window, Estimated: true}
	output := req.MaxTokens
	if output <= 0 {
		output = min(8192, window/5)
	}
	usage.ReservedTokens = output + max(1, window/20)
	usage.InputBudget = window - usage.ReservedTokens
	msgs := llm.PairToolBlocks(req.Messages)
	latestUser := -1
	for i, msg := range msgs {
		if msg.Role == llm.RoleUser && msg.Text() != "" {
			latestUser = i
		}
	}
	open := map[string]bool{}
	groups := []contextGroup{}
	start, size := 0, 0
	for i, msg := range msgs {
		raw, err := json.Marshal(msg)
		if err != nil {
			return req, usage, fmt.Errorf("%w: cannot size message: %v", ErrContextWindow, err)
		}
		size += len(raw) + 13 // separator plus role/framing allowance
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolUse {
				open[b.ID] = true
			}
			if b.Type == llm.BlockToolResult {
				delete(open, b.ToolUseID)
			}
		}
		if len(open) == 0 || i == len(msgs)-1 {
			groups = append(groups, contextGroup{start: start, end: i + 1, bytes: size,
				pinned: start < protectedPrefix || (latestUser >= start && latestUser <= i) || i == len(msgs)-1})
			start, size = i+1, 0
		}
	}
	fixed := req
	fixed.Messages = nil
	raw, err := json.Marshal(fixed)
	if err != nil {
		return req, usage, fmt.Errorf("%w: cannot size system/tools: %v", ErrContextWindow, err)
	}
	total := len(raw) + 64
	for _, g := range groups {
		total += g.bytes
	}
	limit := usage.InputBudget * contextBytesPerToken
	kept := make([]llm.Message, 0, len(msgs))
	for _, g := range groups {
		if total > limit && !g.pinned {
			total -= g.bytes
			usage.TrimmedMessages += g.end - g.start
			continue
		}
		kept = append(kept, msgs[g.start:g.end]...)
	}
	usage.UsedTokens = (total + contextBytesPerToken - 1) / contextBytesPerToken
	if total > limit {
		return req, usage, fmt.Errorf("%w: newest input/exchange and fixed instructions need approximately %d input tokens; available %d of %d (output/safety reserve %d). 请缩短当前输入/附件或工具输出，或调整模型上下文配置；未截断最新请求、工具参数或系统约束。", ErrContextWindow, usage.UsedTokens, max(0, usage.InputBudget), window, usage.ReservedTokens)
	}
	req.Messages = kept
	return req, usage, nil
}
