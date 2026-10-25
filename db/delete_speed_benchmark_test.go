package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// This deliberately cannot run against the live ARTEX database. The restored
// copy is a network-disabled, disposable PostgreSQL, and only SQL is executed:
// no ARTEX server, scheduler, provider, or Agent is started. Every trial rolls
// back. Opt in with ARTEX_DELETE_SPEED_BENCH=1, the dedicated test DSN and
// ARTEX_DELETE_SPEED_CASES JSON (task, llm_records, activities). No production
// task IDs, counts, prompts or credentials are stored in the test source.
func TestRestoredTaskDeletionSpeed(t *testing.T) {
	if os.Getenv("ARTEX_DELETE_SPEED_BENCH") != "1" {
		t.Skip("offline restored-data benchmark only")
	}
	var cases []struct {
		Task        int64 `json:"task"`
		Records     int64 `json:"llm_records"`
		Activities  int64 `json:"activities"`
		Exploration int64 `json:"-"`
	}
	if err := json.Unmarshal([]byte(os.Getenv("ARTEX_DELETE_SPEED_CASES")), &cases); err != nil || len(cases) == 0 {
		t.Fatal("explicit restored-case metadata is required")
	}
	byTask := make(map[int64]int, len(cases))
	for i, c := range cases {
		if c.Task <= 0 || c.Records < 0 || c.Activities < 0 {
			t.Fatal("invalid case metadata")
		}
		byTask[c.Task] = i
	}
	dsn := testDSN(t)
	if !strings.Contains(dsn, "artex_bench_data") {
		t.Fatal("refusing non-benchmark DSN")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	d := &DB{raw}
	var database, user string
	if err := d.QueryRow(`SELECT current_database(),current_user`).Scan(&database, &user); err != nil || database != "artex_bench_data" || user != "artex_bench" {
		t.Fatalf("unsafe benchmark identity %q/%q: %v", database, user, err)
	}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	for i := range cases {
		if err := d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, cases[i].Task).Scan(&cases[i].Exploration); err != nil {
			t.Fatalf("restored task identity missing: %v", err)
		}
	}
	var baselineSignature string
	signature := func() string {
		t.Helper()
		var s string
		if err := d.QueryRow(`SELECT json_build_array(
 (SELECT count(*) FROM llm_records),(SELECT sum(id::numeric) FROM llm_records),
 (SELECT count(*) FROM activity),(SELECT sum(id::numeric) FROM activity),
 (SELECT count(*) FROM tasks),(SELECT sum(id::numeric) FROM tasks))::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	baselineSignature = signature()
	// ABBA ordering per task counterbalances cache/order effects. CHECKPOINT
	// between trials bounds scratch WAL and prevents dirty-page debt carrying on.
	type trialCase struct {
		task      int64
		optimized bool
	}
	var trials []trialCase
	for round := 0; round < 2; round++ {
		for i, c := range cases {
			optimizedFirst := (round+i)%2 == 1
			trials = append(trials, trialCase{c.Task, optimizedFirst}, trialCase{c.Task, !optimizedFirst})
		}
	}
	for _, trial := range trials {
		if _, err := d.Exec(`CHECKPOINT`); err != nil {
			t.Fatal(err)
		}
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`SET LOCAL statement_timeout='30s'; SET LOCAL lock_timeout='5s'`); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		mode := "baseline-128-fixed-10ms"
		if trial.optimized {
			mode = "bounded-bytes-adaptive-yield"
		}
		start := time.Now()
		phase := start
		run := legacyDeleteSpeedBatch
		if trial.optimized {
			run = deleteOwnedRowsBatched
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		n, err := run(ctx, tx, "llm_records", fmt.Sprint(trial.task))
		llmSeconds := time.Since(phase).Seconds()
		phase = time.Now()
		if err != nil {
			cancel()
			tx.Rollback()
			t.Fatal(err)
		}
		a, err := run(ctx, tx, "activity", cases[byTask[trial.task]].Exploration)
		activitySeconds := time.Since(phase).Seconds()
		cancel()
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		elapsed := time.Since(start).Seconds()
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		want := cases[byTask[trial.task]]
		wantRecords, wantActivities := want.Records, want.Activities
		if n != wantRecords || a != wantActivities {
			t.Fatalf("wrong restored dataset: %d/%d", n, a)
		}
		row := map[string]any{"task": trial.task, "mode": mode, "llm_records": n, "activities": a, "llm_seconds": llmSeconds, "activity_seconds": activitySeconds, "total_seconds": elapsed, "rolled_back": true}
		b, _ := json.Marshal(row)
		t.Log("SPEED_RESULT " + string(b))
		if signature() != baselineSignature {
			t.Fatal("trial changed restored records or task identities")
		}
	}
}

// Exact pre-optimization bb2526b algorithm, retained only in this opt-in test.
func legacyDeleteSpeedBatch(ctx context.Context, tx *sql.Tx, table string, owner any) (int64, error) {
	where := "task_id=$1"
	if table == "activity" {
		where = "exploration_id=$1"
	} else if table != "llm_records" {
		return 0, fmt.Errorf("unsupported benchmark table")
	}
	q := fmt.Sprintf(`WITH batch AS (SELECT id FROM %[1]s WHERE %[2]s AND id >= $2 ORDER BY id LIMIT $3), removed AS (DELETE FROM %[1]s target USING batch WHERE target.id=batch.id RETURNING target.id) SELECT count(*),COALESCE(max(id),$2) FROM removed`, table, where)
	cursor := int64(math.MinInt64)
	var total int64
	for {
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		var count, last int64
		err := tx.QueryRowContext(step, q, owner, cursor, 128).Scan(&count, &last)
		cancel()
		if err != nil {
			return total, err
		}
		total += count
		if count == 0 || last == math.MaxInt64 {
			return total, nil
		}
		cursor = last + 1
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
	}
}
