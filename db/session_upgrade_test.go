package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestQueuePromotionCASAndScope(t *testing.T) {
	d, _, task := mainBatchDB(t)
	scope := fmt.Sprintf("main:%d:0", task)
	i, err := d.EnqueueAgentMessage(scope, task, 0, 0, "promotion", "queue", []byte(`{"message":"fixture"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.EditAgentMessage("wrong", i.ID, 1, i.Payload, "steer"); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("cross-scope accepted", err)
	}
	if err = d.EditAgentMessage(scope, i.ID, 1, i.Payload, "steer"); err != nil {
		t.Fatal(err)
	}
	if err = d.EditAgentMessage(scope, i.ID, 1, i.Payload, "steer"); !errors.Is(err, ErrInboxConflict) {
		t.Fatal("stale edit accepted", err)
	}
	got, err := d.DrainAgentSteers(context.Background(), scope)
	if err != nil || len(got) != 1 || got[0].Mode != "steer" {
		t.Fatal(got, err)
	}
	if got, err = d.AgentInbox(scope); err != nil || len(got) != 0 {
		t.Fatal("delivered still pending", got, err)
	}
}

func TestProviderMultipleModelsPreservesKey(t *testing.T) {
	d, _, _ := mainBatchDB(t)
	p := &LLMProfile{Name: "multi-model-fixture", Format: "openai-responses", Model: "one", Models: []string{"one", "two", "two"}, APIKey: "fixture-key", Streaming: true}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteProfile(id)
	p, err = d.ProfileByID(id)
	if err != nil || len(p.Models) != 2 {
		t.Fatal(p, err)
	}
	p.Model = "two"
	p.APIKey = ""
	if _, err = d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	p, err = d.ProfileByID(id)
	if err != nil || p.Model != "two" || p.APIKey != "fixture-key" || len(p.Models) != 2 {
		t.Fatal("switch lost provider configuration", err)
	}
	p.Model = " "
	p.Models = []string{"three", "four"}
	if _, err = d.SaveProfile(p); err != nil || p.Model != "three" {
		t.Fatal("model list did not select a valid default", p.Model, err)
	}
}

func TestPlannerBatchBindsOriginalMainSegment(t *testing.T) {
	d, es, _ := mainBatchDB(t)
	id, err := es.BeginMainWorkerBatch(4, "planner")
	if err != nil {
		t.Fatal(err)
	}
	worker := batchIntent(t, es, id, "done")
	if worker == 0 {
		t.Fatal("missing worker")
	}
	if err = es.SealMainWorkerBatch(id, false); err != nil {
		t.Fatal(err)
	}
	batches, err := d.ReadyMainWorkerBatches(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range batches {
		if b.ID == id {
			if b.MainSeg != 4 || b.Dispatcher != "planner" {
				t.Fatal(b)
			}
			return
		}
	}
	t.Fatal("planner round not ready")
}
