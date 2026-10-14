package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

func (s *Server) plannerHeld(t *Task) bool {
	run, err := s.m.pg.LatestAgentRun(plannerScope(t.ID))
	return err != nil || (run != nil && (run.State == "interrupted" || run.State == "failed"))
}
func (s *Server) runScheduledPlanner(ctx context.Context, t *Task, p *agent.Planner, triggers []agent.TriggerEvent, emit func(db.Activity)) (bool, string, error) {
	key := plannerBusyKey(t.ID)
	scope := plannerScope(t.ID)
	s.chatMu.Lock()
	items, err := s.m.pg.AgentInbox(scope)
	if err != nil || len(items) > 0 || s.inboxDispatching[key] {
		s.chatMu.Unlock()
		restorePlannerTriggers(t, triggers)
		s.wakeInbox()
		return false, "", err
	}
	ctx, err = s.startSessionRun(intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{}), scope, taskNumericID(t.ID), 0, false)
	if err != nil {
		s.chatMu.Unlock()
		restorePlannerTriggers(t, triggers)
		return false, "", err
	}
	s.chatBusy[key] = true
	s.chatMu.Unlock()
	defer func() { s.chatMu.Lock(); delete(s.chatBusy, key); s.chatMu.Unlock(); s.wakeInbox() }()
	ctx = s.attachLiveInbox(ctx, scope, "planner", filepath.Join(s.m.dir, "tasks", t.ID), emit)
	met, reason, err := p.Plan(ctx, taskNumericID(t.ID), s.m.Assets(), t.Store, t.Goal, triggers, emit)
	s.finishSessionRun(ctx, err)
	return met, reason, err
}
func taskNumericID(id string) int64 { n, _ := strconv.ParseInt(id, 10, 64); return n }
func (s *Server) sendPlannerMessage(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, 409, "任务正在删除")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Message     string           `json:"message"`
		Mode        string           `json:"mode"`
		RequestID   string           `json:"request_id"`
		Attachments []chatAttachment `json:"attachments"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "invalid message")
		return
	}
	scope := plannerScope(t.ID)
	key := plannerBusyKey(t.ID)
	message := ""
	control := resumeControl(r.Context())
	if control == nil {
		if !validInboxMessage(w, req.Message, req.Attachments) {
			return
		}
		if d := deliveryFrom(r.Context()); d != nil {
			message = d.payload.AgentMessage
		} else {
			var ok bool
			message, ok = s.prepareChatMentionMessage(w, req.Message)
			if !ok {
				return
			}
		}
		if req.Mode != "" {
			s.enqueueInbox(w, r, scope, taskNumericID(t.ID), 0, 0, req.Mode, req.RequestID, req.Message, message, req.Attachments)
			return
		}
	}
	life := t.lifecycleSnapshot()
	if life.Paused || life.Queued || s.engine.IsPaused(t.ID) || s.engine.isSettling(t.ID) {
		writeErr(w, 409, "任务已暂停、排队或正在收尾，请先恢复任务")
		return
	}
	if !s.taskRuntimeAvailable(t, "planner") {
		writeErr(w, 503, "Planner 模型不可用")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除")
		return
	}
	s.chatMu.Lock()
	if s.chatBusy[key] || s.inboxReserved(key, r.Context()) {
		s.chatMu.Unlock()
		s.engine.decInflight(t.ID)
		writeErr(w, 409, "Planner 正在处理上一轮")
		return
	}
	parent := s.engine.execContextFor(s.ctx, t.ID)
	if isTerminalStatus(life.Status) {
		parent = s.ctx
	}
	parent = s.engine.clockCtx(parent, t, false)
	// A queued reservation may already have been stopped before handler admission.
	if d := deliveryFrom(r.Context()); d != nil {
		if r.Context().Err() != nil {
			s.chatMu.Unlock()
			s.engine.decInflight(t.ID)
			writeErr(w, 409, "待处理回合已取消")
			return
		}
		parent = s.engine.clockCtx(r.Context(), t, false)
	}
	ctx, cancel, release, admitted := s.engine.beginPlanner(parent, t.ID)
	if !admitted {
		s.chatMu.Unlock()
		s.engine.decInflight(t.ID)
		writeErr(w, 409, "Planner 正在运行")
		return
	}
	ctx = intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: req.Message})
	if control != nil {
		ctx = bindResume(ctx, control)
		message = s.continuationInput(scope, t, 0, 0)
	}
	if d := deliveryFrom(r.Context()); d != nil {
		ctx = context.WithValue(ctx, inboxDeliveryKey{}, d)
	}
	ctx, err := s.startSessionRun(ctx, scope, taskNumericID(t.ID), 0, false)
	if err != nil {
		release()
		s.chatMu.Unlock()
		s.engine.decInflight(t.ID)
		sessionRunError(w, err)
		return
	}
	s.chatBusy[key] = true
	s.chatCancel[key] = cancel
	s.chatMu.Unlock()
	activity := userActivityWithAttachments("planner", req.Message, req.Attachments)
	if control != nil {
		activity = resumeActivity("planner", control.ID)
	} else if d := deliveryFrom(ctx); d != nil {
		inboxMetadata(&activity, d.item.Mode, d.item.ID)
	}
	s.engine.emitActivity(t, activity)
	p := s.agentsForTask(t).pl
	go func() {
		var runErr error
		defer func() {
			s.finishSessionRun(ctx, runErr)
			finishInboxRun(ctx, runErr)
			s.chatMu.Lock()
			delete(s.chatBusy, key)
			delete(s.chatCancel, key)
			s.chatMu.Unlock()
			release()
			s.engine.decInflight(t.ID)
			s.wakeInbox()
			wakeDeferredPlannerTriggers(t)
		}()
		emit := func(a db.Activity) { s.engine.emitActivity(t, a) }
		dir := filepath.Join(s.m.dir, "tasks", t.ID)
		ctx = s.attachLiveInbox(ctx, scope, "planner", dir, emit)
		input := composeAgentMessage(message, req.Attachments, dir)
		s.engine.BeginLLMCall(t.ID)
		var met bool
		met, _, runErr = p.PlanWithMessage(ctx, taskNumericID(t.ID), s.m.Assets(), t.Store, t.Goal, input, control != nil, emit)
		s.engine.EndLLMCall(t.ID)
		if met && runErr == nil && ctx.Err() == nil && !isTerminalStatus(life.Status) {
			if won, e := s.m.SetTaskStatusGuarded(t.ID, "done"); e != nil {
				runErr = e
			} else if won {
				s.engine.cancelExec(t.ID, agent.AbortGoalMet)
			}
		}
		if runErr != nil && ctx.Err() == nil {
			emit(db.Activity{Worker: "planner", Kind: "text", IsError: true, Summary: fmt.Sprintf("（Planner 出错：%v）", runErr)})
		}
		// No-goal/completed-task human turns do not revive tasks. Real prove_goal
		// completion of an active task retains normal planner finalization semantics.
	}()
	writeJSON(w, 202, map[string]any{"status": "accepted"})
}
func (s *Server) stopPlanner(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	s.chatMu.Lock()
	err := s.m.pg.CancelAgentSteers(plannerScope(t.ID))
	cancel := s.chatCancel[plannerBusyKey(t.ID)]
	if cancel != nil {
		cancel(agent.AbortChatStoppedByUser)
	}
	s.chatMu.Unlock()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	stopped := s.engine.stopPlanner(t.ID)
	state := "idle"
	if stopped || cancel != nil {
		state = "stopping"
	}
	writeJSON(w, 200, map[string]string{"status": state})
}

// Admission may defer a scheduled round in favour of human Queue messages. Keep
// its concrete event data instead of silently dropping goal/finding triggers.
func restorePlannerTriggers(t *Task, events []agent.TriggerEvent) {
	if len(events) == 0 {
		return
	}
	t.trigMu.Lock()
	t.pendingTriggers = append(events, t.pendingTriggers...)
	t.trigMu.Unlock()
}
func wakeDeferredPlannerTriggers(t *Task) {
	t.trigMu.Lock()
	pending := len(t.pendingTriggers) > 0
	t.trigMu.Unlock()
	if pending {
		t.Notify()
	}
}
