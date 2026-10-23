package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
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

func TestContextWindowCompressesAt90AndTrimsOnlyPast100(t *testing.T) {
	for _, total := range []int{89999, 90000, 95000, 100000, 100001} {
		for _, fail := range []bool{false, true} {
			for _, syncMode := range []bool{false, true} {
				t.Run(fmt.Sprintf("budget=%d/fail=%v/sync=%v", total, fail, syncMode), func(t *testing.T) {
					const window = 100000
					registry := tool.NewRegistry(tool.NewBash())
					messages := []llm.Message{llm.UserText("x")}
					for range 11 {
						messages = append(messages, llm.UserText(strings.Repeat("old ", 100)))
					}
					messages = append(messages, llm.UserText("latest human request"))
					req := llm.CompletionRequest{System: []string{"fixed instructions"}, MaxTokens: 1000, Tools: registry.Schemas(),
						Messages: append([]llm.Message{llm.UserText("system reminder")}, messages...)}
					size, err := llm.EstimateRequestBytes(req)
					if err != nil {
						t.Fatal(err)
					}
					// Exact byte geometry exercises both boundary values, not approximate fixtures.
					messages[0] = llm.UserText(strings.Repeat("x", 1+(total-6000)*3-size))
					req.Messages = append([]llm.Message{llm.UserText("system reminder")}, messages...)
					measured, err := measureContextRequest(req, window)
					if err != nil || measured.UsedTokens+measured.ReservedTokens != total {
						t.Fatal(measured, err)
					}
					original, _ := json.Marshal(messages)
					summaryCalls := 0
					c := compaction.New(compaction.Config{ContextWindow: window, RequestThresholdPercent: 90}, func(context.Context, []llm.Message) (string, error) {
						summaryCalls++
						if fail {
							return "", errors.New("fixture summary failure")
						}
						return "compressed old history", nil
					})
					var sent llm.CompletionRequest
					deps := QueryDeps{
						CallModel: func(_ context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
							sent = r
							return func(y func(llm.StreamEvent, error) bool) {
								for _, ev := range textTurn("done") {
									if !y(ev, nil) {
										return
									}
								}
							}
						},
						CallModelSync: func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
							sent = r
							return llm.UserText("done"), "end_turn", llm.Usage{}, nil
						},
					}
					var usage ContextUsage
					var terminal *Terminal
					for ev, err := range Query(t.Context(), QueryInput{System: req.System, SystemReminder: "system reminder", Messages: messages, Tools: registry,
						MaxTokens: 1000, ContextWindow: window, Compactor: c, NonStreaming: syncMode}, deps) {
						if err != nil {
							t.Fatal(err)
						}
						if ev.Context != nil {
							usage = *ev.Context
						}
						if ev.Terminal != nil {
							terminal = ev.Terminal
						}
					}
					if (summaryCalls > 0) != (total >= 90000) {
						t.Fatal("wrong 90% summary gate", summaryCalls)
					}
					if (usage.TrimmedMessages > 0) != (total > 100000 && fail) {
						t.Fatal("trim before overflow or before summary", usage)
					}
					if terminal == nil || terminal.Reason != ReasonCompleted || usage.UsedTokens+usage.ReservedTokens > window {
						t.Fatal(terminal, usage)
					}
					if sent.Messages[0].Text() != "system reminder" || sent.Messages[len(sent.Messages)-1].Text() != "latest human request" {
						t.Fatal("protected content lost")
					}
					after, _ := json.Marshal(messages)
					if string(after) != string(original) {
						t.Fatal("original history changed")
					}
				})
			}
		}
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

func TestCompactionPublishesPendingContextBeforeSummaryStarts(t *testing.T) {
	beforeSeen, summaryCalled := false, false
	c := compaction.New(compaction.Config{ContextWindow: 10000, RequestThresholdPercent: 90}, func(_ context.Context, _ []llm.Message) (string, error) {
		summaryCalled = true
		if !beforeSeen {
			t.Fatal("UI kept old request until after compaction")
		}
		return "small digest", nil
	})
	m := &scriptedModel{turns: [][]llm.StreamEvent{textTurn("done")}}
	in := QueryInput{Messages: []llm.Message{llm.UserText(strings.Repeat("old", 10000)), llm.UserText("latest human request")}, Tools: tool.NewRegistry(), MaxTokens: 1000, ContextWindow: 10000, Compactor: c}
	var last *ContextUsage
	for ev, err := range Query(t.Context(), in, QueryDeps{CallModel: m.call}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Context != nil {
			last = ev.Context
			if last.Phase == "before_compaction" {
				beforeSeen = true
				if last.UsedTokens+last.ReservedTokens < 9000 {
					t.Fatal(last)
				}
			}
		}
	}
	if !summaryCalled || last == nil || last.Phase != "" || last.UsedTokens+last.ReservedTokens >= 9000 {
		t.Fatal(summaryCalled, last)
	}
}
