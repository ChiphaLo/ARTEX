package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTaskDeleteLLMBatchesRemainAtomicAndKeepUsage(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		d.Close()
		t.Fatal(err)
	}
	first, err := d.CreateTask("batch cascade fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateTask("batch retained fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner, other := strconv.FormatInt(first.ID, 10), strconv.FormatInt(second.ID, 10)
	defer func() {
		_, _ = d.Exec(`DELETE FROM llm_records WHERE task_id IN ($1,$2)`, owner, other)
		_, _ = d.Exec(`DELETE FROM llm_usage WHERE task_id IN ($1,$2)`, owner, other)
		_ = d.DeleteTask(first.ID)
		_ = d.DeleteTask(second.ID)
		d.Close()
	}()
	if _, err := d.Exec(`INSERT INTO llm_records(task_id,raw_request) SELECT $1,repeat('synthetic',1000) FROM generate_series(1,513)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO llm_records(task_id) VALUES($1)`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO activity(exploration_id,kind) SELECT $1,'text' FROM generate_series(1,513)`, first.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO llm_usage(task_id,input_tokens,output_tokens) VALUES($1,123,456)`, owner); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic external preparation failure")
	result, err := d.DeleteTaskCascadePreparedContext(t.Context(), first.ID, true, true, true, func(TaskDeletePreparation) error { return failure })
	if !errors.Is(err, failure) || result.LLMRecordsDeleted != 513 {
		t.Fatalf("did not reach preparation after batching: %+v %v", result, err)
	}
	var records, activities, taskRows int
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM llm_records WHERE task_id=$1),(SELECT count(*) FROM activity WHERE exploration_id=$2),(SELECT count(*) FROM tasks WHERE id=$3)`, owner, first.ExplorationID, first.ID).Scan(&records, &activities, &taskRows); err != nil || records != 513 || activities != 513 || taskRows != 1 {
		t.Fatalf("cascade failure was not atomic: records=%d activities=%d task=%d err=%v", records, activities, taskRows, err)
	}
	result, err = d.DeleteTaskCascadePreparedContext(t.Context(), first.ID, true, true, true, nil)
	if err != nil || result.LLMRecordsDeleted != 513 {
		t.Fatalf("full task delete=%+v %v", result, err)
	}
	var usageInput, usageOutput, retained int
	if err := d.QueryRow(`SELECT input_tokens,output_tokens FROM llm_usage WHERE task_id=$1`, owner).Scan(&usageInput, &usageOutput); err != nil || usageInput != 123 || usageOutput != 456 {
		t.Fatalf("metering ledger changed: %d/%d %v", usageInput, usageOutput, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE task_id=$1`, other).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("other owner's records changed: %d %v", retained, err)
	}
	if _, err := d.DeleteTaskCascadePreparedContext(t.Context(), first.ID, true, true, true, nil); err != nil {
		t.Fatalf("repeat deletion not idempotent: %v", err)
	}
}

