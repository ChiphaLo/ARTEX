package compaction

import (
	"context"
	"errors"
	"github.com/Autumn-27/norma/llm"
	"strings"
	"testing"
)

func TestRequestCompactRetriesOversizedSummary(t *testing.T) {
	calls := 0
	c := New(Config{ContextWindow: 10000, RequestThresholdPercent: 90, KeepRecent: 1}, func(_ context.Context, msgs []llm.Message) (string, error) {
		calls++
		if calls == 1 {
			return strings.Repeat("long ", 4000), nil
		}
		if !strings.Contains(msgs[0].Text(), "Condense") {
			t.Fatal("missing retry instruction")
		}
		return "concise digest", nil
	})
	msgs := []llm.Message{llm.UserText(strings.Repeat("old content ", 1000)), llm.UserText("latest")}
	out := c.PreRequest(t.Context(), msgs, 8500, 500)
	if calls != 2 || llm.LastBoundaryIndex(out) < 0 || out[len(out)-1].Text() != "latest" {
		t.Fatalf("failed retry calls=%d", calls)
	}
}
func TestRequestCompactRetryFailureRetainsHistory(t *testing.T) {
	calls := 0
	var seen error
	c := New(Config{ContextWindow: 10000, RequestThresholdPercent: 90, KeepRecent: 1, OnError: func(e error) { seen = e }}, func(_ context.Context, _ []llm.Message) (string, error) {
		calls++
		if calls == 1 {
			return strings.Repeat("long ", 4000), nil
		}
		return "", errors.New("upstream unavailable")
	})
	msgs := []llm.Message{llm.UserText(strings.Repeat("old content ", 1000)), llm.UserText("latest")}
	out := c.PreRequest(t.Context(), msgs, 8500, 500)
	if calls != 2 || llm.LastBoundaryIndex(out) >= 0 || seen == nil || !strings.Contains(seen.Error(), "upstream unavailable") {
		t.Fatal("unsafe or silent retry", calls, seen)
	}
}
