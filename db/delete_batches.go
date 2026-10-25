package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

const (
	taskDeleteBudget        = 5 * time.Minute
	deleteBatchRows         = 128
	deleteActivityBatchRows = 1024
	deleteBatchBytes        = 8 << 20
	deleteBatchBudget       = 10 * time.Second
	deleteYieldInterval     = 100 * time.Millisecond
	deleteYieldDuration     = 5 * time.Millisecond
)

// A few thousand LLM records can own GBs of TOAST chunks. One unbounded DELETE
// makes statement_timeout cover the whole dataset, not one unit of work. Keep
// every batch in the caller's SAME transaction: cancellation or a later failure
// rolls back ALL batches, including their TOAST rows. Keyset progression avoids
// repeatedly scanning already-deleted index entries in this open transaction.
func deleteOwnedRowsBatched(ctx context.Context, tx *sql.Tx, table string, owner any) (int64, error) {
	var where string
	var payloadBytes string
	rows := deleteBatchRows
	switch table {
	case "llm_records":
		where = "task_id=$1"
		payloadBytes = `128::bigint + COALESCE(pg_column_size(request_body),0) + COALESCE(pg_column_size(response_body),0) + COALESCE(pg_column_size(raw_request),0) + COALESCE(pg_column_size(raw_response),0) + COALESCE(pg_column_size(error),0)`
		if owner == "" {
			where = "(task_id IS NULL OR task_id=$1)"
		}
	case "activity":
		where = "exploration_id=$1"
		rows = deleteActivityBatchRows
		payloadBytes = `128::bigint + COALESCE(pg_column_size(detail),0) + COALESCE(pg_column_size(metadata),0) + COALESCE(pg_column_size(summary),0) + COALESCE(pg_column_size(tool),0) + COALESCE(pg_column_size(tool_use_id),0) + COALESCE(pg_column_size(worker),0) + COALESCE(pg_column_size(kind),0)`
	default:
		return 0, fmt.Errorf("unsupported deletion table %q", table)
	}
	// Only inspect the size stored in each column's TOAST pointer, not its body.
	// A row limit alone allows a batch of large bodies to monopolize disk I/O.
	// Materialize a bounded candidate set BEFORE applying the byte window so the
	// planner never sizes every remaining row. One oversized row can still advance.
	query := fmt.Sprintf(`WITH candidates AS MATERIALIZED (
 SELECT id,%[3]s AS payload_bytes FROM %[1]s WHERE %[2]s AND id >= $2 ORDER BY id LIMIT $3
), batch AS (
 SELECT id FROM (
  SELECT id,row_number() OVER (ORDER BY id) AS ordinal,
         sum(payload_bytes) OVER (ORDER BY id) AS cumulative_bytes FROM candidates
 ) sized WHERE cumulative_bytes <= $4 OR ordinal=1
), removed AS (
 DELETE FROM %[1]s target USING batch WHERE target.id=batch.id RETURNING target.id
)
SELECT count(*),COALESCE(max(id),$2) FROM removed`, table, where, payloadBytes)
	cursor := int64(math.MinInt64)
	var total int64
	lastYield := time.Now()
	for {
		step, cancel := context.WithTimeout(ctx, deleteBatchBudget)
		var count, last int64
		err := tx.QueryRowContext(step, query, owner, cursor, rows, deleteBatchBytes).Scan(&count, &last)
		cancel()
		if err != nil {
			return total, fmt.Errorf("分块删除 %s（已处理 %d 条，事务尚未提交）: %w", table, total, err)
		}
		total += count
		if count == 0 || last == math.MaxInt64 {
			return total, nil
		}
		cursor = last + 1
		if err := ctx.Err(); err != nil {
			return total, err
		}
		// The old fixed 10ms pause after every 128 rows added ~24 seconds to
		// 300K activities even on an idle server. Yield by elapsed work instead;
		// keep cancellation and I/O/statement bounds, never commit partial work.
		if time.Since(lastYield) < deleteYieldInterval {
			continue
		}
		timer := time.NewTimer(deleteYieldDuration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
			lastYield = time.Now()
		}
	}
}
