package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
)

type inboxPayload struct {
	Message      string           `json:"message"`
	AgentMessage string           `json:"agent_message"`
	Attachments  []chatAttachment `json:"attachments,omitempty"`
}
type inboxDeliveryKey struct{}
type inboxDelivery struct {
	item    db.AgentInboxItem
	payload inboxPayload
	done    chan string
}

func deliveryFrom(ctx context.Context) *inboxDelivery {
	d, _ := ctx.Value(inboxDeliveryKey{}).(*inboxDelivery)
	return d
}
func (s *Server) inboxReserved(key string, ctx context.Context) bool {
	return s.inboxDispatching[key] && deliveryFrom(ctx) == nil
}
func finishInboxRun(ctx context.Context, runErr error) {
	if d := deliveryFrom(ctx); d != nil {
		state := "delivered"
		if ctx.Err() != nil {
			state = "interrupted"
			if errors.Is(context.Cause(ctx), agent.AbortChatStoppedByUser) {
				state = "cancelled"
			}
		} else if runErr != nil {
			state = "failed"
		}
		select {
		case d.done <- state:
		default:
		}
	}
}
func inboxMetadata(a *db.Activity, mode string, id int64) {
	a.Metadata, _ = json.Marshal(map[string]any{"delivery_mode": mode, "inbox_id": id})
}
func mainInboxScope(taskID string, seg int) string { return fmt.Sprintf("main:%s:%d", taskID, seg) }
func validInboxMessage(w http.ResponseWriter, msg string, atts []chatAttachment) bool {
	if (strings.TrimSpace(msg) == "" && len(atts) == 0) || len([]rune(msg)) > 4000 || len(atts) > 32 {
		writeErr(w, 400, "消息不能为空且最多 4000 字，附件最多 32 个")
		return false
	}
	return true
}
func (s *Server) enqueueInbox(w http.ResponseWriter, r *http.Request, scope string, taskID, convID int64, seg int, mode, requestID, msg, expanded string, atts []chatAttachment) {
	if mode != "steer" && mode != "queue" {
		writeErr(w, 400, "mode must be steer or queue")
		return
	}
	if !validWorkerMessageRequestID(requestID) || !validInboxMessage(w, msg, atts) {
		if !validWorkerMessageRequestID(requestID) {
			writeErr(w, 400, "invalid request_id")
		}
		return
	}
	payload, _ := json.Marshal(inboxPayload{Message: msg, AgentMessage: expanded, Attachments: atts})
	// Serialize admission with task deletion/stop. No current run is cancelled.
	s.chatMu.Lock()
	if taskID > 0 && s.engine.IsDeleting(strconv.FormatInt(taskID, 10)) {
		s.chatMu.Unlock()
		writeErr(w, 409, "任务正在删除")
		return
	}
	item, err := s.m.pg.EnqueueAgentMessage(scope, taskID, convID, seg, requestID, mode, payload)
	s.chatMu.Unlock()
	if err != nil {
		inboxError(w, err)
		return
	}
	s.wakeInbox()
	writeJSON(w, 202, map[string]any{"id": item.ID, "mode": item.Mode, "state": item.State})
}
func inboxError(w http.ResponseWriter, err error) {
	code := 500
	if errors.Is(err, db.ErrInboxConflict) {
		code = 409
	}
	if errors.Is(err, db.ErrInboxFull) {
		code = 429
	}
	writeErr(w, code, err.Error())
}

// Invoked only after the current stream AND tool batch complete. Consumed human
// text is recorded by the SDK in the real transcript, never a provider overlay.
func (s *Server) attachLiveInbox(ctx context.Context, scope, worker, baseDir string, emit func(db.Activity)) context.Context {
	var mu sync.Mutex
	background := intercept.CurrentReviewBackground(ctx)
	ctx = intercept.WithReviewBackgroundFunc(ctx, func() intercept.ReviewBackground { mu.Lock(); defer mu.Unlock(); return background })
	return agent.WithLiveMessages(ctx, func(callCtx context.Context) []llm.Message {
		items, err := s.m.pg.DrainAgentSteers(callCtx, scope, sessionRunID(ctx))
		if err != nil {
			if callCtx.Err() == nil {
				log.Printf("[agent-inbox] steer drain: %v", err)
			}
			return nil
		}
		messages := []llm.Message{}
		for _, item := range items {
			var p inboxPayload
			if json.Unmarshal(item.Payload, &p) != nil {
				continue
			}
			a := userActivityWithAttachments(worker, p.Message, p.Attachments)
			inboxMetadata(&a, "steer", item.ID)
			emit(a)
			mu.Lock()
			background = intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: p.Message}
			mu.Unlock()
			messages = append(messages, llm.UserText(liveSteerPrefix+composeAgentMessage(p.AgentMessage, p.Attachments, baseDir)))
		}
		return messages
	})
}

