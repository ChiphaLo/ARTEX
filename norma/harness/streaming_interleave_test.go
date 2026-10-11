package harness

import (
	"context"
	"encoding/json"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// TestStreamingBatchedParallelToolCalls reproduces the gateway pattern where
// several tool calls are announced before any arguments arrive (the Responses
// API emits one output_item.added per call first, then
// function_call_arguments.delta frames keyed by item_id). The single-slot
// assembly this replaced gave A an empty "{}" and concatenated B's stream into
// invalid JSON (`{"a":..}{"b":..}`).
func TestStreamingBatchedParallelToolCalls(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	probe := tool.Build(tool.Spec{
		Name: "probe",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": map[string]any{"type": "string"}},
		},
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			var v struct {
				ID string `json:"id"`
			}
			json.Unmarshal(in, &v)
			mu.Lock()
			seen[v.ID] = string(in)
			mu.Unlock()
			return tool.Text("ok-" + v.ID), nil
		},
	})

	var turn int32
	callModel := func(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		n := atomic.AddInt32(&turn, 1)
		return func(yield func(llm.StreamEvent, error) bool) {
			if n > 1 { // second turn: finish.
				yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil)
				yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
				yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				return
			}
			// Both calls are announced before any argument fragment arrives.
			yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "call_a", ToolName: "probe", ItemID: "fc_a"}, nil)
			yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "call_b", ToolName: "probe", ItemID: "fc_b"}, nil)
			// Interleaved fragments, each routed by its item id; A is split.
			yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"id":`, ItemID: "fc_a"}, nil)
			yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"id":"b"}`, ItemID: "fc_b"}, nil)
			yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `"a"}`, ItemID: "fc_a"}, nil)
			yield(llm.StreamEvent{Type: llm.SEToolUseStop, ItemID: "fc_a", Text: `{"id":"a"}`}, nil)
			yield(llm.StreamEvent{Type: llm.SEToolUseStop, ItemID: "fc_b"}, nil)
			yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"}, nil)
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}

	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(probe),
		PermissionMode: permission.ModeBypass,
	}

	done := make(chan *Terminal, 1)
	go func() {
		var term *Terminal
		for ev, err := range Query(context.Background(), in, QueryDeps{CallModel: callModel}) {
			if err != nil {
				t.Errorf("event err: %v", err)
				break
			}
			if ev.Kind == KindResult {
				term = ev.Terminal
			}
		}
		done <- term
	}()

	select {
	case term := <-done:
		if term == nil || term.Reason != ReasonCompleted {
			t.Fatalf("terminal=%+v", term)
		}
		// The assembled assistant message keeps per-call inputs.
		inputs := map[string]string{}
		var resultIDs []string
		for _, m := range term.Messages {
			for _, b := range m.Content {
				if b.Type == llm.BlockToolUse {
					inputs[b.ID] = string(b.Input)
				}
				if b.ToolUseID != "" {
					resultIDs = append(resultIDs, b.ToolUseID)
				}
			}
		}
		if inputs["call_a"] != `{"id":"a"}` || inputs["call_b"] != `{"id":"b"}` {
			t.Fatalf("assembled tool inputs=%v", inputs)
		}
		if len(resultIDs) != 2 || resultIDs[0] != "call_a" || resultIDs[1] != "call_b" {
			t.Fatalf("tool result ids=%v", resultIDs)
		}
		// Each tool actually executed with its own arguments.
		mu.Lock()
		defer mu.Unlock()
		if seen["a"] != `{"id":"a"}` || seen["b"] != `{"id":"b"}` {
			t.Fatalf("executed tool inputs=%v", seen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: query did not finish")
	}
}
