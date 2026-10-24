package traffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// FTS5 may run automatic or crisis merges INSIDE a DELETE. Those merges are
// unrelated to making rows invisible and can rewrite an entire multi-GB index
// before our bounded background reclaimer ever gets control. Suspend them only
// for this transaction's delete, flush pending tombstones while still suspended,
// then restore the caller's effective settings. Rollback restores these settings
// as well. merge(0) flushes without requesting any explicit merge work.
func deleteFTSWhere(ctx context.Context, tx *sql.Tx, where string, args ...any) error {
	if where == "1=1" {
		_, err := tx.ExecContext(ctx, `INSERT INTO ex_fts(ex_fts) VALUES('delete-all')`)
		return err
	}
	type setting struct {
		name                       string
		fallback, suspended, saved int
	}
	settings := []setting{{"automerge", 4, 0, 0}, {"crisismerge", 16, 1 << 30, 0}, {"deletemerge", 10, 0, 0}}
	for i := range settings {
		s := &settings[i]
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT v FROM ex_fts_config WHERE k=?), ?)`, s.name, s.fallback).Scan(&s.saved); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ex_fts(ex_fts, rank) VALUES(?, ?)`, s.name, s.suspended); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ex_fts WHERE rowid IN (SELECT rowid FROM exchanges WHERE `+where+`)`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ex_fts(ex_fts, rank) VALUES('merge', 0)`); err != nil {
		return err
	}
	for _, s := range settings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ex_fts(ex_fts, rank) VALUES(?, ?)`, s.name, s.saved); err != nil {
			return err
		}
	}
	return nil
}

// SQLite can automatically roll back a write transaction on SQLITE_INTERRUPT.
// database/sql still owns a Tx, so a subsequent Rollback receives SQLITE_ERROR
// "no transaction is active". Only that exact SQLite condition is idempotent;
// do not hide I/O errors or arbitrary rollback failures.
func rollbackAlreadyComplete(err error) bool {
	if err == nil || errors.Is(err, sql.ErrTxDone) {
		return true
	}
	var sqliteErr interface{ Code() int }
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == 1 &&
		strings.Contains(err.Error(), "cannot rollback - no transaction is active")
}

func (t *Traffic) rollbackDelete(tx *sql.Tx) error {
	err := tx.Rollback()
	if !rollbackAlreadyComplete(err) {
		return err
	}
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	// A cancelled DELETE may have left a large WAL even though none of its
	// changes committed. Drain it without starting an FTS merge or a vacuum.
	ctx, cancel := context.WithTimeout(context.Background(), checkpointBudget)
	defer cancel()
	conn, connErr := t.db.Conn(ctx)
	if connErr != nil {
		return nil
	}
	defer conn.Close()
	if _, err := checkpointIndex(ctx, conn); err != nil && !t.stopping() {
		log.Printf("[traffic] 删除已回滚，WAL 检查点暂未完成：%v", err)
	}
	return nil
}

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
