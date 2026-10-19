package harness

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestContextWindowTrimsOldestWithoutChangingHistory(t *testing.T) {
	req := llm.CompletionRequest{System: []string{"Keep scope and approval restrictions."}, MaxTokens: 500}
	for i := range 500 {
		req.Messages = append(req.Messages, llm.UserText(strings.Repeat("old history ", 200)+string(rune('a'+i%20))))
	}
	req.Messages = append(req.Messages, llm.UserText("latest human request"))
	before, _ := json.Marshal(req)
	got, usage, err := trimContextRequest(req, 16000, 0)
	if err != nil || usage.TrimmedMessages == 0 || usage.UsedTokens > usage.InputBudget {
		t.Fatal(usage, err)
	}
	if got.Messages[len(got.Messages)-1].Text() != "latest human request" || got.System[0] != req.System[0] {
		t.Fatal("newest input/system lost")
	}
	if len(got.Messages)+usage.TrimmedMessages != len(req.Messages) {
		t.Fatal("incorrect trim accounting")
	}
	first := len(req.Messages) - len(got.Messages)
	for i := range got.Messages {
		if got.Messages[i].Text() != req.Messages[first+i].Text() {
			t.Fatal("not FIFO")
		}
	}
	after, _ := json.Marshal(req)
	if string(after) != string(before) {
		t.Fatal("durable history mutated")
	}
}

func TestContextWindowKeepsToolExchangesAndLatestRequest(t *testing.T) {
	use := func(id string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "probe", Input: json.RawMessage(`{"scope":"unchanged"}`)}}}
	}
	result := func(id, text string) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, text, false)}}
	}
	req := llm.CompletionRequest{System: []string{"fixed instructions"}, MaxTokens: 100, Messages: []llm.Message{
		llm.UserText("protected system reminder"), llm.UserText(strings.Repeat("old ", 12000)),
		llm.UserText("current user scope"), use("old"), result("old", strings.Repeat("old output ", 3000)),
		use("latest"), result("latest", "latest outcome"),
	}}
	got, usage, err := trimContextRequest(req, 4000, 1)
	if err != nil || usage.TrimmedMessages != 3 {
		t.Fatal(usage, err)
	}
	if got.Messages[0].Text() != "protected system reminder" || got.Messages[1].Text() != "current user scope" {
		t.Fatal("reminder/latest request lost")
	}
	if got.Messages[2].Content[0].ID != "latest" || got.Messages[3].Content[0].ToolUseID != "latest" {
		t.Fatal("newest exchange damaged")
	}
	if string(got.Messages[2].Content[0].Input) != `{"scope":"unchanged"}` {
		t.Fatal("tool input changed")
	}
	paired := llm.PairToolBlocks(got.Messages)
	if len(paired) != len(got.Messages) {
		t.Fatal("orphaned tool block")
	}
}

func TestContextWindowCountsFixedSchemasAndRefusesOversizedLatestInput(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		req := llm.CompletionRequest{MaxTokens: 100, Messages: []llm.Message{llm.UserText("latest")}}
		if fixed {
			req.Tools = []llm.ToolSchema{{Name: "huge", Description: strings.Repeat("schema ", 3000)}}
		} else {
			req.Messages[0] = llm.UserText(strings.Repeat("latest input ", 3000))
		}
		before, _ := json.Marshal(req)
		_, usage, err := trimContextRequest(req, 2000, 0)
		if !errors.Is(err, ErrContextWindow) || usage.UsedTokens <= usage.InputBudget {
			t.Fatal(usage, err)
		}
		after, _ := json.Marshal(req)
		if string(before) != string(after) {
			t.Fatal("oversized latest content silently cut")
		}
	}
}

func TestContextWindowDropsParallelExchangeAsOneGroup(t *testing.T) {
	req := llm.CompletionRequest{MaxTokens: 100, Messages: []llm.Message{
		llm.UserText(strings.Repeat("old ", 2000)),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "a", Name: "probe", Input: json.RawMessage(`{}`)},
			{Type: llm.BlockToolUse, ID: "b", Name: "probe", Input: json.RawMessage(`{}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("a", strings.Repeat("old result ", 2000), false)}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("b", "result b", false)}},
		llm.UserText("latest request"),
	}}
	got, usage, err := trimContextRequest(req, 1000, 0)
	if err != nil || usage.TrimmedMessages != 4 || len(got.Messages) != 1 || got.Messages[0].Text() != "latest request" {
		t.Fatal("parallel exchange was split", usage, err)
	}
}

func TestContextWindowBelowLimitAndOutputReserve(t *testing.T) {
	req := llm.CompletionRequest{System: []string{"instructions"}, Messages: []llm.Message{llm.UserText("small")}, MaxTokens: 1000}
	got, usage, err := trimContextRequest(req, 5000, 0)
	if err != nil || usage.TrimmedMessages != 0 || got.Messages[0].Text() != "small" || usage.ReservedTokens != 1250 || usage.InputBudget != 3750 {
		t.Fatal(usage, err)
	}
	req.MaxTokens = 6000
	if _, _, err := trimContextRequest(req, 5000, 0); !errors.Is(err, ErrContextWindow) {
		t.Fatal("output budget not counted", err)
	}
}

func TestContextWindowFailureDoesNotCallProviderOrRetry(t *testing.T) {
	m := &scriptedModel{}
	ctx := WithModelRetry(t.Context(), ModelRetryPolicy{Retries: 5, Retryable: func(error) bool { return true }})
	contexts, retries := 0, 0
	var terminal *Terminal
	for ev, err := range Query(ctx, QueryInput{ContextWindow: 1000, Messages: []llm.Message{llm.UserText(strings.Repeat("large", 10000))}}, QueryDeps{CallModel: m.call}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == KindContextUsage {
			contexts++
		}
		if ev.Kind == KindRetry {
			retries++
		}
		if ev.Terminal != nil {
			terminal = ev.Terminal
		}
	}
	if m.calls != 0 || contexts != 1 || retries != 0 || terminal == nil || terminal.Reason != ReasonPromptTooLong {
		t.Fatal(m.calls, contexts, retries, terminal)
	}
}
