package db

import "testing"

func TestConversationTokensIndependentOfPage(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := d.CreateConversation("mainagent", "token-pagination-regression", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(c.ID) })
	appendUsage := func(kind string, n int) {
		t.Helper()
		output, cacheRead, cacheWrite := n*2, n*3, n*4
		if _, err := d.AppendConvActivity(c.ID, Activity{Kind: kind, InputTokens: &n, OutputTokens: &output, CacheReadTokens: &cacheRead, CacheWriteTokens: &cacheWrite}); err != nil {
			t.Fatal(err)
		}
	}
	assertTotal := func(n, turns int) {
		t.Helper()
		got, err := d.ConversationTokens(c.ID)
		want := ConversationTokenTotal{InputTokens: n, OutputTokens: n * 2, CacheReadTokens: n * 3, CacheWriteTokens: n * 4, Turns: turns}
		if err != nil || got != want {
			t.Fatalf("got %+v, want %+v; err=%v", got, want, err)
		}
	}
	assertTotal(0, 0)
	appendUsage("usage", 10)
	appendUsage("usage", 30)
	assertTotal(30, 2) // cumulative within a run, not 10+30
	appendUsage("result", 30)
	assertTotal(30, 2) // no duplicate result/live accounting
	for i := 0; i < 220; i++ {
		appendUsage("text", 0)
	}
	appendUsage("usage", 5)
	appendUsage("usage", 12)
	assertTotal(42, 4) // old result is now outside the latest 200-row page
	appendUsage("result", 12)
	assertTotal(42, 4)
	page, more, err := d.ConvActivityPage(c.ID, 0, 200)
	if err != nil || !more || len(page) != 200 {
		t.Fatalf("pagination fixture invalid: len=%d more=%v err=%v", len(page), more, err)
	}
	for _, row := range page {
		if row.Kind == "result" && row.InputTokens != nil && *row.InputTokens == 30 {
			t.Fatal("early result unexpectedly visible in loaded page")
		}
	}
	other, err := d.CreateConversation("mainagent", "unrelated-token-chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(other.ID) })
	unrelated := 999
	if _, err := d.AppendConvActivity(other.ID, Activity{Kind: "result", InputTokens: &unrelated}); err != nil {
		t.Fatal(err)
	}
	assertTotal(42, 4) // conversations are isolated
}
