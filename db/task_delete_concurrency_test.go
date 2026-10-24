package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestTaskDeleteClearsActivityBeforeNodeCascadeAndRollsBackAtomically(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, err := d.CreateTask("bulk deletion fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateTask("retained activity fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(first.ID); _ = d.DeleteTask(second.ID) })
	var hasFKIndex bool
	if err := d.QueryRow(`SELECT EXISTS (
 SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attname='node_id'
 WHERE i.indrelid='activity'::regclass AND i.indisvalid AND i.indpred IS NULL
 AND (i.indkey::smallint[])[0]=a.attnum)`).Scan(&hasFKIndex); err != nil || !hasFKIndex {
		t.Fatalf("node_id-only FK lookup has no leading index: %v", err)
	}
	var origin int64
	if origin, err = d.Exploration(first.ExplorationID).OriginFactID(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload)
 SELECT $1,'intent','open','{}'::jsonb FROM generate_series(1,512)`, first.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO activity(exploration_id,node_id,kind)
 SELECT n.exploration_id,n.id,'text' FROM exploration_nodes n CROSS JOIN generate_series(1,16)
 WHERE n.exploration_id=$1`, first.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO activity(exploration_id,kind) SELECT $1,'text' FROM generate_series(1,20000)`, second.ExplorationID); err != nil {
		t.Fatal(err)
	}
	var crossReference int64
	if err := d.QueryRow(`INSERT INTO activity(exploration_id,node_id,kind) VALUES($1,$2,'text') RETURNING id`, second.ExplorationID, origin).Scan(&crossReference); err != nil {
		t.Fatal(err)
	}
	var ownedBefore int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, first.ExplorationID).Scan(&ownedBefore); err != nil {
		t.Fatal(err)
	}
	// The guard makes the old update-then-delete order fail deterministically,
	// without a flaky timing assertion or using any production task contents.
	fn := fmt.Sprintf("test_delete_guard_%d", first.ID)
	activityTrigger := fn + "_activity"
	explorationTrigger := fn + "_exploration"
	if _, err := d.Exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deletion guard'; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DROP TRIGGER IF EXISTS ` + activityTrigger + ` ON activity`)
		_, _ = d.Exec(`DROP TRIGGER IF EXISTS ` + explorationTrigger + ` ON explorations`)
		_, _ = d.Exec(`DROP FUNCTION IF EXISTS ` + fn + `() `)
	})
	if _, err := d.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF node_id ON activity FOR EACH ROW WHEN (OLD.exploration_id=%d) EXECUTE FUNCTION %s()`, activityTrigger, first.ExplorationID, fn)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE DELETE ON explorations FOR EACH ROW WHEN (OLD.id=%d) EXECUTE FUNCTION %s()`, explorationTrigger, first.ExplorationID, fn)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteTaskCascadePreparedContext(t.Context(), first.ID, false, false, false, nil); err == nil || !strings.Contains(err.Error(), "删除任务图谱") {
		t.Fatalf("expected downstream failure after activity preparation: %v", err)
	}
	var ownedAfter int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, first.ExplorationID).Scan(&ownedAfter); err != nil || ownedAfter != ownedBefore {
		t.Fatalf("failed delete lost activity: before=%d after=%d err=%v", ownedBefore, ownedAfter, err)
	}
	var retainedNode sql.NullInt64
	if err := d.QueryRow(`SELECT node_id FROM activity WHERE id=$1`, crossReference).Scan(&retainedNode); err != nil || !retainedNode.Valid || retainedNode.Int64 != origin {
		t.Fatalf("failed deletion mutated another task's reference: %v %v", retainedNode, err)
	}
	if _, err := d.Exec(`DROP TRIGGER ` + explorationTrigger + ` ON explorations`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := d.DeleteTaskCascadePreparedContext(ctx, first.ID, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("deleted 512+ nodes and %d owned activities with 20001 retained activities in %s", ownedBefore, time.Since(started))
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, first.ExplorationID).Scan(&ownedAfter); err != nil || ownedAfter != 0 {
		t.Fatalf("owned activity remained: %d %v", ownedAfter, err)
	}
	if err := d.QueryRow(`SELECT node_id FROM activity WHERE id=$1`, crossReference).Scan(&retainedNode); err != nil || retainedNode.Valid {
		t.Fatalf("retained cross-reference was not nullified: %v %v", retainedNode, err)
	}
	var retainedCount int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, second.ExplorationID).Scan(&retainedCount); err != nil || retainedCount != 20001 {
		t.Fatalf("other task activity was deleted: %d %v", retainedCount, err)
	}
}

