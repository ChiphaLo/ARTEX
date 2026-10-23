package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type interactiveProvider struct {
	mu              sync.Mutex
	requests        []llm.CompletionRequest
	backgrounds     []intercept.ReviewBackground
	blockAt, failAt int
	entered         chan context.Context
	release         chan struct{}
}

func (p *interactiveProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.backgrounds = append(p.backgrounds, intercept.CurrentReviewBackground(ctx))
	n := len(p.requests)
	p.mu.Unlock()
	if n == p.blockAt {
		p.entered <- ctx
		select {
		case <-ctx.Done():
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		case <-p.release:
		}
	}
	if n == p.failAt {
		// Recovery-control fixtures need a terminal, non-retryable failure;
		// transient disconnects are now retried inside the active session.
		return llm.Message{}, "", llm.Usage{}, errors.New("status 400: synthetic invalid request")
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("synthetic response")}}, "end_turn", llm.Usage{}, nil
}
func (p *interactiveProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		m, stop, u, e := p.Complete(ctx, req)
		if e != nil {
			y(llm.StreamEvent{}, e)
			return
		}
		if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: m.Text()}, nil) {
			return
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: u}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}
func interactiveFixture(t *testing.T) (*Server, *Task, *interactiveProvider) {
	s, task, _ := inboxFixture(t)
	p := &interactiveProvider{blockAt: 1, entered: make(chan context.Context, 1), release: make(chan struct{})}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	b := s.taskAgents[task.ID]
	b.main = agent.NewMainAgent(p, "fixture", s.m.dir, tx, 64000, 12)
	b.pl = agent.NewPlanner(p, "fixture", s.m.dir, tx, 64000, 12)
	s.chatAgent = agent.NewChatAgent(p, "fixture", s.m.dir, tx, 64000)
	s.engine.plannerRun = s.runScheduledPlanner
	s.engine.plannerHeld = s.plannerHeld
	t.Cleanup(func() {
		s.engine.stopPlanner(task.ID)
		mainBatchEventually(t, func() bool { return !s.engine.plannerBusy(task.ID) })
	})
	return s, task, p
}
func interactiveRequest(s *Server, t *Task, convID int64, role, action string, body any) *httptest.ResponseRecorder {
	url := "/api/tasks/" + t.ID + "/" + role + "/" + action + "?seg=0"
	id := t.ID
	if convID > 0 {
		url = "/api/conversations/" + fmt.Sprint(convID) + "/" + action
		id = fmt.Sprint(convID)
	}
	payload, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", url, strings.NewReader(string(payload)))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	switch action {
	case "continue":
		s.continueAgentRun(w, r)
	case "run":
		s.agentRunStatus(w, r)
	case "stop":
		if convID > 0 {
			s.pgStopConversation(w, r)
		} else if role == "planner" {
			s.stopPlanner(w, r)
		} else {
			s.stopChat(w, r)
		}
	case "messages":
		s.sendPlannerMessage(w, r)
	}
	return w
}
func waitInteractiveRun(t *testing.T, s *Server, scope, state string) *db.AgentSessionRun {
	t.Helper()
	var run *db.AgentSessionRun
	mainBatchEventually(t, func() bool {
		run, _ = s.m.pg.LatestAgentRun(scope)
		if run == nil || run.State != state {
			return false
		}
		key := scope
		taskID := ""
		if strings.HasPrefix(scope, "main:") {
			taskID = strings.Split(scope, ":")[1]
			key = taskID
		}
		if strings.HasPrefix(scope, "planner:") {
			taskID = strings.TrimPrefix(scope, "planner:")
			key = plannerBusyKey(taskID)
			if s.engine.plannerBusy(taskID) {
				return false
			}
		}
		s.chatMu.Lock()
		busy := s.chatBusy[key] || s.inboxDispatching[key]
		s.chatMu.Unlock()
		return !busy
	})
	return run
}
func TestContinueMainConversationAndPlannerAfterManualStop(t *testing.T) {
	for _, role := range []string{"chat", "conversation", "planner"} {
		t.Run(role, func(t *testing.T) {
			s, task, p := interactiveFixture(t)
			convID := int64(0)
			scope := mainInboxScope(task.ID, 0)
			if role == "conversation" {
				c, e := s.m.pg.CreateConversation("mainagent", "fixture", nil)
				if e != nil {
					t.Fatal(e)
				}
				convID = c.ID
				scope = s.convBusyKey(c.ID)
				t.Cleanup(func() { s.cancelConversation(c.ID); s.m.pg.DeleteConversation(c.ID) })
			} else if role == "planner" {
				scope = plannerScope(task.ID)
			}
			var w *httptest.ResponseRecorder
			if role == "planner" {
				w = interactiveRequest(s, task, 0, role, "messages", map[string]any{"message": "original human task"})
			} else {
				w = inboxRequest(t, s, task, convID, map[string]any{"message": "original human task"})
			}
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
			select {
			case <-p.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("run never started")
			}
			if w := interactiveRequest(s, task, convID, role, "stop", nil); w.Code != 200 {
				t.Fatal(w.Code)
			}
			run := waitInteractiveRun(t, s, scope, "interrupted")
			mainBatchEventually(t, func() bool {
				w := interactiveRequest(s, task, convID, role, "run", nil)
				return strings.Contains(w.Body.String(), `"can_continue":true`)
			})
			if w := interactiveRequest(s, task, convID, role, "continue", map[string]any{"run_id": run.ID}); w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
			next := waitInteractiveRun(t, s, scope, "completed")
			if next.ID == run.ID {
				t.Fatal("did not resume")
			}
			if w := interactiveRequest(s, task, convID, role, "continue", map[string]any{"run_id": run.ID}); w.Code != 409 {
				t.Fatal("stale button accepted", w.Code)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.requests) != 2 {
				t.Fatal("duplicate model calls", len(p.requests))
			}
			resumed := p.requests[1]
			found := false
			for _, m := range resumed.Messages {
				if strings.Contains(m.Text(), "original human task") {
					found = true
				}
			}
			if !found {
				t.Fatal("lost original transcript")
			}
			if p.backgrounds[1].Text != "original human task" || p.backgrounds[1].Source != intercept.BackgroundUserMessage {
				t.Fatal("continue became new authority", p.backgrounds)
			}
			var users int
			table, owner, col := "activity", task.ExpID, "exploration_id"
			worker := "mainagent"
			if role == "planner" {
				worker = "planner"
			}
			if convID > 0 {
				table, owner, col = "conversation_activities", convID, "conversation_id"
			}
			if e := s.m.pg.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s=$1 AND worker=$2 AND kind='user'`, table, col), owner, worker).Scan(&users); e != nil || users != 1 {
				t.Fatal("continue created human message", users, e)
			}
		})
	}
}
func TestPlannerHumanSteerGoallessAndQueueIsolation(t *testing.T) {
	s, task, p := interactiveFixture(t)
	scope := plannerScope(task.ID)
	if open, _ := task.Store.HasOpenGoal(); open {
		t.Fatal("must be goalless")
	}
	w := interactiveRequest(s, task, 0, "planner", "messages", map[string]any{"message": "human planner question"})
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var runCtx context.Context
	select {
	case runCtx = <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("planner did not run")
	}
	for _, mode := range []string{"steer", "queue"} {
		w = interactiveRequest(s, task, 0, "planner", "messages", map[string]any{"message": "planner " + mode, "mode": mode, "request_id": mode})
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// Main stop must not delete Planner steering. No Planner queue delays a Main report.
	s.m.pg.CancelTaskSteers(taskNumericID(task.ID))
	if yes, _ := s.m.pg.HasTaskInbox(taskNumericID(task.ID)); yes {
		t.Fatal("planner inbox blocked main")
	}
	s.pumpAgentInbox()
	if runCtx.Err() != nil {
		t.Fatal("Steer interrupted Planner")
	}
	p.mu.Lock()
	n := len(p.requests)
	p.mu.Unlock()
	if n != 1 {
		t.Fatal("parallel planner runs")
	}
	close(p.release)
	go s.runAgentInbox()
	mainBatchEventually(t, func() bool { items, _ := s.m.pg.AgentInbox(scope); return len(items) == 0 })
	waitInteractiveRun(t, s, scope, "completed")
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 3 {
		t.Fatal("missing/duplicate messages", len(p.requests))
	}
	if p.backgrounds[1].Text != "planner steer" || p.backgrounds[2].Text != "planner queue" {
		t.Fatal("lost steer authorization", p.backgrounds)
	}
	var goals int
	s.m.pg.QueryRow(`SELECT count(*) FROM nodes WHERE exploration_id=$1 AND kind='goal'`, task.ExpID).Scan(&goals)
	if goals != 0 {
		t.Fatal("fabricated goals")
	}
}
func TestContinueProviderFailurePreservesConsumedSteerAndReadonly(t *testing.T) {
	s, task, p := interactiveFixture(t)
	p.failAt = 2
	w := inboxRequest(t, s, task, 0, map[string]any{"message": "original"})
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("not started")
	}
	if w := inboxRequest(t, s, task, 0, map[string]any{"message": "narrowing user steer", "mode": "steer", "request_id": "narrow"}); w.Code != 202 {
		t.Fatal(w.Code)
	}
	close(p.release)
	scope := mainInboxScope(task.ID, 0)
	failed := waitInteractiveRun(t, s, scope, "failed")
	if w := interactiveRequest(s, task, 0, "chat", "continue", map[string]any{"run_id": failed.ID}); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	waitInteractiveRun(t, s, scope, "completed")
	p.mu.Lock()
	if len(p.backgrounds) != 3 || p.backgrounds[2].Text != "narrowing user steer" {
		t.Fatal(p.backgrounds)
	}
	p.mu.Unlock()
	// A control snapshot cannot turn the previous read-only report into Bash or
	// spawning work. Simulate that report's resumable journal using real transcript.
	s.m.pg.Exec(`UPDATE agent_session_runs SET read_only=true,state='interrupted' WHERE scope_key=$1 AND id=(SELECT max(id) FROM agent_session_runs WHERE scope_key=$1)`, scope)
	report, _ := s.m.pg.LatestAgentRun(scope)
	if w := interactiveRequest(s, task, 0, "chat", "continue", map[string]any{"run_id": report.ID}); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	waitInteractiveRun(t, s, scope, "completed")
	p.mu.Lock()
	defer p.mu.Unlock()
	req := p.requests[len(p.requests)-1]
	for _, tool := range req.Tools {
		switch tool.Name {
		case "Bash", "add_intent", "set_goals", "steer_work", "WebFetch", "ExecuteExtraTool":
			t.Fatal("readonly resume escalated", tool.Name)
		}
	}
}
func TestScheduledPlannerAcceptsSteerAndHoldsFailedRoundForContinue(t *testing.T) {
	s, task, p := interactiveFixture(t)
	p.failAt = 2
	if _, err := task.Store.AddGoal(map[string]any{"text": "synthetic goal"}, "human"); err != nil {
		t.Fatal(err)
	}
	s.engine.SetAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return s.taskAgents[task.ID].pl, agent.NewWorker(p, "fixture", s.m.dir, nil, 64000, 12)
	})
	s.engine.debounce = time.Millisecond
	s.engine.Run(s.ctx, task)
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled planner never ran")
	}
	if w := interactiveRequest(s, task, 0, "planner", "messages", map[string]any{"message": "human steer to scheduled planner", "mode": "steer", "request_id": "scheduled"}); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	close(p.release)
	failed := waitInteractiveRun(t, s, plannerScope(task.ID), "failed")
	task.Notify()
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	n := len(p.requests)
	p.mu.Unlock()
	if n != 2 {
		t.Fatal("failed planner auto-replayed", n)
	}
	if w := interactiveRequest(s, task, 0, "planner", "continue", map[string]any{"run_id": failed.ID}); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	waitInteractiveRun(t, s, plannerScope(task.ID), "completed")
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 3 || p.backgrounds[2].Text != "human steer to scheduled planner" {
		t.Fatal("lost scheduled steer/continuation", p.backgrounds)
	}
}
func TestContinueRejectsPausedDeletedAndCrossScope(t *testing.T) {
	s, task, p := interactiveFixture(t)
	p.blockAt = 0
	p.failAt = 1
	if w := inboxRequest(t, s, task, 0, map[string]any{"message": "fixture"}); w.Code != 202 {
		t.Fatal(w.Code)
	}
	failed := waitInteractiveRun(t, s, mainInboxScope(task.ID, 0), "failed")
	if w := interactiveRequest(s, task, 0, "planner", "continue", map[string]any{"run_id": failed.ID}); w.Code != 409 {
		t.Fatal("cross-scope continue", w.Code)
	}
	if err := s.m.SetTaskPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if w := interactiveRequest(s, task, 0, "chat", "continue", map[string]any{"run_id": failed.ID}); w.Code != 409 {
		t.Fatal("paused continue", w.Code)
	}
	s.m.SetTaskPaused(task.ID, false)
	s.engine.BeginDelete(task.ID)
	if w := interactiveRequest(s, task, 0, "chat", "continue", map[string]any{"run_id": failed.ID}); w.Code != 409 {
		t.Fatal("deleted continue", w.Code)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 1 {
		t.Fatal("rejected controls started model")
	}
}
func TestDeferredPlannerRoundPreservesConcreteTriggers(t *testing.T) {
	s, task, _ := interactiveFixture(t)
	if w := interactiveRequest(s, task, 0, "planner", "messages", map[string]any{"message": "queued", "mode": "queue", "request_id": "defer"}); w.Code != 202 {
		t.Fatal(w.Code)
	}
	events := []agent.TriggerEvent{{Kind: "goal", Goals: []string{"synthetic goal"}}}
	if _, _, err := s.runScheduledPlanner(s.ctx, task, s.taskAgents[task.ID].pl, events, func(db.Activity) {}); err != nil {
		t.Fatal(err)
	}
	retained := task.drainTriggers()
	if len(retained) != 1 || retained[0].Kind != "goal" {
		t.Fatal("lost deferred event", retained)
	}
}

func TestPlannerHumanResumeDoesNotReplayIndependentAuditRounds(t *testing.T) {
	s, task, p := interactiveFixture(t)
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	sessionID := fmt.Sprintf("exp%d-planner", task.ExpID)
	for range 106 {
		w := tx.NewWriter(sessionID, "")
		w.RecordMessage(llm.UserText(strings.Repeat("old independent planner audit ", 300)), llm.Usage{})
	}
	w := tx.NewWriter(sessionID, "")
	w.RecordMessage(llm.UserText("latest independent planner round"), llm.Usage{})
	response := interactiveRequest(s, task, 0, "planner", "messages", map[string]any{"message": "continue fixture"})
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("planner not started")
	}
	p.mu.Lock()
	req := p.requests[0]
	p.mu.Unlock()
	for _, m := range req.Messages {
		if strings.Contains(m.Text(), "old independent planner audit") {
			t.Fatal("old rounds injected")
		}
	}
	close(p.release)
	waitInteractiveRun(t, s, plannerScope(task.ID), "completed")
}
