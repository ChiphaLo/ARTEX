package server

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func excludeFixtureRequest(s *Server, task string, conv, seq int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/exploration/activity/x/exclude-context?task="+task, nil)
	r.SetPathValue("seq", fmt.Sprint(seq))
	if conv > 0 {
		r.SetPathValue("id", fmt.Sprint(conv))
	}
	w := httptest.NewRecorder()
	s.excludeContext(w, r)
	return w
}

func TestContextExclusionTaskScopeToolPairAndAudit(t *testing.T) {
	s, task := mainBatchFixture(t, &mainBatchProvider{})
	store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	w := store.NewWriter(fmt.Sprintf("exp%d-main", task.ExpID), "")
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "same-call", Name: "fixture", Input: []byte("{}")}}}, llm.Usage{InputTokens: 9})
	w.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("same-call", "output", false)}}, llm.Usage{})
	var selected int64
	for seg := range 2 {
		for _, kind := range []string{"tool_use", "tool_result"} {
			seq, err := task.Store.AppendActivity(db.Activity{Worker: "mainagent", MainSeg: &seg, Kind: kind, ToolUseID: "same-call", Detail: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if seg == 0 && kind == "tool_use" {
				selected = seq
			}
		}
	}
	other, err := s.m.CreateTask("other scope", "fixture", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.DeleteTask(other.ID, DeleteTaskOptions{}) })
	if got := excludeFixtureRequest(s, other.ID, 0, selected); got.Code != 404 {
		t.Fatal("cross-task selection accepted", got.Code, got.Body.String())
	}
	if got := excludeFixtureRequest(s, task.ID, 0, selected); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var removed, untouched int
	_ = s.m.pg.QueryRow(`SELECT count(*) FILTER (WHERE COALESCE(main_seg,0)=0 AND metadata->>'context_excluded'='true'),count(*) FILTER (WHERE main_seg=1 AND NOT metadata ? 'context_excluded') FROM activity WHERE exploration_id=$1`, task.ExpID).Scan(&removed, &untouched)
	if removed != 2 || untouched != 2 {
		t.Fatalf("wrong segment marked: removed=%d untouched=%d", removed, untouched)
	}
	recs, err := store.Load(store.MainPath(fmt.Sprintf("exp%d-main", task.ExpID)))
	msgs, usage := transcript.Messages(recs)
	if err != nil || len(recs) != 3 || len(msgs) != 0 || usage.InputTokens != 9 {
		t.Fatal("audit or usage lost", len(recs), msgs, usage, err)
	}
}

func TestContextExclusionConversationAndWrappedUser(t *testing.T) {
	s, task := mainBatchFixture(t, &mainBatchProvider{})
	store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	conv, err := s.m.pg.CreateConversation("mainagent", "context fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(conv.ID) })
	w := store.NewWriter(s.convBusyKey(conv.ID), "")
	w.RecordMessage(llm.UserText("plain input"), llm.Usage{})
	seq, _ := s.m.pg.AppendConvActivity(conv.ID, db.Activity{Kind: "user", Summary: "plain input"})
	if got := excludeFixtureRequest(s, task.ID, conv.ID, seq); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	for _, steer := range []bool{false, true} {
		text := fmt.Sprintf("planner fixture steer=%t", steer)
		atts := []chatAttachment{{Path: "uploads/fixture.txt", Size: 3}}
		activity := userActivityWithAttachments("planner", text, atts)
		seq, _ := task.Store.AppendActivity(activity)
		message := composeAgentMessage(text, atts, filepath.Join(s.m.dir, "tasks", task.ID))
		if steer {
			message = liveSteerPrefix + message
		} else {
			message = agent.PlannerHumanPrefix + message + agent.PlannerSituationMarker + "synthetic graph"
		}
		pw := store.NewWriter(fmt.Sprintf("exp%d-planner", task.ExpID), "")
		pw.RecordMessage(llm.UserText(message), llm.Usage{})
		if got := excludeFixtureRequest(s, task.ID, 0, seq); got.Code != 200 {
			t.Fatal(got.Code, got.Body.String())
		}
	}
}

func TestContextExclusionRejectsUnpersistedThinking(t *testing.T) {
	s, task := mainBatchFixture(t, &mainBatchProvider{})
	seq, _ := task.Store.AppendActivity(db.Activity{Worker: "mainagent", Kind: "thinking", Metadata: []byte(`{"stream_id":"pending","stream_delta":"still streaming"}`)})
	if got := excludeFixtureRequest(s, task.ID, 0, seq); got.Code != 409 {
		t.Fatal("unfinished block accepted", got.Code)
	}
}
