package agent

import (
	"context"
	"encoding/json"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestMainWorkerBatchTagsOnlyDirectMainDispatch(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("main batch fixture", "report only", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	batch, err := es.BeginMainWorkerBatch(0)
	if err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(es, "human")
	tools.mainWorkerBatchID = batch
	r, err := tools.addIntent().Call(t.Context(), json.RawMessage(`{"intents":[{"summary":"fixture A"},{"summary":""},{"summary":"fixture B"}]}`), nil)
	if err != nil || r.IsError {
		t.Fatalf("batch call: %v %v", r, err)
	}
	var response struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.Unmarshal([]byte(r.Flatten()), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.IDs) != 3 || response.IDs[0] <= 0 || response.IDs[1] != 0 || response.IDs[2] <= 0 {
		t.Fatalf("partial failure ids %v", response.IDs)
	}
	for _, id := range []int64{response.IDs[0], response.IDs[2]} {
		n, _ := es.GetNode(id)
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		if int64(p["main_batch_id"].(float64)) != batch {
			t.Fatal("member lost batch tag")
		}
	}
	planner := NewToolSet(es, "planner")
	id, err := planner.addOneIntent(intentItem{Summary: "planner unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	n, _ := es.GetNode(id)
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	if _, ok := p["main_batch_id"]; ok {
		t.Fatal("planner inherited main batch")
	}
}

func TestMainWorkerCompletionToolsAreReadOnlyAndNeverDefaultToBash(t *testing.T) {
	tools := append(NewToolSet(nil, "human").MainAgentTools(), actool.DefaultTools()...)
	human := mainTurnTools(tools, false)
	hasBash := false
	for _, tool := range human {
		if tool.Name() == "sleep" {
			t.Fatal("main still exposes polling sleep")
		}
		if tool.Name() == "Bash" {
			hasBash = true
		}
	}
	if !hasBash {
		t.Fatal("human-authorized main lost ordinary tools")
	}
	auto := mainTurnTools(tools, true)
	for _, tool := range auto {
		switch tool.Name() {
		case "Bash", "sleep", "add_intent", "set_goals", "add_hint", "steer_work", "report_finding":
			t.Fatal("auto report can mutate", tool.Name())
		}
	}
	empty := mainTurnTools(actool.DefaultTools(), true)
	if empty == nil || len(empty) != 0 {
		t.Fatal("SDK nil fallback would re-enable dangerous tools")
	}
	previous := ToolAugment
	t.Cleanup(func() { ToolAugment = previous })
	called := false
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		called = true
		return nil, DeferredInfo{}, func() {}
	}
	_, _, close := AugmentTools(WithMainWorkerCompletion(t.Context()), "mainagent", []actool.CoreTool{})
	close()
	if called {
		t.Fatal("auto summary connected arbitrary MCP/host tools")
	}
}
