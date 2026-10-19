package db

import "testing"

// Request sizing is UI metadata, not worker output to feed back into agents.
func TestContextUsageExcludedFromTracesButRetainedInSessionFeed(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("test", "context metadata filtering")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)
	nodeID, err := es.AddIntent(map[string]any{"summary": "test"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.SetIntentState(nodeID, "done"); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, kind := range []string{"text", "context_usage"} {
		id, err := es.AppendActivity(Activity{NodeID: &nodeID, Worker: "test-worker", Kind: kind, Summary: "needle", Detail: "needle"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	check := func(name string, items []Activity, err error) {
		t.Helper()
		if err != nil || len(items) != 1 || items[0].Kind != "text" {
			t.Fatalf("%s: got %+v, err %v", name, items, err)
		}
	}
	queries := map[string]func() ([]Activity, error){
		"trace":               func() ([]Activity, error) { return es.ActivityTrace(nodeID, 100) },
		"terminal trace":      func() ([]Activity, error) { return es.ActivityTraceForTerminalIntent(nodeID, 100) },
		"scoped search":       func() ([]Activity, error) { return es.ActivityTraceSearch(&nodeID, "needle", 100) },
		"global search":       func() ([]Activity, error) { return es.ActivityTraceSearch(nil, "needle", 100) },
		"terminal search":     func() ([]Activity, error) { return es.ActivityTraceSearchForTerminalIntent(nodeID, "needle", 100) },
		"all terminal search": func() ([]Activity, error) { return es.ActivityTraceSearchTerminalIntents("needle", 100) },
		"excluding search":    func() ([]Activity, error) { return es.ActivityTraceSearchExcluding(nodeID+1, "needle", 100) },
		"details":             func() ([]Activity, error) { return es.ActivityByIDs(ids) },
		"terminal details":    func() ([]Activity, error) { return es.ActivityByIDsForTerminalIntents(ids) },
	}
	for name, query := range queries {
		items, err := query()
		check(name, items, err)
	}
	items, _, err := es.ActivityListForTerminalIntent(nodeID, 0, 100)
	check("inherited list", items, err)
	items, _, err = es.ActivityPageForTerminalIntent(nodeID, 0, 100)
	check("inherited page", items, err)
	items, _, err = es.ActivityList(&nodeID, 0, 100)
	if err != nil || len(items) != 2 || items[1].Kind != "context_usage" {
		t.Fatalf("SSE feed lost UI metadata: %+v, %v", items, err)
	}
	items, _, err = es.ActivityPage(ActivitySessionFilter{NodeID: &nodeID}, 0, 100)
	if err != nil || len(items) != 2 || items[1].Kind != "context_usage" {
		t.Fatalf("session page lost UI metadata: %+v, %v", items, err)
	}
}
