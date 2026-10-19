package agentcore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

type contextWindowProvider struct {
	calls, summaryCalls int
	failSummary         bool
	t                   *testing.T
}

func (p *contextWindowProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if strings.Contains(strings.Join(req.System, " "), "summarizing conversations") {
		p.summaryCalls++
		size, err := llm.EstimateRequestBytes(req)
		if err != nil || (size+2)/3+req.MaxTokens+1000 > 20000 {
			p.t.Fatal("oversized summary request", size, err)
		}
		if p.failSummary {
			return llm.Message{}, "", llm.Usage{}, errors.New("fixture summary unavailable")
		}
		return llm.UserText("digest of prior history"), "end_turn", llm.Usage{}, nil
	}
	p.calls++
	size, err := llm.EstimateRequestBytes(req)
	if err != nil || (size+2)/3+req.MaxTokens+1000 > 20000 || !strings.Contains(req.Messages[len(req.Messages)-1].Text(), "latest human request") {
		p.t.Fatal("unbounded/restored input", len(req.Messages))
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
}
func (p *contextWindowProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		msg, stop, _, err := p.Complete(ctx, req)
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for _, ev := range []llm.StreamEvent{{Type: llm.SETextDelta, Text: msg.Text()}, {Type: llm.SEMessageDelta, StopReason: stop}, {Type: llm.SEMessageStop}} {
			if !y(ev, nil) {
				return
			}
		}
	}
}
func TestContextWindowCapsResumedHistoryWithSummaryThenOverflowFallback(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("sync=%v/fail=%v", syncMode, fail), func(t *testing.T) {
				store := transcript.NewStore(t.TempDir())
				w := store.NewWriter("fixture", "")
				for range 600 {
					w.RecordMessage(llm.UserText(strings.Repeat("historical ", 100)), llm.Usage{})
				}
				p := &contextWindowProvider{t: t, failSummary: fail}
				s := NewSession(Options{Provider: p, Transcript: store, SessionID: "fixture", Tools: []tool.CoreTool{}, DisableBackgroundTasks: true,
					Compaction: &compaction.Config{ContextWindow: 20000}, ContextWindow: 20000, NonStreaming: syncMode})
				defer s.Close()
				if err := s.Resume("fixture"); err != nil {
					t.Fatal(err)
				}
				seen := false
				for ev, err := range s.Prompt(t.Context(), "latest human request") {
					if err != nil {
						t.Fatal(err)
					}
					if ev.Context != nil {
						seen = (ev.Context.TrimmedMessages > 0) == fail && ev.Context.UsedTokens <= ev.Context.InputBudget
					}
					if ev.Terminal != nil && ev.Terminal.Reason != harness.ReasonCompleted {
						t.Fatal(ev.Terminal.Reason, ev.Terminal.Err)
					}
				}
				if !seen || p.calls != 1 || p.summaryCalls == 0 {
					t.Fatal(seen, p.calls, p.summaryCalls)
				}
				if len(s.Messages()) < 602 {
					t.Fatal("in-memory audit deleted", len(s.Messages()))
				}
				records, err := store.Load(store.MainPath("fixture"))
				if err != nil {
					t.Fatal(err)
				}
				if len(records) < 602 {
					t.Fatal("raw transcript deleted", len(records))
				}
				if !fail {
					calls := p.summaryCalls
					cold := NewSession(Options{Provider: p, Transcript: store, SessionID: "fixture", Tools: []tool.CoreTool{}, DisableBackgroundTasks: true, ContextWindow: 20000, NonStreaming: syncMode})
					defer cold.Close()
					if err := cold.Resume("fixture"); err != nil {
						t.Fatal(err)
					}
					for _, err := range cold.Prompt(t.Context(), "latest human request after restart") {
						if err != nil {
							t.Fatal(err)
						}
					}
					if p.summaryCalls != calls {
						t.Fatal("cold resume replayed summarized raw history")
					}
				}
			})
		}
	}
}
