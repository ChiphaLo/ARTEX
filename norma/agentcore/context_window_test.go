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

func TestResumeLatestRunDoesNotInflateWithIndependentPlannerAudit(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	for range 106 {
		w := store.NewWriter("planner", "")
		for range 6 {
			w.RecordMessage(llm.UserText(strings.Repeat("old round ", 1000)), llm.Usage{})
		}
	}
	w := store.NewWriter("planner", "")
	w.RecordMessage(llm.UserText("active round"), llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("saved work")}}, llm.Usage{})
	recs, _ := store.Load(store.MainPath("planner"))
	before := len(recs)
	p := &contextWindowProvider{t: t}
	opts := Options{Provider: p, Transcript: store, SessionID: "planner", Tools: []tool.CoreTool{}, DisableBackgroundTasks: true, ContextWindow: 20000, NonStreaming: true}
	active := NewSession(opts)
	defer active.Close()
	if err := active.ResumeLatestRun("planner"); err != nil {
		t.Fatal(err)
	}
	if len(active.Messages()) != 2 {
		t.Fatal("old rounds leaked", len(active.Messages()))
	}
	for _, err := range active.Prompt(t.Context(), "latest human request") {
		if err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 1 || p.summaryCalls != 0 {
		t.Fatal("unnecessary compression", p.calls, p.summaryCalls)
	}
	fresh := NewSession(opts)
	defer fresh.Close()
	if err := fresh.ResumeLatestRun("planner"); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Messages()) != 4 {
		t.Fatal("continue lost latest run", len(fresh.Messages()))
	}
	recs, _ = store.Load(store.MainPath("planner"))
	if len(recs) != before+2 {
		t.Fatal("audit removed", len(recs), before)
	}
	// Ordinary chat resume behavior is intentionally unchanged.
	continuous := NewSession(opts)
	defer continuous.Close()
	if err := continuous.Resume("planner"); err != nil {
		t.Fatal(err)
	}
	if len(continuous.Messages()) != len(recs) {
		t.Fatal("ordinary resume changed")
	}
}
