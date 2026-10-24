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
