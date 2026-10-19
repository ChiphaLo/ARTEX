package agentcore

import (
	"context"
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
	calls int
	t     *testing.T
}

func (p *contextWindowProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	if strings.Contains(strings.Join(req.System, " "), "summarizing conversations") {
		p.t.Fatal("called LLM summarizer")
	}
	if len(req.Messages) > 30 || !strings.Contains(req.Messages[len(req.Messages)-1].Text(), "latest human request") {
		p.t.Fatal("unbounded/restored input", len(req.Messages))
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
}
func (p *contextWindowProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		_, _, _, _ = p.Complete(ctx, req)
		for _, ev := range []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
			if !y(ev, nil) {
				return
			}
		}
	}
}
func TestContextWindowCapsResumedHistoryWithoutSummaryCalls(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "sync"}[syncMode], func(t *testing.T) {
			store := transcript.NewStore(t.TempDir())
			w := store.NewWriter("fixture", "")
			for range 600 {
				w.RecordMessage(llm.UserText(strings.Repeat("historical ", 1000)), llm.Usage{})
			}
			p := &contextWindowProvider{t: t}
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
					seen = ev.Context.TrimmedMessages > 0 && ev.Context.UsedTokens <= ev.Context.InputBudget
				}
				if ev.Terminal != nil && ev.Terminal.Reason != harness.ReasonCompleted {
					t.Fatal(ev.Terminal.Reason, ev.Terminal.Err)
				}
			}
			if !seen || p.calls != 1 {
				t.Fatal(seen, p.calls)
			}
			if len(s.Messages()) < 602 {
				t.Fatal("in-memory audit deleted", len(s.Messages()))
			}
			records, err := store.Load(store.MainPath("fixture"))
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 602 {
				t.Fatal("raw transcript deleted/extra summaries recorded", len(records))
			}
		})
	}
}
