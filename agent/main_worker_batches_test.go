package agent

import (
	"context"
	"encoding/json"
	"strings"
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
	humanNames := map[string]bool{}
	for _, tool := range human {
		if tool.Name() == "sleep" {
			t.Fatal("main still exposes polling sleep")
		}
		humanNames[tool.Name()] = true
	}
	for _, name := range []string{"Bash", "add_intent", "add_task_scope"} {
		if !humanNames[name] {
			t.Fatal("human-authorized main lost ordinary tool", name)
		}
	}
	auto := mainTurnTools(tools, true)
	for _, tool := range auto {
		switch tool.Name() {
		case "Bash", "sleep", "add_intent", "add_task_scope", "set_goals", "add_hint", "steer_work", "report_finding":
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

func TestMainWorkerCompletionPromptDoesNotInheritDispatchInstructions(t *testing.T) {
	previous := PromptOverride
	t.Cleanup(func() { PromptOverride = previous })
	PromptOverride = func(key string) (string, bool) {
		if key == "mainagent" {
			return "fixture editable prompt: dispatch with add_intent", true
		}
		return "", false
	}
	tools := mainTurnTools(NewToolSet(nil, "human").MainAgentTools(), true)
	auto := mainTurnSystem("fixture goal", "unused", "unused", tools, true)
	for _, want := range []string{"只读汇总", "add_intent、add_task_scope", "并非丢失", "人类新消息", "当前实际可用的只读工具：", "fixture goal"} {
		if !strings.Contains(auto, want) {
			t.Fatalf("auto prompt missing %q", want)
		}
	}
	if strings.Contains(auto, "fixture editable prompt") || strings.Contains(auto, mainWorkerBatchBoundary) {
		t.Fatal("auto report inherited human dispatch instructions")
	}
	for _, tool := range tools {
		if !strings.Contains(auto, tool.Name()) {
			t.Fatal("auto prompt omitted actual tool", tool.Name())
		}
	}
	if !strings.Contains(mainTurnSystem("fixture", "unused", "unused", nil, true), "无；请仅根据已收到的结果总结") {
		t.Fatal("empty read-only registry misrepresented as available")
	}
	human := mainTurnSystem("fixture", "unused", "unused", nil, false)
	if !strings.Contains(human, "fixture editable prompt") || !strings.Contains(human, mainWorkerBatchBoundary) {
		t.Fatal("ordinary human prompt changed")
	}
}

func TestPlannerRetainsDispatchAndScopeTools(t *testing.T) {
	missing := map[string]bool{"add_intent": true, "add_task_scope": true}
	for _, tool := range NewToolSet(nil, "planner").PlannerTools() {
		delete(missing, tool.Name())
	}
	if len(missing) != 0 {
		t.Fatal("planner lost ordinary tools", missing)
	}
}
