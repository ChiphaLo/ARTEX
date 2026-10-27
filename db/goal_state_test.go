package db

import "testing"

func TestGoalStateTransitionsAndIsolation(t *testing.T) {
	d, es, _ := mainBatchDB(t)
	other, err := d.CreateTask("other fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(other.ID) })
	id, err := es.AddGoal(map[string]any{"text": "goal"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := d.Exploration(other.ExplorationID).AddGoal(map[string]any{"text": "other"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := es.AddIntent(map[string]any{"summary": "fixture"}, 1, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"met", "open", "abandoned", "open"} {
		if err := es.SetGoalState(id, state); err != nil {
			t.Fatal(err)
		}
		node, err := es.GetNode(id)
		if err != nil || node.State != state {
			t.Fatalf("state %s: %v", state, err)
		}
	}
	for _, tc := range []struct {
		id    int64
		state string
	}{{id, "closed"}, {foreign, "open"}, {intent, "open"}, {999999999, "open"}} {
		if err := es.SetGoalState(tc.id, tc.state); err == nil {
			t.Fatalf("expected reject %+v", tc)
		}
	}
}
