package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	maintenanceReserve = 256 << 20
	deleteBudget       = 30 * time.Second
	reclaimStepBudget  = 2 * time.Second
	checkpointBudget   = time.Second
	reclaimYield       = 25 * time.Millisecond
)

// Unlike a deadline checked BETWEEN SQL statements, this context interrupts an
// executing SQLite statement. Close must also interrupt maintenance in progress.
func (t *Traffic) maintenanceContext(budget time.Duration) (context.Context, context.CancelFunc) {
	return t.maintenanceContextFrom(context.Background(), budget)
}

func (t *Traffic) maintenanceContextFrom(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, budget)
	if t.stopping() {
		cancel()
		return ctx, cancel
	}
	go func() {
		select {
		case <-t.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (t *Traffic) requireSpace(required uint64) error {
	probe := t.freeSpace
	if probe == nil {
		probe = diskAvailable
	}
	available, err := probe(t.dir)
	if err != nil {
		return fmt.Errorf("检查流量存储可用空间: %w", err)
	}
	if available < required {
		return fmt.Errorf("流量存储空间不足：当前可用 %d MiB，需要至少 %d MiB 临时空间；请先释放磁盘空间，不要重复提交删除", available>>20, required>>20)
	}
	return nil
}

// SQLite deletions also WRITE (tombstones, freelists and WAL). Allow room for
// one index-sized transaction plus a reserve instead of filling the root disk
// and taking unrelated PostgreSQL/services down in an attempt to free space.
func (t *Traffic) requireDeleteSpace() error {
	return t.requireSpace(uint64(t.indexBytes()) + maintenanceReserve)
}

// TRUNCATE reports pinned readers through a result column, not an SQL error.
// Never continue writing new maintenance chunks while an older WAL cannot drain.
func checkpointIndex(ctx context.Context, conn *sql.Conn) (bool, error) {
	var busy, frames, checkpointed int
	err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed)
	return err == nil && busy == 0, err
}

// Only the first FTS5 merge is forced. Negative ranks on EVERY chunk restart an
// in-progress merge whenever the recorder adds a segment. Positive ranks resume
// it across chunks AND across budget-limited background passes.
func (t *Traffic) mergeIndex(ctx context.Context, conn *sql.Conn) (bool, error) {
	rank := reclaimMergePages
	if !t.mergeStarted {
		rank = -rank
	}
	var before, after int64
	if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO ex_fts(ex_fts, rank) VALUES('merge', ?)`, rank); err != nil {
		return false, err
	}
	if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
		return false, err
	}
	// The special INSERT itself contributes one change. SQLite explicitly
	// documents >=2 as real merge work; <=1 is a no-op, so stop immediately.
	progressed := after-before >= 2
	t.mergeStarted = progressed
	return progressed, nil
}
