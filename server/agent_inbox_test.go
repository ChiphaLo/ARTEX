package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"iter"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type inboxProvider struct {
	mu          sync.Mutex
	messages    []string
	imageCounts []int
	backgrounds []string
	started     chan context.Context
	release     chan struct{}
}

func (p *inboxProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	msg := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser && req.Messages[i].Text() != "" {
			msg = req.Messages[i].Text()
			break
		}
	}
	bg := intercept.CurrentReviewBackground(ctx)
	p.mu.Lock()
	p.messages = append(p.messages, msg)
	count := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockImage && b.Source != nil {
				count++
			}
		}
	}
	p.imageCounts = append(p.imageCounts, count)
	p.backgrounds = append(p.backgrounds, bg.Text)
	first := len(p.messages) == 1
	p.mu.Unlock()
	if first {
		p.started <- ctx
		select {
		case <-p.release:
		case <-ctx.Done():
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture finished")}}, "end_turn", llm.Usage{}, nil
}
func (p *inboxProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		m, stop, u, err := p.Complete(ctx, req)
		if err != nil {
			y(llm.StreamEvent{}, err)
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
func inboxFixture(t *testing.T) (*Server, *Task, *inboxProvider) {
	s, task := mainBatchFixture(t, &mainBatchProvider{})
	p := &inboxProvider{started: make(chan context.Context, 1), release: make(chan struct{})}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	s.taskAgents[task.ID].main = agent.NewMainAgent(p, "fixture", s.m.dir, tx, 64000, 12)
	s.chatAgent = agent.NewChatAgent(p, "fixture", s.m.dir, tx, 64000)
	s.inboxWake = make(chan struct{}, 1)
	s.inboxDispatching = map[string]bool{}
	return s, task, p
}
func inboxRequest(t *testing.T, s *Server, task *Task, convID int64, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/chat?task="+task.ID, strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	if convID > 0 {
		req.SetPathValue("id", fmt.Sprint(convID))
		s.pgSendConversationMessage(w, req)
	} else {
		s.chat(w, req)
	}
	return w
}
func inboxAPIRequest(s *Server, task *Task, convID int64, method, mid string, body any) *httptest.ResponseRecorder {
	url := "/api/tasks/" + task.ID + "/chat/inbox?seg=0"
	id := task.ID
	if convID > 0 {
		id = fmt.Sprint(convID)
		url = "/api/conversations/" + id + "/inbox"
	}
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, url, strings.NewReader(string(b)))
	r.SetPathValue("id", id)
	r.SetPathValue("mid", mid)
	w := httptest.NewRecorder()
	s.agentInboxAPI(w, r)
	return w
}
func TestAgentInboxMainAndConversationSteerQueueEditReorder(t *testing.T) {
	for _, conv := range []bool{false, true} {
		t.Run(fmt.Sprint(conv), func(t *testing.T) {
			s, task, p := inboxFixture(t)
			convID := int64(0)
			scope := mainInboxScope(task.ID, 0)
			if conv {
				c, err := s.m.pg.CreateConversation("mainagent", "fixture", nil)
				if err != nil {
					t.Fatal(err)
				}
				convID = c.ID
				scope = s.convBusyKey(convID)
				t.Cleanup(func() { s.cancelConversation(convID); s.m.pg.DeleteConversation(convID) })
			}
			if w := inboxRequest(t, s, task, convID, map[string]any{"message": "initial"}); w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
			var runCtx context.Context
			select {
			case runCtx = <-p.started:
			case <-time.After(5 * time.Second):
				t.Fatal("initial run did not start")
			}
			enqueue := func(mode, key, msg string) int64 {
				t.Helper()
				w := inboxRequest(t, s, task, convID, map[string]any{"message": msg, "mode": mode, "request_id": key, "seg": 0})
				if w.Code != 202 {
					t.Fatal(w.Code, w.Body.String())
				}
				var result struct {
					ID int64 `json:"id"`
				}
				json.Unmarshal(w.Body.Bytes(), &result)
				return result.ID
			}
			a := enqueue("queue", "a", "first")
			b := enqueue("queue", "b", "second")
			c := enqueue("queue", "c", "delete me")
			enqueue("steer", "s", "steer human")
			if w := inboxAPIRequest(s, task, convID, "PATCH", fmt.Sprint(a), map[string]any{"message": "edited first", "revision": 1}); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if w := inboxAPIRequest(s, task, convID, "DELETE", fmt.Sprint(c), nil); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if w := inboxAPIRequest(s, task, convID, "PUT", "", map[string]any{"ids": []int64{b, a}, "previous": []int64{a, b}}); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			s.pumpAgentInbox()
			if runCtx.Err() != nil {
				t.Fatal("message interrupted live run")
			}
			p.mu.Lock()
			if len(p.messages) != 1 {
				t.Fatal("queue ran concurrently")
			}
			p.mu.Unlock()
			// Different main segment must not leak its live Steer into this run.
			if !conv {
				w := inboxRequest(t, s, task, 0, map[string]any{"message": "other segment", "mode": "steer", "request_id": "other", "seg": 1})
				if w.Code != 202 {
					t.Fatal(w.Code)
				}
				items, _ := s.m.pg.AgentInbox(mainInboxScope(task.ID, 1))
				if len(items) != 1 {
					t.Fatal(items)
				}
				s.m.pg.CancelAgentMessage(mainInboxScope(task.ID, 1), items[0].ID)
			}
			go s.runAgentInbox()
			close(p.release)
			mainBatchEventually(t, func() bool { items, _ := s.m.pg.AgentInbox(scope); return len(items) == 0 })
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.messages) != 4 {
				t.Fatal("missing/duplicate calls", p.messages)
			}
			if !strings.Contains(p.messages[1], "steer human") || !strings.Contains(p.messages[2], "second") || !strings.Contains(p.messages[3], "edited first") {
				t.Fatal("wrong steer/queue order", p.messages)
			}
			if p.backgrounds[1] != "steer human" || p.backgrounds[2] != "second" {
				t.Fatal("human approval provenance lost", p.backgrounds)
			}
			var steers int
			table, owner, column := "activity", task.ExpID, "exploration_id"
			if conv {
				table, owner, column = "conversation_activities", convID, "conversation_id"
			}
			query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s=$1 AND kind='user' AND metadata->>'delivery_mode'='steer'`, table, column)
			if err := s.m.pg.QueryRow(query, owner).Scan(&steers); err != nil || steers != 1 {
				t.Fatal("steer history", steers, err)
			}
		})
	}
}
func TestAgentInboxStopPreservesQueueAndCancelsSteer(t *testing.T) {
	for _, conv := range []bool{false, true} {
		t.Run(fmt.Sprint(conv), func(t *testing.T) {
			s, task, p := inboxFixture(t)
			convID := int64(0)
			scope := mainInboxScope(task.ID, 0)
			if conv {
				c, err := s.m.pg.CreateConversation("mainagent", "stop fixture", nil)
				if err != nil {
					t.Fatal(err)
				}
				convID = c.ID
				scope = s.convBusyKey(convID)
				t.Cleanup(func() { s.cancelConversation(convID); s.m.pg.DeleteConversation(convID) })
			}
			if w := inboxRequest(t, s, task, convID, map[string]any{"message": "initial"}); w.Code != 202 {
				t.Fatal(w.Code)
			}
			select {
			case <-p.started:
			case <-time.After(5 * time.Second):
				t.Fatal("not started")
			}
			for _, mode := range []string{"queue", "steer"} {
				if w := inboxRequest(t, s, task, convID, map[string]any{"message": mode, "mode": mode, "request_id": mode}); w.Code != 202 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			r := httptest.NewRequest("POST", "/stop", nil)
			r.SetPathValue("id", task.ID)
			w := httptest.NewRecorder()
			if conv {
				r.SetPathValue("id", fmt.Sprint(convID))
				s.pgStopConversation(w, r)
			} else {
				s.stopChat(w, r)
			}
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			mainBatchEventually(t, func() bool {
				s.chatMu.Lock()
				defer s.chatMu.Unlock()
				key := task.ID
				if conv {
					key = scope
				}
				return !s.chatBusy[key]
			})
			items, _ := s.m.pg.AgentInbox(scope)
			if len(items) != 1 || items[0].Mode != "queue" {
				t.Fatal("stop changed queue", items)
			}
			s.pumpAgentInbox()
			mainBatchEventually(t, func() bool { items, _ := s.m.pg.AgentInbox(scope); return len(items) == 0 })
		})
	}
}
func TestAgentInboxManualQueueWinsWorkerAutoReportAndPauseDelete(t *testing.T) {
	s, task, _ := inboxFixture(t)
	batch, err := task.Store.BeginMainWorkerBatch(0)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := task.Store.AddIntent(map[string]any{"summary": "synthetic", "main_batch_id": batch}, 5, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	task.Store.SetIntentState(intent, "done")
	task.Store.SealMainWorkerBatch(batch, false)
	w := inboxRequest(t, s, task, 0, map[string]any{"message": "queued human", "mode": "queue", "request_id": "priority"})
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	s.pumpMainWorkerBatches()
	var state string
	s.m.pg.QueryRow(`SELECT state FROM main_worker_batches WHERE id=$1`, batch).Scan(&state)
	if state != "waiting" {
		t.Fatal("auto overtook human", state)
	}
	s.m.SetTaskPaused(task.ID, true)
	s.pumpAgentInbox()
	items, _ := s.m.pg.AgentInbox(mainInboxScope(task.ID, 0))
	if len(items) != 1 || items[0].State != "pending" {
		t.Fatal("paused task started", items)
	}
	// Existing deletion barrier is also honoured by new delivery modes.
	s.engine.BeginDelete(task.ID)
	w = inboxRequest(t, s, task, 0, map[string]any{"message": "during deletion", "mode": "queue", "request_id": "deleted"})
	if w.Code != 409 {
		t.Fatal("delete admission", w.Code)
	}
}
func TestAgentInboxAdmissionValidationAndRunningReadOnly(t *testing.T) {
	s, task, p := inboxFixture(t)
	for _, body := range []map[string]any{
		{"message": "test", "mode": "queue", "request_id": "bad id"},
		{"message": "test", "mode": "invalid", "request_id": "valid"},
		{"message": "", "mode": "steer", "request_id": "empty"},
	} {
		w := inboxRequest(t, s, task, 0, body)
		if w.Code != 400 {
			t.Fatal("validation accepted", w.Code)
		}
	}
	w := inboxRequest(t, s, task, 0, map[string]any{"message": "initial", "mode": "queue", "request_id": "initial"})
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	s.pumpAgentInbox()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued run not started")
	}
	items, _ := s.m.pg.AgentInbox(mainInboxScope(task.ID, 0))
	if len(items) != 1 || items[0].State != "running" {
		t.Fatal(items)
	}
	if w := inboxAPIRequest(s, task, 0, "PATCH", fmt.Sprint(items[0].ID), map[string]any{"message": "change running", "revision": 1}); w.Code != 409 {
		t.Fatal("edit running accepted", w.Code)
	}
	if w := inboxAPIRequest(s, task, 0, "DELETE", fmt.Sprint(items[0].ID), nil); w.Code != 409 {
		t.Fatal("delete running accepted", w.Code)
	}
	if w := inboxRequest(t, s, task, 0, map[string]any{"message": "legacy"}); w.Code != 409 {
		t.Fatal("legacy busy behavior changed", w.Code)
	}
	close(p.release)
	mainBatchEventually(t, func() bool { items, _ := s.m.pg.AgentInbox(mainInboxScope(task.ID, 0)); return len(items) == 0 })
}