func TestDeleteBatchesSurviveStatementLimitAndRollbackAllCompletedBatches(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		d.Close()
		t.Fatal(err)
	}
	owner := fmt.Sprintf("batch-timeout-%d", time.Now().UnixNano())
	other := owner + "0"
	fn := fmt.Sprintf("test_batch_delay_%d", time.Now().UnixNano())
	defer func() {
		_, _ = d.Exec(`DROP TRIGGER IF EXISTS ` + fn + ` ON llm_records`)
		_, _ = d.Exec(`DROP FUNCTION IF EXISTS ` + fn + `() `)
		_, _ = d.Exec(`DELETE FROM llm_records WHERE task_id IN ($1,$2)`, owner, other)
		d.Close()
	}()
	// Incompressible-ish synthetic TOAST data; no model or real task contents.
	if _, err := d.Exec(`INSERT INTO llm_records(task_id,raw_request)
 SELECT $1,(SELECT string_agg(md5(g::text||n::text),'') FROM generate_series(1,600) g)
 FROM generate_series(1,513) n`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO llm_records(task_id) SELECT $1 FROM generate_series(1,3)`, other); err != nil {
		t.Fatal(err)
	}
	var cutoff int64
	if err := d.QueryRow(`SELECT id FROM llm_records WHERE task_id=$1 ORDER BY id OFFSET 127 LIMIT 1`, owner).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE n bigint; last_id bigint;
 BEGIN
 SELECT count(*),max(id) INTO n,last_id FROM removed_batch WHERE task_id='%s';
 IF current_setting('artex.fail_batch',true)='yes' AND last_id>%d THEN
 RAISE EXCEPTION 'synthetic second batch failure'; END IF;
 PERFORM pg_sleep(n*0.01); RETURN NULL;
 END $$`, fn, owner, cutoff)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER ` + fn + ` AFTER DELETE ON llm_records REFERENCING OLD TABLE AS removed_batch FOR EACH STATEMENT EXECUTE FUNCTION ` + fn + `() `); err != nil {
		t.Fatal(err)
	}
	countOwner := func(want int) {
		t.Helper()
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE task_id=$1`, owner).Scan(&n); err != nil || n != want {
			t.Fatalf("owner rows=%d want=%d err=%v", n, want, err)
		}
	}
	legacy, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`SET LOCAL statement_timeout='2s'`); err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`DELETE FROM llm_records WHERE COALESCE(task_id,'')=$1`, owner)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("legacy bulk DELETE did not reproduce timeout: %v", err)
	}
	_ = legacy.Rollback()
	countOwner(513)

	// At least one batch commits to the TRANSACTION, not the database, before
	// cancellation/failure. Both must restore every row, not leave partial purges.
	for _, cancelRequest := range []bool{true, false} {
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`SET LOCAL statement_timeout='2s'`); err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		cancel := func() {}
		if cancelRequest {
			ctx, cancel = context.WithTimeout(ctx, 1800*time.Millisecond)
		} else {
			if _, err := tx.Exec(`SET LOCAL artex.fail_batch='yes'`); err != nil {
				t.Fatal(err)
			}
		}
		n, err := deleteOwnedRowsBatched(ctx, tx, "llm_records", owner)
		cancel()
		_ = tx.Rollback()
		if err == nil || n < deleteBatchRows {
			t.Fatalf("failure did not occur after a completed batch: n=%d err=%v", n, err)
		}
		if !cancelRequest && !strings.Contains(err.Error(), "synthetic second batch failure") {
			t.Fatal(err)
		}
		countOwner(513)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SET LOCAL statement_timeout='2s'`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	started := time.Now()
	n, err := deleteOwnedRowsBatched(ctx, tx, "llm_records", owner)
	if err != nil || n != 513 {
		t.Fatalf("bounded deletion=(%d,%v)", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	countOwner(0)
	var retained int
	if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE task_id=$1`, other).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("other owner rows=%d err=%v", retained, err)
	}
	t.Logf("513 TOAST records deleted in %s under a 2-second per-statement limit; timeout and downstream failure both rolled back completed batches", time.Since(started))
}

func TestDeleteLLMRecordsContextExactOwnershipAndExtremeIDs(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprintf("exact-owner-%d-7", time.Now().UnixNano())
	other := owner + "0"
	defer d.Exec(`DELETE FROM llm_records WHERE task_id IN ($1,$2)`, owner, other)
	if _, err := d.Exec(`INSERT INTO llm_records(id,task_id) VALUES($1,$3),($2,$3)`, int64(math.MinInt64), int64(math.MaxInt64), owner); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO llm_records(task_id) VALUES($1)`, other); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.DeleteLLMRecordsContext(ctx, owner); err == nil {
		t.Fatal("cancelled request succeeded")
	}
	if n, err := d.DeleteLLMRecordsContext(t.Context(), owner); err != nil || n != 2 {
		t.Fatalf("extreme-id deletion=(%d,%v)", n, err)
	}
	var retained int
	if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE task_id=$1`, other).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("non-exact owner changed: %d %v", retained, err)
	}
}