const liveSteerPrefix = "【人类实时 Steer，不打断当前工具；请在本轮继续处理】\n"

func (s *Server) wakeInbox() {
	if s.inboxWake != nil {
		select {
		case s.inboxWake <- struct{}{}:
		default:
		}
	}
}
func (s *Server) runAgentInbox() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	s.wakeInbox()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.inboxWake:
		case <-ticker.C:
		}
		if s.ctx.Err() != nil {
			return
		}
		s.pumpAgentInbox()
	}
}
func (s *Server) pumpAgentInbox() {
	items, err := s.m.pg.PendingAgentMessages()
	if err != nil {
		if s.ctx.Err() == nil {
			log.Printf("[agent-inbox] heads: %v", err)
		}
		return
	}
	for _, item := range items {
		if s.ctx.Err() != nil {
			return
		}
		key := s.convBusyKey(item.ConversationID)
		if item.TaskID > 0 {
			taskKey := strconv.FormatInt(item.TaskID, 10)
			key = taskKey
			if strings.HasPrefix(item.Scope, "planner:") {
				key = plannerBusyKey(taskKey)
				if s.engine.plannerBusy(taskKey) {
					continue
				}
			}
			t, ok := s.m.Task(taskKey)
			if !ok || s.engine.IsDeleting(strconv.FormatInt(item.TaskID, 10)) {
				continue
			}
			life := t.lifecycleSnapshot()
			if strings.HasPrefix(item.Scope, "planner:") && (s.engine.isSettling(t.ID) || !s.taskRuntimeAvailable(t, "planner")) {
				continue
			}
			if life.Paused || life.Queued || s.engine.IsPaused(strconv.FormatInt(item.TaskID, 10)) {
				continue
			}
		}
		if item.ConversationID > 0 {
			c, err := s.m.pg.GetConversation(item.ConversationID)
			if err != nil || c == nil || s.resolveChatAgent(c) == nil {
				continue
			}
		}
		s.chatMu.Lock()
		if item.TaskID > 0 {
			t, ok := s.m.Task(strconv.FormatInt(item.TaskID, 10))
			if !ok || s.engine.IsDeleting(strconv.FormatInt(item.TaskID, 10)) {
				s.chatMu.Unlock()
				continue
			}
			life := t.lifecycleSnapshot()
			if life.Paused || life.Queued || s.engine.IsPaused(strconv.FormatInt(item.TaskID, 10)) {
				s.chatMu.Unlock()
				continue
			}
		}
		if s.chatBusy[key] || s.inboxDispatching[key] || s.ctx.Err() != nil {
			s.chatMu.Unlock()
			continue
		}
		if s.inboxDispatching == nil {
			s.inboxDispatching = map[string]bool{}
		}
		s.inboxDispatching[key] = true
		claimed, err := s.m.pg.ClaimAgentMessage(item.Scope, item.ID)
		if !claimed || err != nil {
			delete(s.inboxDispatching, key)
			s.chatMu.Unlock()
			continue
		}
		// Read after claim: edits accepted before the claim must not be lost.
		var payload []byte
		err = s.m.pg.QueryRow(`SELECT payload FROM agent_inbox WHERE id=$1`, item.ID).Scan(&payload)
		var deliveryCtx context.Context
		var cancel context.CancelCauseFunc
		if err == nil {
			deliveryCtx, cancel = context.WithCancelCause(s.ctx)
			s.chatCancel[key] = cancel
		}
		s.chatMu.Unlock()
		if err != nil {
			s.m.pg.FinishAgentMessage(item.ID, "failed")
			s.releaseInbox(key)
			continue
		}
		item.Payload = payload
		go s.deliverInbox(deliveryCtx, cancel, item, key)
	}
}
func (s *Server) releaseInbox(key string) {
	s.chatMu.Lock()
	delete(s.inboxDispatching, key)
	if !s.chatBusy[key] {
		delete(s.chatCancel, key)
	}
	s.chatMu.Unlock()
	s.wakeInbox()
	s.wakeMainWorkerBatches()
}
func (s *Server) deliverInbox(ctx context.Context, cancel context.CancelCauseFunc, item db.AgentInboxItem, key string) {
	defer cancel(agent.AbortChatTurnFinished)
	defer s.releaseInbox(key)
	var p inboxPayload
	if err := json.Unmarshal(item.Payload, &p); err != nil {
		s.m.pg.FinishAgentMessage(item.ID, "failed")
		return
	}
	delivery := &inboxDelivery{item: item, payload: p, done: make(chan string, 1)}
	body, _ := json.Marshal(map[string]any{"message": p.Message, "attachments": p.Attachments, "seg": item.MainSeg})
	// Reuse the exact human-turn admission/path rather than duplicate its batch,
	// transcript, attachment, profile and authorisation logic. No network request.
	req := httptest.NewRequest(http.MethodPost, "/api/chat?task="+strconv.FormatInt(item.TaskID, 10), bytes.NewReader(body))
	req = req.WithContext(context.WithValue(ctx, inboxDeliveryKey{}, delivery))
	rec := httptest.NewRecorder()
	if strings.HasPrefix(item.Scope, "planner:") {
		req.SetPathValue("id", strconv.FormatInt(item.TaskID, 10))
		s.sendPlannerMessage(rec, req)
	} else if item.TaskID > 0 {
		s.chat(rec, req)
	} else {
		req.SetPathValue("id", strconv.FormatInt(item.ConversationID, 10))
		s.pgSendConversationMessage(rec, req)
	}
	if strings.HasPrefix(item.Scope, "planner:") && rec.Code == 409 && ctx.Err() == nil {
		s.m.pg.ReleaseAgentMessage(item.ID)
		return
	}
	state := "failed"
	if ctx.Err() != nil {
		state = "interrupted"
		if errors.Is(context.Cause(ctx), agent.AbortChatStoppedByUser) {
			state = "cancelled"
		}
	}
	if rec.Code >= 200 && rec.Code < 300 {
		select {
		case state = <-delivery.done:
		case <-s.ctx.Done():
			state = "interrupted"
		}
	}
	if rec.Code >= 400 && ctx.Err() == nil {
		// A configuration/deletion race must not silently eat the human message.
		worker := "mainagent"
		if strings.HasPrefix(item.Scope, "planner:") {
			worker = "planner"
		}
		a := userActivityWithAttachments(worker, p.Message, p.Attachments)
		inboxMetadata(&a, item.Mode, item.ID)
		failure := db.Activity{Worker: worker, Kind: "text", IsError: true, Summary: fmt.Sprintf("（待处理消息 #%d 未能启动，请检查 Agent 配置后重新发送）", item.ID)}
		if item.TaskID > 0 {
			if t, ok := s.m.Task(strconv.FormatInt(item.TaskID, 10)); ok && !s.engine.IsDeleting(strconv.FormatInt(item.TaskID, 10)) {
				seg := item.MainSeg
				if worker == "mainagent" {
					a.MainSeg = &seg
					failure.MainSeg = &seg
				}
				s.engine.emitActivity(t, a)
				s.engine.emitActivity(t, failure)
			}
		} else if c, err := s.m.pg.GetConversation(item.ConversationID); err == nil && c != nil {
			a.Worker = c.AgentKey
			failure.Worker = c.AgentKey
			s.m.pg.AppendConvActivity(c.ID, a)
			s.m.pg.AppendConvActivity(c.ID, failure)
		}
	}
	if err := s.m.pg.FinishAgentMessage(item.ID, state); err != nil {
		log.Printf("[agent-inbox] finish %d: %v", item.ID, err)
	}
}

