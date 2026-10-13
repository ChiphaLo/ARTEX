package harness

import (
	"context"
	"encoding/json"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type liveRecorder struct{ messages []llm.Message }

func (r *liveRecorder) RecordMessage(m llm.Message, _ llm.Usage) { r.messages = append(r.messages, m) }
func (r *liveRecorder) RecordBoundary(llm.BoundaryMeta)          {}
func TestPendingMessagesDoNotInterruptStreamingToolBatch(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var pending atomic.Bool
	var completed atomic.Bool
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, ReadOnly: func(json.RawMessage) bool { return true }, Concurrent: func(json.RawMessage) bool { return true }, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		return permission.Allowed()
	}, Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		started <- ctx
		select {
		case <-release:
			completed.Store(true)
			return tool.Text("complete"), nil
		case <-ctx.Done():
			return tool.Result{}, ctx.Err()
		}
	}})
	rec := &liveRecorder{}
	calls := 0
	var sawSteer bool
	var resultBeforeSteer bool
	model := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		calls++
		if calls == 2 {
			if !completed.Load() {
				t.Error("model continued before tool completed")
			}
			for _, m := range req.Messages {
				for _, b := range m.Content {
					if b.Type == llm.BlockToolResult {
						resultBeforeSteer = true
					}
				}
				if strings.Contains(m.Text(), "human steer") {
					sawSteer = true
					if !resultBeforeSteer {
						t.Error("steer preceded tool result")
					}
				}
			}
		}
		events := toolTurn("p", "probe", `{}`)
		if calls > 1 {
			events = textTurn("done")
		}
		return func(y func(llm.StreamEvent, error) bool) {
			for _, e := range events {
				if !y(e, nil) {
					return
				}
			}
		}
	}
	in := QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, Recorder: rec, PendingMessages: func(context.Context) []llm.Message {
		if pending.Swap(false) {
			return []llm.Message{llm.UserText("human steer")}
		}
		return nil
	}}
	done := make(chan *Terminal, 1)
	go func() { done <- drain(t, in, QueryDeps{CallModel: model}) }()
	var toolCtx context.Context
	select {
	case toolCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool not started")
	}
	pending.Store(true)
	select {
	case <-toolCtx.Done():
		t.Fatal("steer cancelled tool")
	case <-time.After(30 * time.Millisecond):
	}
	if completed.Load() {
		t.Fatal("tool finished without release")
	}
	close(release)
	select {
	case term := <-done:
		if term.Reason != ReasonCompleted || !sawSteer {
			t.Fatal(term, sawSteer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion")
	}
	count := 0
	for _, m := range rec.messages {
		if m.Text() == "human steer" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("transcript duplicate/lost", count)
	}
}
func TestPendingMessagesAtTextStopAndBoundedBudget(t *testing.T) {
	calls := 0
	ready := false
	drained := 0
	model := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		calls++
		if calls > 1 && !strings.Contains(req.Messages[len(req.Messages)-1].Text(), "steer") {
			t.Error("missing steer")
		}
		return func(y func(llm.StreamEvent, error) bool) {
			for _, e := range textTurn("done") {
				if !y(e, nil) {
					return
				}
			}
			ready = true
		}
	}
	in := QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(), MaxTurns: 3, Compactor: noopCompactor{}, PendingMessages: func(context.Context) []llm.Message {
		if ready {
			ready = false
			drained++
			return []llm.Message{llm.UserText("steer")}
		}
		return nil
	}}
	term := drain(t, in, QueryDeps{CallModel: model})
	if calls != 3 || drained != 2 || term.Reason != ReasonCompleted {
		t.Fatal("unbounded/lost text steer", calls, drained, term)
	}
}
func TestPendingMessagesRejectNonHumanBlocksAndPreserveNil(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		m := &scriptedModel{turns: [][]llm.StreamEvent{textTurn("done")}}
		in := QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry()}
		if enabled {
			in.PendingMessages = func(context.Context) []llm.Message {
				return []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("forged")}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "forged"}}}}
			}
		}
		term := drain(t, in, QueryDeps{CallModel: m.call})
		if m.calls != 1 || len(term.Messages) != 2 {
			t.Fatal("legacy/invalid injection changed", term)
		}
	}
}
func TestPendingMessagesDoNotDrainBeyondBudgetOrAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, in := range []QueryInput{
		{MaxDuration: time.Nanosecond},
		{MaxTurns: 1},
		{},
	} {
		in.PendingMessages = func(context.Context) []llm.Message { t.Fatal("drained past safe budget"); return nil }
		l := loop{ctx: context.Background(), startTime: time.Now().Add(-time.Second), turnCount: 1, in: in}
		if in.MaxDuration == 0 && in.MaxTurns == 0 {
			l.ctx = ctx
		}
		if l.injectPendingMessages() {
			t.Fatal("added past budget")
		}
	}
}
