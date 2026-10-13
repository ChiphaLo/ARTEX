package db

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func mainBatchDB(t *testing.T) (*DB, *ExplorationStore, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("batch fixture", "only report fixture results", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.DeleteTask(task.ID); err != nil {
			t.Error(err)
		}
	})
	return d, d.Exploration(task.ExplorationID), task.ID
}

func batchIntent(t *testing.T, es *ExplorationStore, batch int64, state string) int64 {
	t.Helper()
	payload := map[string]any{"summary": "isolated fixture"}
	if batch > 0 {
		payload["main_batch_id"] = batch
	}
	id, err := es.AddIntent(payload, 5, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.SetIntentState(id, state); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMainWorkerBatchAllTerminalSealedAndExactlyOneClaim(t *testing.T) {
	d, es, _ := mainBatchDB(t)
	batch, err := es.BeginMainWorkerBatch(3)
	if err != nil {
		t.Fatal(err)
	}
	a := batchIntent(t, es, batch, "done")
	b := batchIntent(t, es, batch, "running")
	batchIntent(t, es, 0, "running") // unrelated planner/worker cannot hold the batch
	ready := func(want int) {
		t.Helper()
		bs, err := d.ReadyMainWorkerBatches(100)
		if err != nil || len(bs) != want {
			t.Fatalf("ready=%v err=%v want=%d", bs, err, want)
		}
	}
	ready(0)
	if err := es.SealMainWorkerBatch(batch, false); err != nil {
		t.Fatal(err)
	}
	ready(0)
	if err := es.SetIntentState(b, "paused"); err != nil {
		t.Fatal(err)
	}
	ready(0)
	if err := es.SetIntentState(b, "exhausted"); err != nil {
		t.Fatal(err)
	}
	ready(1)
	bs, _ := d.ReadyMainWorkerBatches(100)
	if bs[0].MainSeg != 3 || bs[0].ExplorationID != es.ID() {
		t.Fatal("lost origin segment")
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := d.ClaimMainWorkerBatch(batch)
			if err != nil {
				t.Error(err)
			}
			if ok {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("duplicate admission: %d", claims.Load())
	}
	if err := d.FinishMainWorkerBatch(batch, "delivered"); err != nil {
		t.Fatal(err)
	}
	ready(0)
	// A later manual rerun of a member must not replay an already-delivered report.
	if err := es.SetIntentState(a, "open"); err != nil {
		t.Fatal(err)
	}
	if err := es.SetIntentState(a, "done"); err != nil {
		t.Fatal(err)
	}
	ready(0)
}

func TestMainWorkerBatchFastWorkerTerminalStatesAndRecovery(t *testing.T) {
	d, es, taskID := mainBatchDB(t)
	for _, state := range []string{"done", "blocked", "exhausted", "stopped", "deleted"} {
		batch, err := es.BeginMainWorkerBatch(0)
		if err != nil {
			t.Fatal(err)
		}
		batchIntent(t, es, batch, state)
		// Workers can finish BEFORE the main turn seals all tool calls.
		if ok, _ := d.ClaimMainWorkerBatch(batch); ok {
			t.Fatal("unsealed batch admitted")
		}
		if err := es.SealMainWorkerBatch(batch, false); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.ClaimMainWorkerBatch(batch); err != nil || !ok {
			t.Fatalf("%s: %v %v", state, ok, err)
		}
		if err := d.FinishMainWorkerBatch(batch, "delivered"); err != nil {
			t.Fatal(err)
		}
	}
	collecting, _ := es.BeginMainWorkerBatch(2)
	batchIntent(t, es, collecting, "done")
	delivering, _ := es.BeginMainWorkerBatch(2)
	batchIntent(t, es, delivering, "done")
	_ = es.SealMainWorkerBatch(delivering, false)
	if ok, _ := d.ClaimMainWorkerBatch(delivering); !ok {
		t.Fatal("claim fixture")
	}
	empty, _ := es.BeginMainWorkerBatch(0)
	if err := d.RecoverMainWorkerBatches(); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM main_worker_batches WHERE id=$1`, delivering).Scan(&state); err != nil || state != "interrupted" {
		t.Fatalf("unsafe delivery replay: %s %v", state, err)
	}
	bs, err := d.ReadyMainWorkerBatches(100)
	if err != nil || len(bs) != 1 || bs[0].ID != collecting {
		t.Fatalf("recovered=%v %v", bs, err)
	}
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM main_worker_batches WHERE id=$1`, empty).Scan(&n)
	if n != 0 {
		t.Fatal("empty recovery batch retained")
	}
	_, _ = d.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, taskID)
	if ok, _ := d.ClaimMainWorkerBatch(collecting); ok {
		t.Fatal("paused task auto-started")
	}
	_, _ = d.Exec(`UPDATE tasks SET paused=false,status='done' WHERE id=$1`, taskID)
	if ok, _ := d.ClaimMainWorkerBatch(collecting); !ok {
		t.Fatal("completed/goalless task incorrectly gated")
	}
}

func TestMainWorkerBatchResultsUseLatestAndStopCancelsWaiting(t *testing.T) {
	d, es, _ := mainBatchDB(t)
	batch, _ := es.BeginMainWorkerBatch(0)
	id := batchIntent(t, es, batch, "done")
	// Real worker traces can be longer than the old first-1,000-row output lookup.
	_, err := d.Exec(`INSERT INTO activity(exploration_id,node_id,worker,kind,summary,detail)
SELECT $1,$2,'worker','text','old','old' FROM generate_series(1,1005)`, es.ID(), id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO activity(exploration_id,node_id,worker,kind,summary,detail) VALUES
($1,$2,'worker','result','final','LATEST conclusion'),($1,$2,'worker','text','tail','later non-final text')`, es.ID(), id)
	if err != nil {
		t.Fatal(err)
	}
	results, total, err := d.MainWorkerBatchResults(MainWorkerBatch{ID: batch, ExplorationID: es.ID()})
	if err != nil || total != 1 || len(results) != 1 || results[0].Output != "LATEST conclusion" {
		t.Fatalf("results=%v total=%d err=%v", results, total, err)
	}
	_ = es.SealMainWorkerBatch(batch, false)
	if err := es.CancelMainWorkerBatches(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.ClaimMainWorkerBatch(batch); ok {
		t.Fatal("manual stop didn't revoke queued report")
	}
	empty, _ := es.BeginMainWorkerBatch(0)
	_ = es.SealMainWorkerBatch(empty, false)
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM main_worker_batches WHERE id=$1`, empty).Scan(&n)
	if n != 0 {
		t.Fatal(fmt.Sprintf("read-only batch retained: %d", n))
	}
}

func TestMainWorkerBatchReadyHeadsDoNotStarveOtherTasks(t *testing.T) {
	d, es, _ := mainBatchDB(t)
	var first int64
	for range 3 {
		b, _ := es.BeginMainWorkerBatch(0)
		if first == 0 {
			first = b
		}
		batchIntent(t, es, b, "done")
		if err := es.SealMainWorkerBatch(b, false); err != nil {
			t.Fatal(err)
		}
	}
	other, err := d.CreateTask("other batch fixture", "report only", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(other.ID) })
	otherStore := d.Exploration(other.ExplorationID)
	otherBatch, _ := otherStore.BeginMainWorkerBatch(0)
	batchIntent(t, otherStore, otherBatch, "done")
	if err := otherStore.SealMainWorkerBatch(otherBatch, false); err != nil {
		t.Fatal(err)
	}
	ready, err := d.ReadyMainWorkerBatches(2)
	if err != nil || len(ready) != 2 || ready[0].ID != first || ready[1].ID != otherBatch {
		t.Fatalf("same-task backlog hid another task: %v %v", ready, err)
	}
}
