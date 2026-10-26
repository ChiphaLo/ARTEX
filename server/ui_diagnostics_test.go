package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestUIDiagnosticsHumanAPIVisibleButAutomaticMainPromptFiltered(t *testing.T) {
	p := &mainBatchProvider{}
	s, task := mainBatchFixture(t, p)
	batch, err := task.Store.BeginMainWorkerBatch(0)
	if err != nil {
		t.Fatal(err)
	}
	node, err := task.Store.AddIntent(map[string]any{"summary": "synthetic work", "main_batch_id": batch}, 1, nil, "mainagent")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetIntentState(node, "blocked"); err != nil {
		t.Fatal(err)
	}
	id, err := task.Store.AppendActivity(db.Activity{NodeID: &node, Worker: "fixture", Kind: "result", IsError: true,
		Summary: "（无文字总结，终态 model_error：diagnostic fixture）", Detail: "status 402: fixture account balance is insufficient",
		Metadata: []byte(`{"ui_only":true,"terminal_reason":"model_error"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// Test the human lazy-detail handler, not just storage. No task content or
	// upstream API is involved: this is a newly created synthetic fixture.
	r := httptest.NewRequest("GET", fmt.Sprintf("/api/activity/%d?task=%s", id, task.ID), nil)
	r.SetPathValue("seq", fmt.Sprint(id))
	w := httptest.NewRecorder()
	s.activityDetail(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fixture account balance is insufficient") {
		t.Fatalf("UI lost diagnostic: %d %s", w.Code, w.Body.String())
	}
	if err := task.Store.SealMainWorkerBatch(batch, false); err != nil {
		t.Fatal(err)
	}
	s.pumpMainWorkerBatches()
	mainBatchEventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.autoCalls == 1
	})
	p.mu.Lock()
	message := p.autoMessage
	p.mu.Unlock()
	if strings.Contains(message, "balance is insufficient") || strings.Contains(message, "model_error") || strings.Contains(message, "status 402") || !strings.Contains(message, `"state":"blocked"`) {
		t.Fatalf("main prompt should contain state, not UI diagnostic: %s", message)
	}
}
