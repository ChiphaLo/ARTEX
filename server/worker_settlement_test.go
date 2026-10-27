package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// The trigger simulates PostgreSQL 57P03 only for this synthetic intent. It
// neither stops PostgreSQL nor reads/changes any existing user task.
func failFixtureStateWrites(t *testing.T, s *Server, task *Task, id int64) func() {
	t.Helper()
	fn := fmt.Sprintf("fixture_settlement_fail_%d", id)
	trigger := fmt.Sprintf("fixture_settlement_trigger_%d", id)
	q := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.id=%d AND OLD.state='running' AND NEW.state<>'running' THEN
 RAISE EXCEPTION USING ERRCODE='57P03', MESSAGE='synthetic database recovery';
END IF; RETURN NEW; END $$;
CREATE TRIGGER %s BEFORE UPDATE OF state ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION %s()`, fn, id, trigger, fn)
	if _, err := s.m.pg.Exec(q); err != nil {
		t.Fatal(err)
	}
	removed := false
	restore := func() {
		if removed {
			return
		}
		if _, err := s.m.pg.Exec(fmt.Sprintf(`DROP TRIGGER %s ON exploration_nodes; DROP FUNCTION %s()`, trigger, fn)); err != nil {
			t.Fatal(err)
		}
		removed = true
	}
	t.Cleanup(restore)
	return restore
}

func newSettlementFixture(t *testing.T) (*Server, *Task, int64) {
	t.Helper()
	s, task := mainBatchFixture(t, &mainBatchProvider{})
	id, err := task.Store.AddIntent(map[string]any{"summary": "synthetic settlement"}, 1, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetIntentState(id, "running"); err != nil {
		t.Fatal(err)
	}
	return s, task, id
}

func TestWorkerSettlementRetriesAfterDatabaseRecovery(t *testing.T) {
	s, task, id := newSettlementFixture(t)
	restore := failFixtureStateWrites(t, s, task, id)
	err := s.engine.settleWorkerState(s.ctx, task, id, "blocked", db.IntentBlockedLLMQuota, true)
	if err == nil || !strings.Contains(err.Error(), "synthetic database recovery") {
		t.Fatalf("failed write was not detected: %v", err)
	}
	if n, err := task.Store.GetNode(id); err != nil || n.State != "running" {
		t.Fatalf("synthetic failure not maintained: %+v %v", n, err)
	}
	restore()
	mainBatchEventually(t, func() bool {
		n, _ := task.Store.GetNode(id)
		return n != nil && n.State == "blocked" && n.BlockedReason == db.IntentBlockedLLMQuota
	})
	s.engine.settlementMu.Lock()
	_, ok := s.engine.settlementPending[id]
	s.engine.settlementMu.Unlock()
	if ok {
		t.Fatal("successfully reconciled settlement leaked")
	}
}

func TestWorkerSettlementPauseReconcilesKnownFinishedWork(t *testing.T) {
	s, task, id := newSettlementFixture(t)
	restore := failFixtureStateWrites(t, s, task, id)
	if err := s.engine.settleWorkerState(s.ctx, task, id, "blocked", "", true); err == nil {
		t.Fatal("expected synthetic outage")
	}
	restore()
	result, err := s.applyIntentControl(t.Context(), task, id, "pause", "", "")
	if err != nil || result.State != "paused" {
		t.Fatalf("pause pending settlement: %+v %v", result, err)
	}
	// The delayed retry must not overwrite the user-selected paused state.
	time.Sleep(workerSettlementRetryInterval + 100*time.Millisecond)
	n, err := task.Store.GetNode(id)
	if err != nil || n.State != "paused" {
		t.Fatalf("delayed retry overwrote pause: %+v %v", n, err)
	}
}

func TestWorkerSettlementNeverGuessesUnknownMissingWork(t *testing.T) {
	s, task, id := newSettlementFixture(t)
	_, err := s.applyIntentControl(t.Context(), task, id, "pause", "", "")
	if !errors.Is(err, errWorkControlConflict) {
		t.Fatalf("unknown handle should remain a conflict: %v", err)
	}
	n, _ := task.Store.GetNode(id)
	if n.State != "running" {
		t.Fatalf("unknown running worker changed: %+v", n)
	}
}

func TestWorkerSettlementCancelKnownFinishedWork(t *testing.T) {
	s, task, id := newSettlementFixture(t)
	restore := failFixtureStateWrites(t, s, task, id)
	if err := s.engine.settleWorkerState(s.ctx, task, id, "blocked", "", true); err == nil {
		t.Fatal("expected synthetic outage")
	}
	restore()
	result, err := s.applyIntentControl(t.Context(), task, id, "cancel", "synthetic reason", "soft")
	if err != nil || result.State != db.StateIntentDeleted {
		t.Fatalf("cancel pending settlement: %+v %v", result, err)
	}
	n, err := task.Store.GetNode(id)
	if err != nil || n.State != db.StateIntentDeleted {
		t.Fatalf("cancel state: %+v %v", n, err)
	}
}

func TestWorkerSettlementRetryDoesNotOverwriteNewerState(t *testing.T) {
	s, task, id := newSettlementFixture(t)
	restore := failFixtureStateWrites(t, s, task, id)
	if err := s.engine.settleWorkerState(s.ctx, task, id, "blocked", "", true); err == nil {
		t.Fatal("expected synthetic outage")
	}
	restore()
	if err := task.Store.SetIntentState(id, "paused"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(workerSettlementRetryInterval + 100*time.Millisecond)
	n, err := task.Store.GetNode(id)
	if err != nil || n.State != "paused" {
		t.Fatalf("retry overwrote newer state: %+v %v", n, err)
	}
}

func TestWorkerSettlementDatabaseWaitHonorsCancellation(t *testing.T) {
	s, task, id := newSettlementFixture(t)
	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE exploration_nodes SET state=state WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = persistWorkerSettlement(ctx, task, id, "paused", "")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("database lock ignored cancellation: elapsed=%s err=%v", time.Since(started), err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	n, err := task.Store.GetNode(id)
	if err != nil || n.State != "running" {
		t.Fatalf("cancelled update changed state: %+v %v", n, err)
	}
}

func TestWorkerSettlementActualWorkerDoesNotReplayAfterRecovery(t *testing.T) {
	p := &mainBatchProvider{}
	s, task := mainBatchFixture(t, p)
	id, err := task.Store.AddIntent(map[string]any{"summary": "synthetic worker"}, 1, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetIntentState(id, "running"); err != nil {
		t.Fatal(err)
	}
	intent, err := task.Store.GetNode(id)
	if err != nil || intent == nil {
		t.Fatalf("synthetic intent: %+v %v", intent, err)
	}
	restore := failFixtureStateWrites(t, s, task, id)
	worker := agent.NewWorker(p, "synthetic", s.m.dir, nil, 64000, 5)
	if !s.engine.beginTaskOperation(task.ID) {
		t.Fatal("fixture task unavailable")
	}
	s.engine.runIntent(s.ctx, task, "fixture", worker, intent, "", "")
	s.engine.decInflight(task.ID)
	n, err := task.Store.GetNode(id)
	if err != nil || n.State != "running" {
		t.Fatalf("failed settlement was not retained: %+v %v", n, err)
	}
	restore()
	mainBatchEventually(t, func() bool {
		n, _ := task.Store.GetNode(id)
		return n != nil && n.State == "done"
	})
	p.mu.Lock()
	calls := p.workerCalls
	p.mu.Unlock()
	if calls != 1 {
		t.Fatalf("model was replayed during state recovery: %d calls", calls)
	}
}
