package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

const (
	taskDeleteBudget  = 5 * time.Minute
	deleteBatchRows   = 128
	deleteBatchBudget = 10 * time.Second
)

// A few thousand LLM records can own GBs of TOAST chunks. One unbounded DELETE
// makes statement_timeout cover the whole dataset, not one unit of work. Keep
// every batch in the caller's SAME transaction: cancellation or a later failure
// rolls back ALL batches, including their TOAST rows. Keyset progression avoids
// repeatedly scanning already-deleted index entries in this open transaction.
func deleteOwnedRowsBatched(ctx context.Context, tx *sql.Tx, table string, owner any) (int64, error) {
	var where string
	switch table {
	case "llm_records":
		where = "task_id=$1"
		if owner == "" {
			where = "(task_id IS NULL OR task_id=$1)"
		}
	case "activity":
		where = "exploration_id=$1"
	default:
		return 0, fmt.Errorf("unsupported deletion table %q", table)
	}
	query := fmt.Sprintf(`WITH batch AS (
 SELECT id FROM %[1]s WHERE %[2]s AND id >= $2 ORDER BY id LIMIT $3
), removed AS (
 DELETE FROM %[1]s target USING batch WHERE target.id=batch.id RETURNING target.id
)
SELECT count(*),COALESCE(max(id),$2) FROM removed`, table, where)
	cursor := int64(math.MinInt64)
	var total int64
	for {
		step, cancel := context.WithTimeout(ctx, deleteBatchBudget)
		var count, last int64
		err := tx.QueryRowContext(step, query, owner, cursor, deleteBatchRows).Scan(&count, &last)
		cancel()
		if err != nil {
			return total, fmt.Errorf("分块删除 %s（已处理 %d 条，事务尚未提交）: %w", table, total, err)
		}
		total += count
		if count == 0 || last == math.MaxInt64 {
			return total, nil
		}
		cursor = last + 1
		// Yield between bounded batches. Never commit partial deletion to achieve
		// progress, and never sleep through a request cancellation.
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
	}
}
