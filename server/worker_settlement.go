package server

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

const workerSettlementRetryInterval = 5 * time.Second
const workerSettlementWriteTimeout = 5 * time.Second

// pendingWorkerSettlement is created only after the worker has stopped and a
// state write failed. It never owns a model run or any tool execution.
type pendingWorkerSettlement struct {
	mu     sync.Mutex
	task   *Task
	id     int64
	state  string
	reason string
	notify bool
	closed bool
}

func persistWorkerSettlement(ctx context.Context, t *Task, id int64, state, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, workerSettlementWriteTimeout)
	defer cancel()
	var changed bool
	var err error
	if reason == "" {
		changed, err = t.Store.CompareAndSetIntentStateContext(ctx, id, "running", state)
	} else {
		changed, err = t.Store.CompareAndSetIntentBlockedReasonContext(ctx, id, "running", reason)
	}
	if err != nil {
		return err
	}
	if !changed {
		return db.ErrIntentStateConflict
	}
	return nil
}

// settleWorkerState never retries commands or LLM requests. The failed state
// transition is retried with a compare-and-set after database recovery.
func (e *Engine) settleWorkerState(ctx context.Context, t *Task, id int64, state, reason string, notify bool) error {
	err := persistWorkerSettlement(ctx, t, id, state, reason)
	if err == nil || errors.Is(err, db.ErrIntentStateConflict) || e.IsDeleting(t.ID) {
		return err
	}
	p := &pendingWorkerSettlement{task: t, id: id, state: state, reason: reason, notify: notify}
	e.settlementMu.Lock()
	if e.settlementPending == nil {
		e.settlementPending = make(map[int64]*pendingWorkerSettlement)
	}
	if e.settlementPending[id] != nil {
		e.settlementMu.Unlock()
		return err
	}
	e.settlementPending[id] = p
	e.settlementMu.Unlock()
	go e.retryWorkerSettlement(ctx, p)
	return err
}

// Caller holds p.mu. Map removal is conditional so a stale retry cannot remove
// a later settlement for the same intent.
func (e *Engine) closePendingSettlement(p *pendingWorkerSettlement) {
	p.closed = true
	e.settlementMu.Lock()
	if e.settlementPending[p.id] == p {
		delete(e.settlementPending, p.id)
	}
	e.settlementMu.Unlock()
}

func (e *Engine) retryWorkerSettlement(ctx context.Context, p *pendingWorkerSettlement) {
	for {
		if sleepCtx(ctx, workerSettlementRetryInterval) || e.IsDeleting(p.task.ID) {
			p.mu.Lock()
			e.closePendingSettlement(p)
			p.mu.Unlock()
			return
		}
		// Task deletion waits for admitted writers; a retry never races cleanup.
		if !e.beginTaskOperation(p.task.ID) {
			p.mu.Lock()
			e.closePendingSettlement(p)
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			e.decInflight(p.task.ID)
			return
		}
		err := persistWorkerSettlement(ctx, p.task, p.id, p.state, p.reason)
		finished := err == nil || errors.Is(err, db.ErrIntentStateConflict)
		if finished {
			e.closePendingSettlement(p)
		}
		p.mu.Unlock()
		e.decInflight(p.task.ID)
		if finished {
			if err == nil {
				log.Printf("[worker] task %s intent %d state %s recovered after database failure", p.task.ID, p.id, p.state)
				if p.notify {
					p.task.NotifyDone(p.id)
				} else {
					p.task.Notify()
				}
				if e.onIntentSettled != nil {
					e.onIntentSettled()
				}
			}
			return
		}
		log.Printf("[worker] task %s intent %d state recovery still pending: %v", p.task.ID, p.id, err)
	}
}

// Only a known finished worker whose state write failed can use this path.
// Unknown missing handles still return a conflict: they might be in the narrow
// claim/registration window or belong to another server process.
func (e *Engine) pausePendingWorker(ctx context.Context, t *Task, id int64) (bool, error) {
	e.settlementMu.Lock()
	p := e.settlementPending[id]
	e.settlementMu.Unlock()
	if p == nil || p.task != t {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false, nil
	}
	if err := persistWorkerSettlement(ctx, t, id, "paused", ""); err != nil {
		return true, err
	}
	e.closePendingSettlement(p)
	t.Notify()
	return true, nil
}
