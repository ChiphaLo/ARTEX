package traffic

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type maintenanceStub struct {
	mu          sync.Mutex
	changes     int64
	work        []int64
	ranks       []int64
	free        int64
	checkpoints int
	busy        int64
	block       bool
	entered     chan struct{}
	once        sync.Once
}

type maintenanceDriver struct{ state *maintenanceStub }
type maintenanceConn struct{ state *maintenanceStub }
type maintenanceRows struct {
	values []driver.Value
	done   bool
}

func (d maintenanceDriver) Open(string) (driver.Conn, error)   { return &maintenanceConn{d.state}, nil }
func (c *maintenanceConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (c *maintenanceConn) Close() error                        { return nil }
func (c *maintenanceConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }
func (r *maintenanceRows) Columns() []string {
	return strings.Split(strings.Repeat("value,", len(r.values)-1)+"value", ",")
}
func (r *maintenanceRows) Close() error { return nil }
func (r *maintenanceRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.values)
	r.done = true
	return nil
}
func (c *maintenanceConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.Contains(q, "total_changes"):
		return &maintenanceRows{values: []driver.Value{s.changes}}, nil
	case strings.Contains(q, "freelist_count"):
		return &maintenanceRows{values: []driver.Value{s.free}}, nil
	case strings.Contains(q, "wal_checkpoint"):
		s.checkpoints++
		return &maintenanceRows{values: []driver.Value{s.busy, int64(0), int64(0)}}, nil
	default:
		return nil, fmt.Errorf("unexpected query %s", q)
	}
}
func (c *maintenanceConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	s := c.state
	s.mu.Lock()
	if strings.Contains(q, "INSERT INTO ex_fts") {
		s.ranks = append(s.ranks, args[0].Value.(int64))
		if s.block {
			s.mu.Unlock()
			s.once.Do(func() { close(s.entered) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		work := int64(1)
		if len(s.work) > 0 {
			work, s.work = s.work[0], s.work[1:]
		}
		s.changes += work
	} else if strings.Contains(q, "incremental_vacuum") {
		s.free = max(0, s.free-500)
	} else {
		s.mu.Unlock()
		return nil, fmt.Errorf("unexpected exec %s", q)
	}
	s.mu.Unlock()
	return driver.RowsAffected(1), nil
}

var maintenanceDriverID atomic.Int64

func openMaintenanceStub(t *testing.T, state *maintenanceStub) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("traffic-maintenance-%d", maintenanceDriverID.Add(1))
	sql.Register(name, maintenanceDriver{state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func plentyOfSpace(string) (uint64, error) { return 1 << 40, nil }

func TestMergeForcesOnlyFirstChunkAndDetectsNoop(t *testing.T) {
	state := &maintenanceStub{work: []int64{10, 10, 1}}
	db := openMaintenanceStub(t, state)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tr := &Traffic{}
	for i, want := range []bool{true, true, false} {
		progress, err := tr.mergeIndex(context.Background(), conn)
		if err != nil || progress != want || tr.mergeStarted != want {
			t.Fatalf("step %d: progress=%v started=%v err=%v", i, progress, tr.mergeStarted, err)
		}
	}
	if !reflect.DeepEqual(state.ranks, []int64{-reclaimMergePages, reclaimMergePages, reclaimMergePages}) {
		t.Fatalf("merge restarted: ranks=%v", state.ranks)
	}
}

func TestCloseCancelsExecutingReclaimSQL(t *testing.T) {
	state := &maintenanceStub{block: true, entered: make(chan struct{})}
	tr := &Traffic{db: openMaintenanceStub(t, state), dir: t.TempDir(), closed: make(chan struct{}), fts: true, freeSpace: plentyOfSpace}
	tr.reclaim()
	select {
	case <-state.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("merge did not enter")
	}
	done := make(chan error, 1)
	go func() { done <- tr.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Close did not cancel the executing SQL")
	}
	if tr.reclaiming.Load() {
		t.Fatal("maintenance goroutine survived shutdown")
	}
}

func TestReclaimCheckpointsEveryChunkAndStopsForPinnedReaders(t *testing.T) {
	for _, busy := range []int64{0, 1} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			state := &maintenanceStub{free: 1500, busy: busy}
			tr := &Traffic{db: openMaintenanceStub(t, state), dir: t.TempDir(), closed: make(chan struct{}), incrementalVacuum: true, freeSpace: plentyOfSpace}
			tr.reclaim()
			tr.reaping.Wait()
			defer tr.Close()
			if busy == 0 && (state.free != 0 || state.checkpoints < 3) {
				t.Fatalf("WAL only drained after entire pass: free=%d checkpoints=%d", state.free, state.checkpoints)
			}
			if busy == 1 && state.free != 1000 {
				t.Fatalf("kept writing behind pinned WAL: free=%d", state.free)
			}
		})
	}
}

func TestLowDiskReclaimDoesNotWrite(t *testing.T) {
	state := &maintenanceStub{}
	tr := &Traffic{db: openMaintenanceStub(t, state), dir: t.TempDir(), closed: make(chan struct{}), fts: true,
		freeSpace: func(string) (uint64, error) { return 0, nil }}
	tr.reclaim()
	tr.reaping.Wait()
	defer tr.Close()
	if len(state.ranks) != 0 || state.checkpoints != 0 {
		t.Fatal("maintenance wrote after low-disk guard")
	}
}

func TestLowDiskDeleteIsAtomic(t *testing.T) {
	tr, _ := openTraffic(t)
	bulkRecord(tr, "low-space.example", 2, 1024)
	tr.freeSpace = func(string) (uint64, error) { return 0, nil }
	if n, err := tr.DeleteHostsExact([]string{"low-space.example"}); n != 0 || err == nil || !strings.Contains(err.Error(), "空间不足") {
		t.Fatalf("delete=(%d,%v)", n, err)
	}
	if n, err := tr.Count(); n != 2 || err != nil {
		t.Fatalf("partial deletion: count=%d err=%v", n, err)
	}
}

func TestCancelledDeleteDoesNotRemoveRows(t *testing.T) {
	tr, _ := openTraffic(t)
	bulkRecord(tr, "cancelled.example", 2, 1024)
	tx, err := tr.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = tr.deleteWhereContext(ctx, tx, "host=?", "cancelled.example")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n, err := tr.Count(); n != 2 || err != nil {
		t.Fatalf("partial deletion: count=%d err=%v", n, err)
	}
}

func TestFTSDeleteDefersImplicitMergesAndRestoresSettings(t *testing.T) {
	tr, _ := openTraffic(t)
	if !tr.fts {
		t.Skip("FTS5 unavailable")
	}
	for _, q := range []string{
		`INSERT INTO ex_fts(ex_fts,rank) VALUES('automerge',0)`,
		`INSERT INTO ex_fts(ex_fts,rank) VALUES('crisismerge',1000)`,
	} {
		if _, err := tr.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Eight independent segments. Half the rows in EACH segment are deleted,
	// making automatic tombstone merges eligible even on an otherwise quiet DB.
	for segment := 0; segment < 8; segment++ {
		tx, err := tr.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 24; i++ {
			host := "kept.example"
			if i%2 == 0 {
				host = "deleted.example"
			}
			res, err := tx.Exec(`INSERT INTO exchanges(id,host) VALUES(?,?)`, fmt.Sprintf("%d-%d", segment, i), host)
			if err != nil {
				t.Fatal(err)
			}
			rowid, _ := res.LastInsertId()
			if _, err := tx.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, rowid, strings.Repeat(fmt.Sprintf("sharedmarker segment%d document%d ", segment, i), 120)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO ex_fts(ex_fts,rank) VALUES('automerge',2)`,
		`INSERT INTO ex_fts(ex_fts,rank) VALUES('crisismerge',2)`,
		`INSERT INTO ex_fts(ex_fts,rank) VALUES('deletemerge',1)`,
	} {
		if _, err := tr.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var before, legacyAfter, after int
	if err := tr.DB().QueryRow(`SELECT count(DISTINCT segid) FROM ex_fts_idx`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	legacy, err := tr.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DELETE FROM ex_fts WHERE rowid IN (SELECT rowid FROM exchanges WHERE host=?)`, "deleted.example"); err != nil {
		t.Fatal(err)
	}
	// Force the pending tombstone flush that also occurs at transaction sync
	// (or mid-statement when the in-memory buffer fills on a large deletion).
	if _, err := legacy.Exec(`INSERT INTO ex_fts(ex_fts,rank) VALUES('merge',0)`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.QueryRow(`SELECT count(DISTINCT segid) FROM ex_fts_idx`).Scan(&legacyAfter); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Rollback(); err != nil {
		t.Fatal(err)
	}
	if legacyAfter >= before {
		t.Fatalf("fixture did not trigger old implicit merge: %d -> %d", before, legacyAfter)
	}
	stage, err := tr.StageDeleteHostsExact([]string{"deleted.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Rollback()
	if err := stage.tx.QueryRow(`SELECT count(DISTINCT segid) FROM ex_fts_idx`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("DELETE rewrote segments: %d -> %d", before, after)
	}
	for key, want := range map[string]int{"automerge": 2, "crisismerge": 2, "deletemerge": 1} {
		var got int
		if err := stage.tx.QueryRow(`SELECT v FROM ex_fts_config WHERE k=?`, key).Scan(&got); err != nil || got != want {
			t.Fatalf("%s not restored: %d, %v", key, got, err)
		}
	}
	t.Logf("legacy DELETE merged %d -> %d segments; bounded path retained %d", before, legacyAfter, after)
	// Commit must not postpone the same unbounded automatic merge until after
	// settings restoration. Keep our background reclaimer out of this assertion.
	tr.reclaiming.Store(true)
	if err := stage.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tr.DB().QueryRow(`SELECT count(DISTINCT segid) FROM ex_fts_idx`).Scan(&after); err != nil || after != before {
		t.Fatalf("commit performed an implicit merge: %d -> %d, %v", before, after, err)
	}
	tr.reclaiming.Store(false)
	tr.reclaim()
	tr.reaping.Wait()
	var matches int
	if err := tr.DB().QueryRow(`SELECT count(*) FROM ex_fts WHERE ex_fts MATCH 'sharedmarker'`).Scan(&matches); err != nil || matches != 96 {
		t.Fatalf("retained search results=%d err=%v", matches, err)
	}
}

func TestInterruptedWriteAutoRollbackRestoresDeletion(t *testing.T) {
	tr, _ := openTraffic(t)
	bulkRecord(tr, "interrupted.example", 2, 1024)
	if _, err := tr.DB().Exec(`CREATE TABLE abort_probe(n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	stage, err := tr.StageDeleteHostsExact([]string{"interrupted.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = stage.tx.ExecContext(ctx, `INSERT INTO abort_probe WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<100000000) SELECT x FROM n`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write was not interrupted: %v", err)
	}
	if err := stage.Rollback(); err != nil {
		t.Fatalf("automatic rollback reported as failure: %v", err)
	}
	if n, err := tr.Count(); err != nil || n != 2 {
		t.Fatalf("delete survived automatic rollback: count=%d err=%v", n, err)
	}
	var n int
	if err := tr.DB().QueryRow(`SELECT count(*) FROM abort_probe`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cancelled write persisted: count=%d err=%v", n, err)
	}
	if _, err := tr.DeleteHostsExact([]string{"interrupted.example"}); err != nil {
		t.Fatalf("connection not reusable after interruption: %v", err)
	}
}

type rollbackTestError struct {
	message string
	code    int
}

func (e rollbackTestError) Error() string { return e.message }
func (e rollbackTestError) Code() int     { return e.code }

func TestRollbackOnlyIgnoresKnownAlreadyEndedTransaction(t *testing.T) {
	if !rollbackAlreadyComplete(rollbackTestError{"SQL logic error: cannot rollback - no transaction is active (1)", 1}) {
		t.Fatal("automatic rollback not recognized")
	}
	for _, err := range []error{errors.New("cannot rollback - no transaction is active"), rollbackTestError{"disk I/O error", 10}, rollbackTestError{"some other SQL logic error", 1}} {
		if rollbackAlreadyComplete(err) {
			t.Fatalf("real rollback error hidden: %v", err)
		}
	}
}