func TestDeleteBatchesBoundPayloadBytesAndAllowOneOversizedRow(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		d.Close()
		t.Fatal(err)
	}
	task, err := d.CreateTask("byte-limited deletion fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner := strconv.FormatInt(task.ID, 10)
	name := fmt.Sprintf("test_delete_bytes_%d", time.Now().UnixNano())
	defer func() {
		for _, table := range []string{"llm_records", "activity"} {
			_, _ = d.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON ` + table)
		}
		_, _ = d.Exec(`DROP FUNCTION IF EXISTS ` + name + `() `)
		_, _ = d.Exec(`DELETE FROM llm_records WHERE task_id=$1`, owner)
		_ = d.DeleteTask(task.ID)
		d.Close()
	}()
	// MD5 streams avoid compressible padding: six 3.2MB records plus one 9.6MB
	// record force the byte limit, even though there are fewer than 128 rows.
	for _, table := range []string{"llm_records", "activity"} {
		column, ownerColumn := "raw_request", "task_id"
		var value any = owner
		if table == "activity" {
			column, ownerColumn = "detail", "exploration_id"
			value = task.ExplorationID
		}
		query := fmt.Sprintf(`INSERT INTO %s(%s,%s) SELECT $1,(SELECT string_agg(md5(g::text||':'||n::text),'') FROM generate_series(1,CASE WHEN n=7 THEN 300000 ELSE 100000 END) g) FROM generate_series(1,7) n`, table, ownerColumn, column)
		if _, err := d.Exec(query, value); err != nil {
			t.Fatal(err)
		}
		var maxBytes int64
		if err := d.QueryRow(fmt.Sprintf(`SELECT max(pg_column_size(%s)) FROM %s WHERE %s=$1`, column, table, ownerColumn), value).Scan(&maxBytes); err != nil || maxBytes <= deleteBatchBytes {
			t.Fatalf("oversized fixture missing: %d %v", maxBytes, err)
		}
	}
	if _, err := d.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE n bigint; bytes bigint;
 BEGIN
 IF TG_TABLE_NAME='llm_records' THEN
  SELECT count(*),coalesce(sum(128::bigint+coalesce(pg_column_size(raw_request),0)),0) INTO n,bytes FROM deleted_byte_rows WHERE task_id='%s';
 ELSE
  SELECT count(*),coalesce(sum(128::bigint+coalesce(pg_column_size(detail),0)+coalesce(pg_column_size(metadata),0)),0) INTO n,bytes FROM deleted_byte_rows WHERE exploration_id=%d;
 END IF;
 IF n>1 AND bytes>%d THEN RAISE EXCEPTION 'batch byte limit exceeded'; END IF;
 RETURN NULL; END $$`, name, owner, task.ExplorationID, deleteBatchBytes)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"llm_records", "activity"} {
		if _, err := d.Exec(`CREATE TRIGGER ` + name + ` AFTER DELETE ON ` + table + ` REFERENCING OLD TABLE AS deleted_byte_rows FOR EACH STATEMENT EXECUTE FUNCTION ` + name + `() `); err != nil {
			t.Fatal(err)
		}
		var value any = owner
		if table == "activity" {
			value = task.ExplorationID
		}
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		n, err := deleteOwnedRowsBatched(t.Context(), tx, table, value)
		if err != nil || n != 7 {
			_ = tx.Rollback()
			t.Fatalf("%s byte-bounded delete=(%d,%v)", table, n, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		var remaining int
		col := "task_id"
		if table == "activity" {
			col = "exploration_id"
		}
		if err := d.QueryRow(`SELECT count(*) FROM `+table+` WHERE `+col+`=$1`, value).Scan(&remaining); err != nil || remaining != 7 {
			t.Fatalf("rollback lost byte fixture rows: %d %v", remaining, err)
		}
	}
}
