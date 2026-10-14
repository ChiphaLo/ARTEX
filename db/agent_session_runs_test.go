package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAgentSessionResumeCASRecoveryAndBackground(t *testing.T) {
	d, _, task := mainBatchDB(t)
	scope := fmt.Sprintf("main:%d:0", task)
	review := []byte(`{"source":"user_message","text":"original request"}`)
	first, err := d.StartAgentRun(scope, task, 0, true, review, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.StartAgentRun(scope, task, 0, false, review, 0); !errors.Is(err, ErrSessionRunConflict) {
		t.Fatal("allowed parallel run", err)
	}
	payload, _ := json.Marshal(map[string]string{"message": "human narrowing steer"})
	if _, err := d.EnqueueAgentMessage(scope, task, 0, 0, "steer", "steer", payload); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DrainAgentSteers(context.Background(), scope, first+100000); err == nil {
		t.Fatal("drained into wrong run")
	}
	if items, _ := d.AgentInbox(scope); len(items) != 1 {
		t.Fatal("atomic background failure lost steer")
	}
	if _, err := d.DrainAgentSteers(context.Background(), scope, first); err != nil {
		t.Fatal(err)
	}
	latest, _ := d.LatestAgentRun(scope)
	var bg map[string]string
	json.Unmarshal(latest.Review, &bg)
	if bg["text"] != "human narrowing steer" {
		t.Fatal(bg)
	}
	if err := d.FinishAgentRun(first, "interrupted"); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			_, err := d.StartAgentRun(scope, task, 0, false, []byte(`{"source":"worker_completion"}`), first)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrSessionRunConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("duplicate resume", winners.Load())
	}
	latest, _ = d.LatestAgentRun(scope)
	if !latest.ReadOnly {
		t.Fatal("resume escalated capabilities")
	}
	json.Unmarshal(latest.Review, &bg)
	if bg["text"] != "human narrowing steer" {
		t.Fatal("resume forged background", bg)
	}
	if err := d.RecoverAgentRuns(); err != nil {
		t.Fatal(err)
	}
	latest, _ = d.LatestAgentRun(scope)
	if latest.State != "interrupted" {
		t.Fatal(latest)
	}
	completed, e := d.StartAgentRun(scope, task, 0, false, review, latest.ID)
	if e != nil {
		t.Fatal(e)
	}
	d.FinishAgentRun(completed, "completed")
	if _, err := d.StartAgentRun(scope, task, 0, false, review, completed); !errors.Is(err, ErrSessionRunConflict) {
		t.Fatal("continued completed run", err)
	}
	if _, err := d.StartAgentRun(scope+"other", task, 0, false, review, first); !errors.Is(err, ErrSessionRunConflict) {
		t.Fatal("cross-scope continue", err)
	}
}
