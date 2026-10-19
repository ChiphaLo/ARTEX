package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestManagedRetryIncludesFullUpstreamError(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprint(syncMode), func(t *testing.T) {
			upstream := "openai-responses: status 503: {\n  \"code\":\"upstream_unavailable\",\n  \"detail\":\"" + strings.Repeat("diagnostic ", 700) + "\",\n  \"requestId\":\"fixture-request-id\"\n}"
			calls, retries := 0, 0
			fail := func() bool { calls++; return calls == 1 }
			deps := QueryDeps{
				CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					return func(y func(llm.StreamEvent, error) bool) {
						if fail() {
							y(llm.StreamEvent{}, errors.New(upstream))
							return
						}
						for _, e := range textTurn("recovered") {
							if !y(e, nil) {
								return
							}
						}
					}
				},
				CallModelSync: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					if fail() {
						return llm.Message{}, "", llm.Usage{}, errors.New(upstream)
					}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("recovered")}}, "end_turn", llm.Usage{}, nil
				},
			}
			ctx := WithModelRetry(t.Context(), ModelRetryPolicy{Retries: 5, Retryable: func(error) bool { return true }})
			for e, err := range Query(ctx, QueryInput{NonStreaming: syncMode}, deps) {
				if err != nil {
					t.Fatal(err)
				}
				if e.Kind == KindRetry {
					retries++
					if !strings.HasSuffix(e.Text, upstream) || !strings.Contains(e.Text, "（1/5）") {
						t.Fatal("lost raw upstream error", e.Text)
					}
				}
				if e.Terminal != nil && e.Terminal.Reason != ReasonCompleted {
					t.Fatal(e.Terminal)
				}
			}
			if calls != 2 || retries != 1 {
				t.Fatal(calls, retries)
			}
		})
	}
}

func TestManagedRetryLimitAndCancel(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		policy := ModelRetryPolicy{Retries: 5, Retryable: func(error) bool { return true }}
		if cancelWait {
			policy.Delay = 20 * time.Second
		}
		ctx = WithModelRetry(ctx, policy)
		calls, retries, terminals := 0, 0, 0
		model := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(y func(llm.StreamEvent, error) bool) { calls++; y(llm.StreamEvent{}, errors.New("reset")) }
		}
		start := time.Now()
		for ev, err := range Query(ctx, QueryInput{Messages: []llm.Message{llm.UserText("fixture")}}, QueryDeps{CallModel: model}) {
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind == KindRetry {
				retries++
				if cancelWait {
					cancel()
				}
			}
			if ev.Kind == KindResult {
				terminals++
				if cancelWait && ev.Terminal.Reason != ReasonAbortedStreaming {
					t.Fatal(ev.Terminal)
				}
			}
		}
		cancel()
		if terminals != 1 {
			t.Fatalf("terminal count=%d", terminals)
		}
		if cancelWait {
			if calls != 1 || time.Since(start) > time.Second {
				t.Fatal("cancellation did not interrupt 20s backoff")
			}
		} else if calls != 6 || retries != 5 {
			t.Fatalf("calls=%d retries=%d", calls, retries)
		}
	}
}

func TestManagedRetryRequiresTerminalSchema(t *testing.T) {
	ctx := WithModelRetry(t.Context(), ModelRetryPolicy{Retries: 5, Retryable: func(error) bool { return true }})
	calls := 0
	model := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(y func(llm.StreamEvent, error) bool) {
			calls++
			y(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil)
		}
	}
	var terminal *Terminal
	for ev, _ := range Query(ctx, QueryInput{Messages: []llm.Message{llm.UserText("fixture")}}, QueryDeps{CallModel: model}) {
		if ev.Kind == KindResult {
			terminal = ev.Terminal
		}
	}
	if calls != 6 || terminal == nil || terminal.Reason != ReasonModelError {
		t.Fatalf("calls=%d terminal=%+v", calls, terminal)
	}
}

func TestManagedRetryPreservesExecutedTools(t *testing.T) {
	var executions atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		return permission.Allowed()
	}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		executions.Add(1)
		return tool.Text("once"), nil
	}})
	calls := 0
	model := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(y func(llm.StreamEvent, error) bool) {
			calls++
			if calls == 1 {
				for _, ev := range []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "complete", ToolName: "probe"}, {Type: llm.SEToolInputJSON, Text: "{}"}, {Type: llm.SEToolUseStop}, {Type: llm.SEToolUseStart, ToolID: "partial", ToolName: "probe"}, {Type: llm.SEToolInputJSON, Text: `{"unfinished":`}} {
					if !y(ev, nil) {
						return
					}
				}
				y(llm.StreamEvent{}, errors.New("reset"))
				return
			}
			if len(req.Messages) != 3 || req.Messages[1].Content[0].ID != "complete" || req.Messages[2].Content[0].ToolUseID != "complete" {
				t.Errorf("lost call/result pair: %+v", req.Messages)
			}
			for _, ev := range textTurn("done") {
				if !y(ev, nil) {
					return
				}
			}
		}
	}
	ctx := WithModelRetry(t.Context(), ModelRetryPolicy{Retries: 5, Retryable: func(error) bool { return true }})
	for _, err := range Query(ctx, QueryInput{Messages: []llm.Message{llm.UserText("fixture")}, Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: model}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || executions.Load() != 1 {
		t.Fatalf("calls=%d executions=%d", calls, executions.Load())
	}
}

func TestManagedRetryWaitHonorsWallClockBudget(t *testing.T) {
	ctx := WithModelRetry(t.Context(), ModelRetryPolicy{Retries: 5, Delay: 20 * time.Second, Retryable: func(error) bool { return true }})
	calls := 0
	model := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(y func(llm.StreamEvent, error) bool) { calls++; y(llm.StreamEvent{}, errors.New("reset")) }
	}
	start := time.Now()
	var reason TerminalReason
	for ev, _ := range Query(ctx, QueryInput{MaxDuration: 20 * time.Millisecond}, QueryDeps{CallModel: model}) {
		if ev.Terminal != nil {
			reason = ev.Terminal.Reason
		}
	}
	if calls != 1 || reason != ReasonTimeout || time.Since(start) > time.Second {
		t.Fatalf("retry ignored duration: calls=%d reason=%s elapsed=%s", calls, reason, time.Since(start))
	}
}

func TestManagedRetryEmptyAndUnterminatedCompletions(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprint(syncMode), func(t *testing.T) {
			ctx := WithModelRetry(t.Context(), ModelRetryPolicy{Retries: 5, Retryable: func(error) bool { return true }})
			calls := 0
			deps := QueryDeps{
				CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					return func(y func(llm.StreamEvent, error) bool) {
						calls++
						y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
					}
				},
				CallModelSync: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					return llm.Message{Role: llm.RoleAssistant}, "end_turn", llm.Usage{InputTokens: 3}, nil
				},
			}
			var terminal *Terminal
			for ev, _ := range Query(ctx, QueryInput{NonStreaming: syncMode}, deps) {
				if ev.Terminal != nil {
					terminal = ev.Terminal
				}
			}
			if calls != 6 || terminal == nil || terminal.Reason != ReasonModelError {
				t.Fatalf("empty reply was completed: calls=%d terminal=%+v", calls, terminal)
			}
			if syncMode && terminal.Usage.InputTokens != 18 {
				t.Fatal("retry usage lost", terminal.Usage)
			}
		})
	}
}
