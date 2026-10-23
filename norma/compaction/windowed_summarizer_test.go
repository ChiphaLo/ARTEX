package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

type windowSummaryProvider struct {
	reqs                   []llm.CompletionRequest
	stop                   string
	empty, missingTerminal bool
}

func (p *windowSummaryProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.reqs = append(p.reqs, req)
	text := "rolling digest"
	if p.empty {
		text = ""
	}
	return llm.UserText(text), p.stop, llm.Usage{}, nil
}

func (p *windowSummaryProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		msg, stop, _, _ := p.Complete(ctx, req)
		if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: msg.Text()}, nil) {
			return
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, nil) {
			return
		}
		if !p.missingTerminal {
			y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func TestWindowedSummaryBatchesEveryOldMessageWithoutOverrunning(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprint(syncMode), func(t *testing.T) {
			p := &windowSummaryProvider{stop: "end_turn"}
			var messages []llm.Message
			for i := range 100 {
				messages = append(messages, llm.UserText(fmt.Sprintf("event-%03d:", i)+strings.Repeat("x", 6000)))
			}
			before, _ := json.Marshal(messages)
			text, err := WindowedProviderSummarizer(p, 20000, 0, syncMode)(t.Context(), messages)
			if err != nil || text != "rolling digest" || len(p.reqs) < 2 {
				t.Fatal(text, err, len(p.reqs))
			}
			next := 0
			for i, req := range p.reqs {
				b, err := llm.EstimateRequestBytes(req)
				if err != nil || (b+2)/3+req.MaxTokens+1000 > 20000 {
					t.Fatal("summary request exceeded total budget", b, err)
				}
				if req.Thinking != "disabled" || len(req.Tools) > 0 {
					t.Fatal("summarizer can execute tools")
				}
				if i > 0 && !strings.Contains(req.Messages[0].Text(), "rolling digest") {
					t.Fatal("earlier digest lost")
				}
				for _, m := range req.Messages {
					if strings.HasPrefix(m.Text(), "event-") {
						if !strings.HasPrefix(m.Text(), fmt.Sprintf("event-%03d:", next)) {
							t.Fatal("oldest content skipped or reordered", next)
						}
						next++
					}
				}
			}
			if next != 100 {
				t.Fatal("history not fully summarized", next)
			}
			after, _ := json.Marshal(messages)
			if string(before) != string(after) {
				t.Fatal("raw history modified")
			}
		})
	}
}

func TestWindowedSummaryRejectsOversizedExchangeAndIncompleteReplies(t *testing.T) {
	p := &windowSummaryProvider{stop: "end_turn"}
	if _, err := WindowedProviderSummarizer(p, 20000, 0, true)(t.Context(), []llm.Message{llm.UserText(strings.Repeat("huge", 30000))}); err == nil || len(p.reqs) != 0 {
		t.Fatal("oversized summary was sent", err)
	}
	for _, syncMode := range []bool{false, true} {
		for _, test := range []string{"empty", "truncated", "missing_terminal"} {
			if syncMode && test == "missing_terminal" {
				continue
			}
			t.Run(fmt.Sprintf("sync=%v/%s", syncMode, test), func(t *testing.T) {
				p := &windowSummaryProvider{stop: "end_turn", empty: test == "empty", missingTerminal: test == "missing_terminal"}
				if test == "truncated" {
					p.stop = "max_tokens"
				}
				_, err := WindowedProviderSummarizer(p, 20000, 0, syncMode)(t.Context(), []llm.Message{llm.UserText("history")})
				if err == nil || len(p.reqs) != maxStreamingRetries {
					t.Fatal("invalid summary accepted", err, len(p.reqs))
				}
			})
		}
	}
}

func TestRequestThresholdPreservesLatestUserAndParallelExchange(t *testing.T) {
	msgs := []llm.Message{llm.UserText("older history"), llm.UserText("latest human scope"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "a", Name: "Read"}, {Type: llm.BlockToolUse, ID: "b", Name: "Read"}}},
		toolResult("a"), toolResult("b"), asstText("latest response")}
	c := New(Config{ContextWindow: 10000, RequestThresholdPercent: 90, KeepRecent: 2}, nil)
	_, tail := c.workingBounds(msgs)
	if tail != 1 {
		t.Fatal("latest user request summarized", tail)
	}
	// Without a later textual user request the nominal last-2 cut falls in the
	// parallel call group, so it must move back to the assistant's call row.
	msgs[1] = asstText("prior response")
	msgs[0] = asstText("old response")
	_, tail = c.workingBounds(msgs)
	if tail != 2 {
		t.Fatal("parallel tool exchange split", tail)
	}
	groups := summaryGroups(msgs)
	if len(groups) != 4 || len(groups[2]) != 3 {
		t.Fatal("summary batching split a parallel exchange", groups)
	}
	before, _ := json.Marshal(msgs)
	out := c.Pre(t.Context(), msgs, 999999)
	after, _ := json.Marshal(out)
	if string(before) != string(after) {
		t.Fatal("request mode performed early micro trimming")
	}
}

func TestRequestThresholdCanSummarizeShortLargeHistory(t *testing.T) {
	calls := 0
	c := New(Config{ContextWindow: 10000, RequestThresholdPercent: 90}, func(context.Context, []llm.Message) (string, error) { calls++; return "digest", nil })
	msgs := []llm.Message{llm.UserText(strings.Repeat("old ", 4000)), llm.UserText("latest human request")}
	out := c.PreRequest(t.Context(), msgs, 8500, 500)
	if calls != 1 || llm.LastBoundaryIndex(out) < 0 || out[len(out)-1].Text() != "latest human request" {
		t.Fatal("short history was not compacted", calls)
	}
}

func TestWindowedSummaryReportsHiddenCompactionProgress(t *testing.T) {
	p := &windowSummaryProvider{stop: "end_turn"}
	var events []llm.CallProgress
	ctx := llm.WithCallProgress(t.Context(), func(p llm.CallProgress) { events = append(events, p) })
	var msgs []llm.Message
	for range 30 {
		msgs = append(msgs, llm.UserText(strings.Repeat("historical fixture ", 250)))
	}
	_, err := WindowedProviderSummarizer(p, 10000, 1000, true)(ctx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) < 2 {
		t.Fatal("fixture must span chunks")
	}
	previous := 0
	for _, ev := range events {
		if ev.Stage != "compacting" || ev.GroupsDone < previous {
			t.Fatal(ev)
		}
		previous = ev.GroupsDone
	}
	last := events[len(events)-1]
	if last.Phase != "chunk_finished" || last.GroupsDone != last.TotalGroups {
		t.Fatal(last)
	}
}
