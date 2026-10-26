package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type uiDiagnosticProvider struct {
	fail     bool
	requests []llm.CompletionRequest
}

const uiDiagnosticErrorFixture = `openai-responses: status 402: {"code":30001,"message":"Sorry, your account balance is insufficient"}`

func (p *uiDiagnosticProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.requests = append(p.requests, req)
	if p.fail {
		return llm.Message{}, "", llm.Usage{}, errors.New(uiDiagnosticErrorFixture)
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("recovered fixture")}}, "end_turn", llm.Usage{}, nil
}

func (p *uiDiagnosticProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		msg, stop, _, err := p.Complete(ctx, req)
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for _, ev := range []llm.StreamEvent{{Type: llm.SETextDelta, Text: msg.Text()}, {Type: llm.SEMessageDelta, StopReason: stop}, {Type: llm.SEMessageStop}} {
			if !y(ev, nil) {
				return
			}
		}
	}
}

func TestUIDiagnosticsCapturePreservesUIAndDoesNotPolluteResume(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming-%v", streaming), func(t *testing.T) {
			p := &uiDiagnosticProvider{fail: true}
			opts := agentcore.Options{Provider: p, NonStreaming: !streaming, DisableBackgroundTasks: true,
				Transcript: transcript.NewStore(filepath.Join(t.TempDir(), "transcripts")), SessionID: "diagnostic-fixture",
				ContextWindow: 10000, MaxTokens: 100}
			s := agentcore.NewSession(opts)
			var result db.Activity
			_, reason, err := captureRunSession(t.Context(), s, "synthetic task", func(a db.Activity) {
				if a.Kind == "result" {
					result = a
				}
			})
			_ = s.Close()
			if err == nil || reason != harness.ReasonModelError || !strings.Contains(result.Detail, uiDiagnosticErrorFixture) {
				t.Fatalf("UI lost raw error: reason=%s err=%v result=%+v", reason, err, result)
			}
			var meta map[string]any
			if err := json.Unmarshal(result.Metadata, &meta); err != nil || meta["ui_only"] != true || meta["terminal_reason"] != "model_error" || meta["context_usage"] == nil {
				t.Fatalf("diagnostic metadata/context snapshot lost: %s %v", result.Metadata, err)
			}
			p.fail = false
			s = agentcore.NewSession(opts)
			defer s.Close()
			if err := s.Resume(opts.SessionID); err != nil {
				t.Fatal(err)
			}
			text, _, err := captureRunSession(t.Context(), s, "continue fixture", func(a db.Activity) {
				if a.Kind == "result" {
					result = a
				}
			})
			if err != nil || text != "recovered fixture" || strings.Contains(string(result.Metadata), `"ui_only":true`) {
				t.Fatalf("recovery result hidden: %q %v %+v", text, err, result)
			}
			for _, req := range p.requests {
				body, _ := json.Marshal(req.Messages)
				if strings.Contains(string(body), "balance is insufficient") || strings.Contains(string(body), "无文字总结") {
					t.Fatalf("host diagnostic entered a model request: %s", body)
				}
			}
		})
	}
}

func TestUIDiagnosticsAbsentFromPlannerTriggersAndWorkerTools(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("UI diagnostics fixture", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	es := d.Exploration(exp)
	node, err := es.AddIntent(map[string]any{"summary": "synthetic intent"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	_ = es.SetIntentState(node, "blocked")
	_, err = es.AppendActivity(db.Activity{NodeID: &node, Worker: "fixture", Kind: "text", Summary: "real progress", Detail: "real progress detail"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := es.AppendActivity(db.Activity{NodeID: &node, Worker: "fixture", Kind: "result", IsError: true,
		Summary: "（无文字总结，终态 model_error：诊断）", Detail: uiDiagnosticErrorFixture})
	if err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(es, "fixture")
	output, err := tools.getWorkerOutput().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"intent_id":%d}`, node)), nil)
	if err != nil || !strings.Contains(output.Flatten(), "real progress detail") || strings.Contains(output.Flatten(), "model_error") {
		t.Fatalf("worker output leaked: %s %v", output.Flatten(), err)
	}
	trace, err := tools.getWorkerTrace().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"intent_id":%d,"step_ids":[%d]}`, node, id)), nil)
	if err != nil || strings.Contains(trace.Flatten(), "balance is insufficient") {
		t.Fatalf("worker detail leaked: %s %v", trace.Flatten(), err)
	}
	trigger := renderTriggers(es, []TriggerEvent{{Kind: "done", IntentID: node}})
	if !strings.Contains(trigger, "real progress detail") || strings.Contains(trigger, "model_error") || strings.Contains(trigger, "balance is insufficient") {
		t.Fatalf("planner trigger leaked: %s", trigger)
	}
}

func TestUIDiagnosticsPreserveRealPartialOutputAndBilling(t *testing.T) {
	p := captureUsageProvider{stream: func(_ context.Context, y func(llm.StreamEvent, error) bool) {
		for _, ev := range []llm.StreamEvent{
			{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 17}},
			{Type: llm.SETextDelta, Text: "real partial observation"},
			{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 3}},
		} {
			if !y(ev, nil) {
				return
			}
		}
		y(llm.StreamEvent{}, errors.New(uiDiagnosticErrorFixture))
	}}
	var rows []db.Activity
	_, _, err := captureRun(t.Context(), agentcore.Options{Provider: p, DisableBackgroundTasks: true}, "synthetic", func(a db.Activity) { rows = append(rows, a) })
	if err == nil {
		t.Fatal("expected model failure")
	}
	partial, diagnostic := false, false
	for _, row := range rows {
		if row.Kind == "text" && row.Detail == "real partial observation" {
			partial = !strings.Contains(string(row.Metadata), `"ui_only":true`)
		}
		if row.Kind == "result" {
			diagnostic = strings.Contains(string(row.Metadata), `"ui_only":true`) && strings.Contains(row.Detail, uiDiagnosticErrorFixture)
		}
	}
	if !partial || !diagnostic {
		t.Fatalf("lost real partial output or diagnostic: %+v", rows)
	}
	assertCapturedResultUsage(t, rows, 17, 3, 0, 0)
}
