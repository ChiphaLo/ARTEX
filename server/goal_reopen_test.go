package server

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGoalReopenRepairsCompletedTaskAsPaused(t *testing.T) {
	s, task, _ := inboxFixture(t)
	id, err := task.Store.AddGoal(map[string]any{"text": "fixture"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PATCH", "/", strings.NewReader(`{"state":"open"}`))
	req.SetPathValue("id", task.ID)
	req.SetPathValue("gid", fmt.Sprint(id))
	w := httptest.NewRecorder()
	s.changeGoalState(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	life := task.lifecycleSnapshot()
	if life.Status != "running" || !life.Paused || life.CompletedAt != 0 || life.Queued || !s.engine.IsPaused(task.ID) {
		t.Fatalf("bad lifecycle: %+v", life)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	pg, err := s.m.pg.GetTask(taskID)
	if err != nil || pg.Status != "running" || !pg.Paused || pg.CompletedAt != nil {
		t.Fatalf("bad persisted lifecycle: %+v %v", pg, err)
	}
}
