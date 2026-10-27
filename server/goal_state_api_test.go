package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGoalStateAPIReopenAndValidation(t *testing.T) {
	s, task, _ := inboxFixture(t)
	id, err := task.Store.AddGoal(map[string]any{"text": "fixture"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err = task.Store.SetGoalState(id, "met"); err != nil {
		t.Fatal(err)
	}
	call := func(body string, gid int64) int {
		r := httptest.NewRequest("PATCH", "/", strings.NewReader(body))
		r.SetPathValue("id", task.ID)
		r.SetPathValue("gid", fmt.Sprint(gid))
		w := httptest.NewRecorder()
		s.changeGoalState(w, r)
		return w.Code
	}
	if err := s.m.SetTaskPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := call(`{"state":"open"}`, id); got != 200 {
		t.Fatal(got)
	}
	if !task.lifecycleSnapshot().Paused {
		t.Fatal("reopening resumed a paused task")
	}
	n, err := task.Store.GetNode(id)
	if err != nil || n.State != "open" {
		t.Fatal(n, err)
	}
	if got := call(`{"state":"closed"}`, id); got != 400 {
		t.Fatal(got)
	}
	if got := call(`{"state":"open"}`, 999999999); got != 404 {
		t.Fatal(got)
	}
}
