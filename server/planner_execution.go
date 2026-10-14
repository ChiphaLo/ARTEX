package server

import (
	"context"
	"sync"

	"github.com/Autumn-27/artex/agent"
)

type plannerExecution struct{ cancel context.CancelCauseFunc }

func plannerScope(taskID string) string   { return "planner:" + taskID }
func plannerBusyKey(taskID string) string { return "planner-" + taskID }

// Scheduled and human planning share one gate/transcript owner per task.
func (e *Engine) beginPlanner(parent context.Context, taskID string) (context.Context, context.CancelCauseFunc, func(), bool) {
	e.plannerMu.Lock()
	defer e.plannerMu.Unlock()
	if e.plannerRuns[taskID] != nil || parent.Err() != nil || e.IsDeleting(taskID) {
		return nil, nil, nil, false
	}
	if e.plannerRuns == nil {
		e.plannerRuns = map[string]*plannerExecution{}
	}
	ctx, cancel := context.WithCancelCause(parent)
	e.plannerRuns[taskID] = &plannerExecution{cancel: cancel}
	done := sync.OnceFunc(func() {
		cancel(agent.AbortChatTurnFinished)
		e.plannerMu.Lock()
		delete(e.plannerRuns, taskID)
		e.plannerMu.Unlock()
	})
	return ctx, cancel, done, true
}
func (e *Engine) plannerBusy(taskID string) bool {
	e.plannerMu.Lock()
	defer e.plannerMu.Unlock()
	return e.plannerRuns[taskID] != nil
}
func (e *Engine) stopPlanner(taskID string) bool {
	return e.cancelPlanner(taskID, agent.AbortChatStoppedByUser)
}
func (e *Engine) cancelPlanner(taskID string, cause error) bool {
	e.plannerMu.Lock()
	defer e.plannerMu.Unlock()
	run := e.plannerRuns[taskID]
	if run == nil {
		return false
	}
	run.cancel(cause)
	return true
}