// Scoped APIs: running messages are read-only; edit/delete/reorder use atomic
// pending predicates. No endpoint can mutate another conversation's inbox.
func (s *Server) agentInboxAPI(w http.ResponseWriter, r *http.Request) {
	scope := ""
	if strings.Contains(r.URL.Path, "/tasks/") {
		t := s.m.ResolveTask(r.PathValue("id"))
		if t == nil {
			writeErr(w, 404, "task not found")
			return
		}
		if s.engine.IsDeleting(t.ID) {
			writeErr(w, 409, "任务正在删除")
			return
		}
		seg, _ := t.Store.CurrentMainSeg()
		if v := r.URL.Query().Get("seg"); v != "" {
			var err error
			seg, err = strconv.Atoi(v)
			if err != nil || seg < 0 {
				writeErr(w, 400, "invalid seg")
				return
			}
		}
		if strings.Contains(r.URL.Path, "/planner/inbox") {
			scope = plannerScope(t.ID)
		} else {
			scope = mainInboxScope(t.ID, seg)
		}
	} else {
		_, c, ok := s.convByID(w, r)
		if !ok {
			return
		}
		scope = s.convBusyKey(c.ID)
	}
	if r.Method == http.MethodGet {
		s.writeInbox(w, scope)
		return
	}
	if r.PathValue("mid") == "" {
		var req struct {
			IDs      []int64 `json:"ids"`
			Previous []int64 `json:"previous"`
		}
		if err := decode(r, &req); err != nil || len(req.IDs) > 32 {
			writeErr(w, 400, "invalid order")
			return
		}
		if err := s.m.pg.ReorderAgentMessages(scope, req.IDs, req.Previous); err != nil {
			inboxError(w, err)
			return
		}
		s.wakeInbox()
		s.writeInbox(w, scope)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid message id")
		return
	}
	if r.Method == http.MethodDelete {
		ok, err := s.m.pg.CancelAgentMessage(scope, id)
		if err != nil {
			inboxError(w, err)
			return
		}
		if !ok {
			inboxError(w, db.ErrInboxConflict)
			return
		}
		s.wakeInbox()
		s.writeInbox(w, scope)
		return
	}
	var req struct {
		Message     string            `json:"message"`
		Revision    int               `json:"revision"`
		Mode        string            `json:"mode,omitempty"`
		Attachments *[]chatAttachment `json:"attachments,omitempty"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "invalid message")
		return
	}
	items, err := s.m.pg.AgentInbox(scope)
	if err != nil {
		inboxError(w, err)
		return
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		var p inboxPayload
		if json.Unmarshal(item.Payload, &p) != nil {
			writeErr(w, 500, "invalid stored message")
			return
		}
		if req.Attachments != nil {
			p.Attachments = *req.Attachments
		}
		if !validInboxMessage(w, req.Message, p.Attachments) {
			return
		}
		expanded, ok := s.prepareChatMentionMessage(w, req.Message)
		if !ok {
			return
		}
		p.Message, p.AgentMessage = req.Message, expanded
		payload, _ := json.Marshal(p)
		mode := req.Mode
		if mode == "" {
			mode = "queue"
		}
		if mode != "queue" && mode != "steer" {
			writeErr(w, 400, "invalid mode")
			return
		}
		if err := s.m.pg.EditAgentMessage(scope, id, req.Revision, payload, mode); err != nil {
			inboxError(w, err)
			return
		}
		s.wakeInbox()
		s.writeInbox(w, scope)
		return
	}
	inboxError(w, db.ErrInboxConflict)
}
func (s *Server) writeInbox(w http.ResponseWriter, scope string) {
	items, err := s.m.pg.AgentInbox(scope)
	if err != nil {
		inboxError(w, err)
		return
	}
	out := []map[string]any{}
	for _, item := range items {
		var p inboxPayload
		_ = json.Unmarshal(item.Payload, &p)
		out = append(out, map[string]any{"id": item.ID, "mode": item.Mode, "state": item.State, "revision": item.Revision, "message": p.Message, "attachments": p.Attachments})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}
