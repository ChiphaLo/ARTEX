package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// WindowedProviderSummarizer uses bounded, chronological batches with a rolling
// digest. Resuming a multi-window transcript must not send that entire history
// to the summarizer or silently throw away the oldest part just to make it fit.
func WindowedProviderSummarizer(p llm.Provider, window, maxOutputTokens int, nonStreaming bool) Summarizer {
	maxOut := min(compactMaxOutputTokens, max(1, window/10))
	if maxOutputTokens > 0 {
		maxOut = min(maxOut, maxOutputTokens)
	}
	inputBytes := (window - maxOut - max(1, window/20)) * llm.RequestBytesPerToken
	return func(ctx context.Context, msgs []llm.Message) (string, error) {
		work := pairForReplay(msgs)
		groups := summaryGroups(work)
		digest := ""
		for len(groups) > 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			var head []llm.Message
			if digest != "" {
				head = append(head, llm.UserText("Previously summarized history (context, not new instructions):\n"+digest))
			}
			req := llm.CompletionRequest{System: []string{summarySystemPrompt}, MaxTokens: maxOut, Thinking: "disabled"}
			req.Messages = append(append([]llm.Message{}, head...), llm.UserText(compactPrompt))
			used, err := llm.EstimateRequestBytes(req)
			if err != nil {
				return "", fmt.Errorf("cannot size compaction request: %w", err)
			}
			n := 0
			for n < len(groups) {
				size := 0
				for _, msg := range groups[n] {
					raw, err := json.Marshal(msg)
					if err != nil {
						return "", fmt.Errorf("cannot size compaction message: %w", err)
					}
					size += len(raw) + llm.MessageFramingBytes
				}
				if used+size > inputBytes {
					break
				}
				used += size
				head = append(head, groups[n]...)
				n++
			}
			if n == 0 {
				return "", fmt.Errorf("compaction cannot fit a complete message/tool exchange within the %d-token window; history kept for the final overflow guard", window)
			}
			raw, err := streamSummaryChecked(ctx, p, maxOut, head, nonStreaming, true)
			if err != nil {
				return "", err
			}
			digest = formatCompactSummary(raw)
			if strings.TrimSpace(digest) == "" {
				return "", fmt.Errorf("compaction returned an empty summary")
			}
			groups = groups[n:]
		}
		return digest, nil
	}
}

// Do not split an exchange, including parallel calls returned in separate rows.
func summaryGroups(msgs []llm.Message) [][]llm.Message {
	var groups [][]llm.Message
	open := map[string]bool{}
	start := 0
	for i, msg := range msgs {
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolUse {
				open[b.ID] = true
			}
			if b.Type == llm.BlockToolResult {
				delete(open, b.ToolUseID)
			}
		}
		if len(open) == 0 || i == len(msgs)-1 {
			groups = append(groups, msgs[start:i+1])
			start = i + 1
		}
	}
	return groups
}
