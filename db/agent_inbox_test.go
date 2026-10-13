package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAgentInboxOrderingEditingIsolationAndRecovery(t *testing.T) {
	d, _, taskID := mainBatchDB(t)
	scope := fmt.Sprintf("main:%d:0", taskID)
	payload := func(msg string) []byte {
		p, _ := json.Marshal(map[string]string{"message": msg, "agent_message": msg})
		return p
	}
	enqueue := func(key, mode, msg string) AgentInboxItem {
		t.Helper()
		i, err := d.EnqueueAgentMessage(scope, taskID, 0, 0, key, mode, payload(msg))
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	a := enqueue("a", "queue", "first")
	b := enqueue("b", "queue", "second")
	steer := enqueue("s", "steer", "live")
	c := enqueue("c", "queue", "third")
	same := enqueue("a", "queue", "first")
	if same.ID != a.ID {
		t.Fatal("duplicate enqueue")
	}
	if _, err := d.EnqueueAgentMessage(scope, taskID, 0, 0, "a", "queue", payload("different")); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("no idempotency conflict", err)
	}
	if err := d.EditAgentMessage(scope, b.ID, 1, payload("edited")); err != nil {
		t.Fatal(err)
	}
	if err := d.EditAgentMessage(scope, b.ID, 1, payload("stale")); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("stale edit accepted", err)
	}
	// Retrying original POST after editing must not revert the edit or duplicate it.
	if dup := enqueue("b", "queue", "second"); dup.ID != b.ID || !containsInboxPayload(dup, "edited") {
		t.Fatal("edit lost on retry", dup)
	}
	previous := []int64{a.ID, b.ID, c.ID}
	order := []int64{c.ID, b.ID, a.ID}
	if err := d.ReorderAgentMessages(scope, order, previous); err != nil {
		t.Fatal(err)
	}
	if err := d.ReorderAgentMessages(scope, previous, previous); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("stale reorder accepted", err)
	}
	if err := d.ReorderAgentMessages(scope, []int64{c.ID, c.ID, a.ID}, order); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("duplicate reorder accepted", err)
	}
	if items, _ := d.AgentInbox(scope); !reflect.DeepEqual([]int64{items[0].ID, items[1].ID, items[2].ID, items[3].ID}, []int64{c.ID, b.ID, steer.ID, a.ID}) {
		t.Fatal("wrong order", items)
	}
	drained, err := d.DrainAgentSteers(context.Background(), scope)
	if err != nil || len(drained) != 1 || drained[0].ID != steer.ID {
		t.Fatal("wrong drain", drained, err)
	}
	if ok, err := d.ClaimAgentMessage(scope, a.ID); err != nil || ok {
		t.Fatal("claim skipped head", err)
	}
	if ok, err := d.CancelAgentMessage(scope+"other", a.ID); err != nil || ok {
		t.Fatal("cross scope cancellation", err)
	}
	if err := d.EditAgentMessage(scope+"other", b.ID, 2, payload("bad")); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("cross scope edit", err)
	}
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ok, err := d.ClaimAgentMessage(scope, c.ID)
			if err != nil {
				t.Error(err)
			}
			if ok {
				claimed.Add(1)
			}
		})
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatal("multiple claims", claimed.Load())
	}
	if ok, _ := d.CancelAgentMessage(scope, c.ID); ok {
		t.Fatal("cancelled running")
	}
	if err := d.EditAgentMessage(scope, c.ID, 1, payload("bad")); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("edited running", err)
	}
	if err := d.RecoverAgentInbox(); err != nil {
		t.Fatal(err)
	}
	var state string
	d.QueryRow(`SELECT state FROM agent_inbox WHERE id=$1`, c.ID).Scan(&state)
	if state != "interrupted" {
		t.Fatal(state)
	}
	items, _ := d.AgentInbox(scope)
	if len(items) != 2 || items[0].ID != b.ID || items[1].ID != a.ID {
		t.Fatal("pending recovery lost", items)
	}
	if ok, err := d.CancelAgentMessage(scope, a.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := d.FinishAgentMessage(c.ID, "pending"); err == nil {
		t.Fatal("invalid finish allowed")
	}
}
func containsInboxPayload(i AgentInboxItem, msg string) bool {
	var p map[string]string
	_ = json.Unmarshal(i.Payload, &p)
	return p["message"] == msg
}
func TestAgentInboxLimitAndSteerStop(t *testing.T) {
	d, _, taskID := mainBatchDB(t)
	scope := fmt.Sprintf("main:%d:0", taskID)
	for i := range 32 {
		mode := "queue"
		if i%2 == 0 {
			mode = "steer"
		}
		if _, err := d.EnqueueAgentMessage(scope, taskID, 0, 0, fmt.Sprint(i), mode, []byte(`{"message":"test"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.EnqueueAgentMessage(scope, taskID, 0, 0, "full", "queue", []byte(`{}`)); !errors.Is(err, ErrInboxFull) {
		t.Fatal("no limit", err)
	}
	if err := d.CancelTaskSteers(taskID); err != nil {
		t.Fatal(err)
	}
	items, _ := d.AgentInbox(scope)
	if len(items) != 16 {
		t.Fatal(items)
	}
	for _, i := range items {
		if i.Mode != "queue" {
			t.Fatal("steer left", i)
		}
	}
	other := fmt.Sprintf("main:%d:1", taskID)
	if _, err := d.EnqueueAgentMessage(other, taskID, 0, 1, "a", "steer", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.DrainAgentSteers(ctx, other); err == nil {
		t.Fatal("cancelled drain accepted")
	}
	if items, _ := d.AgentInbox(other); len(items) != 1 {
		t.Fatal("cancelled drain lost message")
	}
}
