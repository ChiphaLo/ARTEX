package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestTaskModelsRuntimeResolutionRecordingAndSideQuestion(t *testing.T) {
	if _, _, err := db.DSN(); err != nil {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	wasRecording := m.LLMRecordEnabled()
	if err := m.SetLLMRecordEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.SetLLMRecordEnabled(wasRecording) })
	var wireModels []string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body struct {
			Model  string
			Stream bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		wireModels = append(wireModels, body.Model)
		if fail {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"fixture-key: model error request-id-123"}`)
			return
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3}}}\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":11,"output_tokens":3}}`)
		}
	}))
	defer srv.Close()
	pid, err := m.pg.SaveProfile(&db.LLMProfile{Name: fmt.Sprintf("task-model-%d", time.Now().UnixNano()), Format: "openai-responses", Model: "default", Models: []string{"alpha", "beta"}, APIKey: "fixture-key", BaseURL: srv.URL, Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(pid) })
	s := newAdmissionTestServer(m, nil)
	s.provByProfile = map[int64]*provEntry{}
	create := func(model string) *Task {
		t.Helper()
		task, err := m.CreateTaskWithOptions("synthetic", "verify model isolation", db.TaskCreateOptions{LLMProfileIDs: []int64{pid}, LLMModels: map[int64]string{pid: model}})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.SetTaskPaused(task.ID, true); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = m.pg.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, task.ID)
			_, _ = m.pg.Exec(`DELETE FROM llm_records WHERE task_id=$1`, task.ID)
			_, _ = m.DeleteTask(task.ID, DeleteTaskOptions{})
		})
		return task
	}
	a, b := create("alpha"), create("beta")
	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}}
	var snapshot sidequestion.Snapshot
	for _, fixture := range []struct {
		task  *Task
		model string
	}{{a, "alpha"}, {b, "beta"}} {
		runtime := &taskLLMRuntime{s: s, taskID: fixture.task.ID, agentKey: "worker"}
		sel, err := runtime.current()
		if err != nil {
			t.Fatal(err)
		}
		cfg, ok := runtime.activeCfg()
		if !ok || cfg.Model != fixture.model {
			t.Fatalf("runtime config=%+v", cfg)
		}
		resolved, err := s.resolveTaskRoleLLM(fixture.task, "worker")
		if err != nil || !resolved.Available || resolved.Model != fixture.model || resolved.Source != "task_chain" {
			t.Fatalf("resolution=%+v err=%v", resolved, err)
		}
		ctx := transcript.WithSessionID(llmrec.WithTaskID(t.Context(), fixture.task.ID), "exp0-worker-fixture")
		ctx = sidequestion.WithPublisher(ctx, func(snap sidequestion.Snapshot) { snapshot = snap })
		ctx, deps := sidequestion.Attach(ctx, sidequestion.Parent{}, harness.QueryDeps{}, sel.provider)
		if _, _, _, err := deps.CallModelSync(ctx, req); err != nil {
			t.Fatal(err)
		}
		if _, err := collectTaskLLMStream(deps.CallModel(ctx, req)); err != nil {
			t.Fatal(err)
		}
		// Each successful call captures both request and final answer. Two calls
		// must produce four checkpoints, not nested bindings/double captures.
		if snapshot.Model.Model != fixture.model || !snapshot.Model.Pinned || snapshot.Version != 4 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
		usage, err := m.pg.TokenByModel(fixture.task.ID)
		if err != nil || len(usage) != 1 || usage[0].Model != fixture.model || usage[0].InputTokens != 22 || usage[0].OutputTokens != 6 {
			t.Fatalf("usage=%+v err=%v", usage, err)
		}
		records, total, err := m.pg.ListLLMRecords("", "", fixture.task.ID, 0, 10)
		if err != nil || total != 2 {
			t.Fatalf("record count=%d err=%v", total, err)
		}
		for _, record := range records {
			detail, err := m.pg.GetLLMRecord(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Model != fixture.model || detail == nil || !strings.Contains(detail.RawRequest, fixture.model) {
				t.Fatalf("record model attribution=%+v", record)
			}
		}
	}
	if fmt.Sprint(wireModels) != "[alpha alpha beta beta]" {
		t.Fatalf("wire=%v", wireModels)
	}
	// The default cached provider is unchanged. Model variants retain redaction.
	base, cfg, ok := s.providerForProfile(pid)
	if !ok || cfg.Model != "default" || base == nil {
		t.Fatal("default provider mutated")
	}
	side, err := s.sideProvider(snapshot.Model)
	if err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, _, _, err := side.Complete(t.Context(), req); err == nil || strings.Contains(err.Error(), "fixture-key") || !strings.Contains(err.Error(), "request-id-123") {
		t.Fatalf("variant lost diagnostic redaction: %v", err)
	}
	fail = false
	// Explicitly choosing the current default is still a pin, not inheritance.
	pinnedDefault, defaultCfg, ok := s.providerForProfileModel(pid, "default")
	if !ok {
		t.Fatal("default model pin unavailable")
	}
	var defaultSnapshot sidequestion.Snapshot
	ctx := sidequestion.WithPublisher(t.Context(), func(snap sidequestion.Snapshot) { defaultSnapshot = snap })
	ctx, deps := sidequestion.Attach(ctx, sidequestion.Parent{}, harness.QueryDeps{}, pinnedDefault)
	if _, _, _, err := deps.CallModelSync(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !defaultSnapshot.Model.Pinned {
		t.Fatal("explicit default pin lost its identity")
	}
	inheritedSnapshot := sideModel(defaultCfg, pid, snapshot.Model.Name)
	// Editing only the default must not invalidate a checkpoint for a listed pin.
	p, _ := m.pg.ProfileByID(pid)
	p.Model = "alpha"
	if _, err := m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	s.provByProfile = map[int64]*provEntry{}
	if _, err := s.sideProvider(snapshot.Model); err != nil {
		t.Fatalf("default edit invalidated pinned checkpoint: %v", err)
	}
	if _, err := s.sideProvider(defaultSnapshot.Model); err != nil {
		t.Fatalf("explicit default pin followed new default: %v", err)
	}
	if _, err := s.sideProvider(inheritedSnapshot); err == nil {
		t.Fatal("inherited snapshot ignored changed default")
	}
	p.Models = []string{"alpha"}
	if _, err := m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sideProvider(snapshot.Model); err == nil {
		t.Fatal("removed snapshot model accepted")
	}
	if _, err := (&taskLLMRuntime{s: s, taskID: b.ID, agentKey: "worker"}).current(); err == nil {
		t.Fatal("removed task model silently fell back")
	}
	resolved, err := s.resolveTaskRoleLLM(b, "worker")
	if err != nil || resolved.Available || resolved.Model != "beta" {
		t.Fatalf("removed model resolution=%+v %v", resolved, err)
	}
}

func TestTaskModelsUpdateAPIAndAgentBinding(t *testing.T) {
	if _, _, err := db.DSN(); err != nil {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	pid, err := m.pg.SaveProfile(&db.LLMProfile{Name: fmt.Sprintf("model-edit-%d", time.Now().UnixNano()), Format: "openai-responses", Model: "default", Models: []string{"alpha", "beta"}, APIKey: "fixture-key", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(pid) })
	task, err := m.CreateTaskWithOptions("synthetic", "test edits", db.TaskCreateOptions{LLMProfileIDs: []int64{pid}, LLMModels: map[int64]string{pid: "alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	if err := m.SetTaskPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, nil)
	s.provByProfile = map[int64]*provEntry{}
	update := func(models any, include bool) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{"llm_profile_ids": []int64{pid}, "active_llm_profile_id": pid}
		if include {
			body["llm_models"] = models
		}
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPut, "/api/tasks/"+task.ID+"/llm", bytes.NewReader(raw))
		r.SetPathValue("id", task.ID)
		w := httptest.NewRecorder()
		s.updateTaskLLMProfiles(w, r)
		return w
	}
	if w := update(map[int64]string{pid: "unknown"}, true); w.Code != 400 {
		t.Fatalf("invalid edit: %s", w.Body)
	}
	w := update(map[int64]string{pid: "beta"}, true)
	var response struct {
		SwitchEvent struct {
			Metadata llmActivityMetadata `json:"metadata"`
		} `json:"switch_event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	audit := response.SwitchEvent.Metadata.LLMTransition
	if w.Code != 200 || audit.Previous == nil || audit.Next == nil || audit.Previous.Model != "alpha" || audit.Next.Model != "beta" {
		t.Fatalf("model edit/switch audit: %d %s", w.Code, w.Body)
	}
	if taskDTO(task, "paused").LLMModels[pid] != "beta" {
		t.Fatal("DTO lost pin")
	}
	if w := update(nil, false); w.Code != 200 || task.llmStateSnapshot().Models[pid] != "beta" {
		t.Fatal("legacy API erased pin")
	}
	// Existing role binding takes precedence but does not change other roles.
	old, err := m.pg.GetAgentByKey("worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetAgentLLMProfile("worker", &pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.SetAgentLLMProfile("worker", old.LLMProfileID) })
	for _, role := range []string{"worker", "planner"} {
		resolved, err := s.resolveTaskRoleLLM(task, role)
		cfg, ok := (&taskLLMRuntime{s: s, taskID: task.ID, agentKey: role}).activeCfg()
		want := "beta"
		if role == "worker" {
			want = "default"
		}
		if err != nil || resolved.Model != want || !ok || cfg.Model != want {
			t.Fatalf("%s resolved=%+v cfg=%+v err=%v", role, resolved, cfg, err)
		}
	}
	if w := update(map[string]string{}, true); w.Code != 200 || len(task.llmStateSnapshot().Models) != 0 {
		t.Fatal("explicit clear did not restore inheritance")
	}
}
