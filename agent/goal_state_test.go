package agent

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestGoalStateTool(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("goal state fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	id, err := es.AddGoal(map[string]any{"text": "fixture"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err = es.SetGoalState(id, "met"); err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "human")
	tool := ts.updateGoalState()
	result, err := tool.Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"goal_id":%d,"state":"open","reason":"incorrectly closed"}`, id)), nil)
	if err != nil || result.IsError {
		t.Fatalf("tool: %v %v", result, err)
	}
	n, err := es.GetNode(id)
	if err != nil || n.State != "open" {
		t.Fatalf("not reopened %v", err)
	}
	for _, raw := range []string{fmt.Sprintf(`{"goal_id":%d,"state":"closed","reason":"x"}`, id), fmt.Sprintf(`{"goal_id":%d,"state":"met"}`, id), `{"goal_id":99999999,"state":"open","reason":"x"}`} {
		r, e := tool.Call(t.Context(), json.RawMessage(raw), nil)
		if e != nil || !r.IsError {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, role := range []string{"mainagent", "planner"} {
		found := false
		for _, seed := range BuiltinToolSeeds() {
			if seed.Key == "update_goal_state" {
				for _, a := range seed.Agents {
					if a == role {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatalf("missing binding %s", role)
		}
	}
}