func TestTaskDeleteContextCancelsBlockedStatement(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, second, _, _ := createTaskDeleteRaceFixture(t, d)
	t.Cleanup(func() { _ = d.DeleteTask(first.ID); _ = d.DeleteTask(second.ID) })
	writer, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec(`SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, first.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = d.DeleteTaskCascadePreparedContext(ctx, first.ID, true, false, false, nil)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("blocked delete ignored cancellation: %v (%s)", err, time.Since(started))
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := d.QueryRow(`SELECT count(*) FROM tasks WHERE id=$1`, first.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("cancelled delete removed data: rows=%d err=%v", rows, err)
	}
}

func TestDeleteTaskTrafficHostsUseOneLockedTransaction(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()

	t.Run("sharing committed before deletion lock is observed", func(t *testing.T) {
		first, second, host, rootAssetID := createTaskDeleteRaceFixture(t, d)
		serviceURL := "https://" + host + "/concurrent-owner"
		t.Cleanup(func() {
			_ = d.DeleteTask(first.ID)
			_ = d.DeleteTask(second.ID)
			_, _ = d.Exec(`DELETE FROM assets WHERE id=$1 OR url=$2`, rootAssetID, serviceURL)
		})

		writer, _ := openTaskDeleteTestDB(t, dsn)
		writerTx, err := writer.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer writerTx.Rollback()
		if _, err := writerTx.Exec(`
INSERT INTO assets(type, url, service_type, domain, task_ids)
VALUES ('service', $1, 'http', $2, ARRAY[$3]::bigint[])`, serviceURL, host, second.ID); err != nil {
			t.Fatal(err)
		}

		deleter, deleterPID := openTaskDeleteTestDB(t, dsn)
		var preparedHosts []string
		deleteDone := make(chan error, 1)
		go func() {
			_, deleteErr := deleter.DeleteTaskCascadePrepared(first.ID, true, false, false, func(p TaskDeletePreparation) error {
				preparedHosts = append([]string(nil), p.TrafficHosts...)
				return nil
			})
			deleteDone <- deleteErr
		}()

		if err := waitForTaskDeleteBlock(d, deleterPID, deleteDone); err != nil {
			t.Fatal(err)
		}
		if err := writerTx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := waitForTaskDeleteResult(deleteDone); err != nil {
			t.Fatal(err)
		}
		if containsDeleteHost(preparedHosts, host) {
			t.Fatalf("newly shared host %q was selected for traffic deletion: %v", host, preparedHosts)
		}
		var remaining int
		if err := d.QueryRow(`SELECT count(*) FROM assets WHERE url=$1 AND $2=ANY(task_ids)`, serviceURL, second.ID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 1 {
			t.Fatalf("concurrent owner's asset was not preserved: count=%d", remaining)
		}
	})

	t.Run("sharing cannot enter between host resolution and commit", func(t *testing.T) {
		first, second, host, rootAssetID := createTaskDeleteRaceFixture(t, d)
		serviceURL := "https://" + host + "/late-owner"
		t.Cleanup(func() {
			_ = d.DeleteTask(first.ID)
			_ = d.DeleteTask(second.ID)
			_, _ = d.Exec(`DELETE FROM assets WHERE id=$1 OR url=$2`, rootAssetID, serviceURL)
		})

		deleter, _ := openTaskDeleteTestDB(t, dsn)
		prepared := make(chan TaskDeletePreparation, 1)
		releasePrepare := make(chan struct{})
		deleteDone := make(chan error, 1)
		go func() {
			_, deleteErr := deleter.DeleteTaskCascadePrepared(first.ID, true, false, false, func(p TaskDeletePreparation) error {
				prepared <- p
				<-releasePrepare
				return nil
			})
			deleteDone <- deleteErr
		}()

		var plan TaskDeletePreparation
		select {
		case plan = <-prepared:
		case err := <-deleteDone:
			close(releasePrepare)
			t.Fatalf("delete returned before preparation: %v", err)
		case <-time.After(5 * time.Second):
			close(releasePrepare)
			t.Fatal("timed out waiting for task delete preparation")
		}
		if !containsDeleteHost(plan.TrafficHosts, host) {
			close(releasePrepare)
			t.Fatalf("exclusive host %q missing from preparation: %v", host, plan.TrafficHosts)
		}

		writer, writerPID := openTaskDeleteTestDB(t, dsn)
		writerDone := make(chan error, 1)
		go func() {
			_, writeErr := writer.Exec(`
INSERT INTO assets(type, url, service_type, domain, task_ids)
VALUES ('service', $1, 'http', $2, ARRAY[$3]::bigint[])`, serviceURL, host, second.ID)
			writerDone <- writeErr
		}()
		if err := waitForTaskDeleteBlock(d, writerPID, writerDone); err != nil {
			close(releasePrepare)
			t.Fatal(err)
		}

		close(releasePrepare)
		if err := waitForTaskDeleteResult(deleteDone); err != nil {
			t.Fatal(err)
		}
		if err := waitForTaskDeleteResult(writerDone); err != nil {
			t.Fatal(err)
		}
	})
}

func createTaskDeleteRaceFixture(t *testing.T, d *DB) (first, second *Task, host string, rootAssetID int64) {
	t.Helper()
	var err error
	first, err = d.CreateTask("delete race owner", "delete safely", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err = d.CreateTask("delete race sharer", "preserve shared host", nil, 0, 0)
	if err != nil {
		_ = d.DeleteTask(first.ID)
		t.Fatal(err)
	}
	host = fmt.Sprintf("task-delete-race-%d.example.test", first.ID)
	rootAssetID, err = d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: host, TaskID: first.ID})
	if err != nil {
		_ = d.DeleteTask(first.ID)
		_ = d.DeleteTask(second.ID)
		t.Fatal(err)
	}
	return first, second, host, rootAssetID
}

func openTaskDeleteTestDB(t *testing.T, dsn string) (*DB, int) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := sqlDB.Exec(`SET statement_timeout='10s'`); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := sqlDB.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return &DB{sqlDB}, pid
}

func waitForTaskDeleteBlock(observer *DB, pid int, done <-chan error) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return fmt.Errorf("operation returned before reaching the task deletion lock: %v", err)
		default:
		}
		var blockers int
		if err := observer.QueryRow(`SELECT cardinality(pg_blocking_pids($1))`, pid).Scan(&blockers); err != nil {
			return err
		}
		if blockers > 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("backend %d did not block within 5s", pid)
}

func waitForTaskDeleteResult(done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(12 * time.Second):
		return fmt.Errorf("timed out waiting for concurrent task deletion operation")
	}
}

func containsDeleteHost(hosts []string, want string) bool {
	for _, host := range hosts {
		if host == want {
			return true
		}
	}
	return false
}
