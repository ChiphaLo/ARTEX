package db

import (
	"strings"
	"testing"
)

func TestUIDiagnosticsExcludedFromAgentReadsButRetainedForPeople(t *testing.T) {
	d, es, taskID := mainBatchDB(t)
	batch, err := es.BeginMainWorkerBatch(0)
	if err != nil {
		t.Fatal(err)
	}
	node := batchIntent(t, es, batch, "blocked")
	usage := 370863
	rows := []Activity{
		{Kind: "text", Summary: "needle observation", Detail: "real observation"},
		{Kind: "tool_result", IsError: true, Summary: "needle tool error", Detail: "real command exit 1"},
		{Kind: "result", Summary: "needle conclusion", Detail: "real conclusion"},
		{Kind: "result", IsError: true, Summary: "needle diagnostic", Detail: "402 insufficient balance", Metadata: []byte(`{"ui_only":true}`), InputTokens: &usage},
		{Kind: "result", IsError: true, Summary: "（无文字总结，终态 model_error：needle legacy）", Detail: "legacy 402 raw body"},
		{Kind: "result", Summary: "（运行被中断：needle legacy）", Detail: "legacy context canceled"},
		{Kind: "result", Summary: "（达到运行预算上限(max_turns)，needle legacy）"},
		{Kind: "result", IsError: true, Summary: "执行出错: needle upstream error"},
		{Kind: "retry", Summary: "needle 502 retry body"},
		{Kind: "model_status", Summary: "needle progress"},
		{Kind: "text", IsError: true, Summary: "needle planner runtime error"},
		{Kind: "thinking", Summary: "needle reasoning"},
		{Kind: "usage", Summary: "needle accounting"},
		{Kind: "context_usage", Summary: "needle context"},
	}
	var ids []int64
	for _, row := range rows {
		row.NodeID, row.Worker = &node, "fixture-worker"
		id, err := es.AppendActivity(row)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	queries := map[string]func() ([]Activity, error){
		"trace":               func() ([]Activity, error) { return es.ActivityTrace(node, 3) },
		"terminal trace":      func() ([]Activity, error) { return es.ActivityTraceForTerminalIntent(node, 3) },
		"scoped search":       func() ([]Activity, error) { return es.ActivityTraceSearch(&node, "needle", 3) },
		"global search":       func() ([]Activity, error) { return es.ActivityTraceSearch(nil, "needle", 3) },
		"terminal search":     func() ([]Activity, error) { return es.ActivityTraceSearchForTerminalIntent(node, "needle", 3) },
		"all terminal search": func() ([]Activity, error) { return es.ActivityTraceSearchTerminalIntents("needle", 3) },
		"excluding search":    func() ([]Activity, error) { return es.ActivityTraceSearchExcluding(node+1, "needle", 3) },
		"details":             func() ([]Activity, error) { return es.ActivityByIDs(ids) },
		"terminal details":    func() ([]Activity, error) { return es.ActivityByIDsForTerminalIntents(ids) },
	}
	for name, query := range queries {
		items, err := query()
		if err != nil || len(items) != 3 {
			t.Fatalf("%s: rows=%+v err=%v", name, items, err)
		}
		for i := range items {
			if items[i].ID != ids[i] {
				t.Fatalf("%s: unexpected row %+v", name, items[i])
			}
		}
	}
	output, err := es.LatestWorkerOutput(node, false)
	if err != nil || output == nil || output.Detail != "real conclusion" {
		t.Fatalf("worker conclusion: %+v, %v", output, err)
	}
	summary, err := es.LatestWorkerOutputSummary(node, true)
	if err != nil || summary == nil || summary.Summary != "needle conclusion" || summary.Detail != "" {
		t.Fatalf("overview: %+v, %v", summary, err)
	}
	results, total, err := d.MainWorkerBatchResults(MainWorkerBatch{ID: batch, ExplorationID: es.ID(), TaskID: taskID})
	if err != nil || total != 1 || len(results) != 1 || results[0].Output != "real conclusion" || results[0].State != "blocked" {
		t.Fatalf("batch: %+v, total=%d err=%v", results, total, err)
	}
	feed, _, err := es.ActivityList(&node, 0, 100)
	if err != nil || len(feed) != len(rows) {
		t.Fatalf("human feed lost diagnostics: %d rows, %v", len(feed), err)
	}
	page, _, err := es.ActivityPage(ActivitySessionFilter{NodeID: &node}, 0, 100)
	if err != nil || len(page) != len(rows) {
		t.Fatalf("human page lost diagnostics: %d rows, %v", len(page), err)
	}
	if detail, err := es.ActivityDetail(ids[3]); err != nil || detail != "402 insufficient balance" {
		t.Fatalf("human detail: %q %v", detail, err)
	}
	tokens, err := es.TokenTotal()
	if err != nil || tokens.InputTokens != usage {
		t.Fatalf("billing changed: %+v %v", tokens, err)
	}
}

func TestUIDiagnosticsOnlyWorkerHasNoFabricatedConclusion(t *testing.T) {
	d, es, taskID := mainBatchDB(t)
	batch, _ := es.BeginMainWorkerBatch(0)
	node := batchIntent(t, es, batch, "blocked")
	_, err := es.AppendActivity(Activity{NodeID: &node, Worker: "fixture", Kind: "result", IsError: true,
		Summary: "（无文字总结，终态 model_error：余额不足）", Detail: "provider credential error"})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := es.LatestWorkerOutput(node, false); err != nil || output != nil {
		t.Fatalf("fabricated output: %+v %v", output, err)
	}
	results, _, err := d.MainWorkerBatchResults(MainWorkerBatch{ID: batch, ExplorationID: es.ID(), TaskID: taskID})
	if err != nil || len(results) != 1 || results[0].Output != "" || !results[0].IsError || results[0].State != "blocked" {
		t.Fatalf("diagnostic-only batch must retain state, not raw error: %+v %v", results, err)
	}
}

func TestUIDiagnosticsInheritedOutputAndHumanDetail(t *testing.T) {
	d, source, sourceTask := mainBatchDB(t)
	current, err := d.CreateTaskWithOptions("diagnostic reader", "synthetic", TaskCreateOptions{SourceTaskIDs: []int64{sourceTask}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(current.ID) })
	reader := d.Exploration(current.ExplorationID)
	node := batchIntent(t, source, 0, "done")
	_, err = source.AppendActivity(Activity{NodeID: &node, Kind: "text", Summary: "retained observation", Detail: "retained detail"})
	if err != nil {
		t.Fatal(err)
	}
	// More than a UI page of newer diagnostics cannot hide the real conclusion.
	_, err = d.Exec(`INSERT INTO activity(exploration_id,node_id,kind,summary,detail,metadata)
 SELECT $1,$2,'result','UI diagnostic','raw provider error','{"ui_only":true}'::jsonb FROM generate_series(1,1001)`, source.ID(), node)
	if err != nil {
		t.Fatal(err)
	}
	output, err := reader.LatestWorkerOutputWithSources(node)
	if err != nil || output == nil || !output.Inherited || output.SourceTaskID != sourceTask || output.Detail != "retained detail" {
		t.Fatalf("source output: %+v %v", output, err)
	}
	var diagnosticID int64
	if err := d.QueryRow(`SELECT max(id) FROM activity WHERE exploration_id=$1`, source.ID()).Scan(&diagnosticID); err != nil {
		t.Fatal(err)
	}
	if detail, err := reader.ActivityDetailWithSources(diagnosticID); err != nil || detail != "raw provider error" {
		t.Fatalf("inherited human detail lost: %q %v", detail, err)
	}
	if details, err := reader.ActivityByIDsWithSources([]int64{diagnosticID}); err != nil || len(details) != 0 {
		t.Fatalf("inherited agent detail leaked: %+v %v", details, err)
	}
	if err := source.SetIntentState(node, "running"); err != nil {
		t.Fatal(err)
	}
	if output, err := reader.LatestWorkerOutputWithSources(node); err != nil || output != nil {
		t.Fatalf("reopened source leaked: %+v %v", output, err)
	}
	if detail, err := reader.ActivityDetailWithSources(diagnosticID); err != nil || strings.TrimSpace(detail) != "" {
		t.Fatalf("reopened UI source leaked: %q %v", detail, err)
	}
}
