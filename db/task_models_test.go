package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestTaskModelsIndependentRoundtripAndEdits(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	pid, err := d.SaveProfile(&LLMProfile{Name: fmt.Sprintf("task-model-%d", time.Now().UnixNano()), Format: "openai-responses", Model: "default", Models: []string{"alpha", "beta"}, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteProfile(pid) })
	create := func(model string) *Task {
		t.Helper()
		task, err := d.CreateTaskWithOptions("synthetic model test", "verify routing", TaskCreateOptions{LLMProfileIDs: []int64{pid}, LLMModels: map[int64]string{pid: model}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
		return task
	}
	a, b, inherited := create(" alpha "), create("beta"), create("")
	assertModel := func(id int64, want string) *Task {
		t.Helper()
		got, err := d.GetTask(id)
		if err != nil || got == nil || got.LLMModels[pid] != want {
			t.Fatalf("task %d=%+v err=%v want=%q", id, got, err, want)
		}
		return got
	}
	assertModel(a.ID, "alpha")
	assertModel(b.ID, "beta")
	assertModel(inherited.ID, "")
	if a.LLMModels[pid] != "alpha" {
		t.Fatal("create response not normalized")
	}
	list, err := d.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range list {
		if got.ID == a.ID && got.LLMModels[pid] != "alpha" || got.ID == b.ID && got.LLMModels[pid] != "beta" {
			t.Fatalf("bulk model hydration=%+v", got)
		}
	}
	chain, err := d.TaskLLMProfiles(a.ID)
	if err != nil || len(chain) != 1 || chain[0].Model != "alpha" {
		t.Fatalf("chain=%+v err=%v", chain, err)
	}
	for _, pinned := range []bool{true, false} {
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer tx.Rollback()
			if _, err := tx.Exec(`DELETE FROM task_llm_profiles WHERE task_id=$1`, a.ID); err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"task_id": a.ID, "profile_id": pid, "status": "ready"}
			want := ""
			if pinned {
				row["model"] = "alpha"
				want = "alpha"
			}
			raw, _ := json.Marshal([]any{row})
			if _, err := restoreTaskLLMProfiles(tx, raw); err != nil {
				t.Fatal(err)
			}
			var model string
			if err := tx.QueryRow(`SELECT model FROM task_llm_profiles WHERE task_id=$1`, a.ID).Scan(&model); err != nil || model != want {
				t.Fatalf("archive model=%q want=%q err=%v", model, want, err)
			}
		}()
	}
	if err := d.ReplaceTaskLLMProfiles(a.ID, []int64{pid}, pid); err != nil {
		t.Fatal(err)
	}
	assertModel(a.ID, "alpha") // old client must retain the pin
	before := assertModel(a.ID, "alpha")
	for _, invalid := range []map[int64]string{{pid: "unknown"}, {pid + 99999: "alpha"}} {
		if err := d.ReplaceTaskLLMProfiles(a.ID, []int64{pid}, pid, invalid); !errors.Is(err, ErrTaskModelInvalid) {
			t.Fatalf("invalid selection accepted: %v", err)
		}
		if got := assertModel(a.ID, "alpha"); got.LLMChainRevision != before.LLMChainRevision {
			t.Fatal("invalid edit partially committed")
		}
		if _, err := d.CreateTaskWithOptions("invalid fixture", "reject", TaskCreateOptions{LLMProfileIDs: []int64{pid}, LLMModels: invalid}); !errors.Is(err, ErrTaskModelInvalid) {
			t.Fatalf("invalid create=%v", err)
		}
	}
	if err := d.ReplaceTaskLLMProfiles(a.ID, []int64{pid}, pid, map[int64]string{pid: "beta"}); err != nil {
		t.Fatal(err)
	}
	after := assertModel(a.ID, "beta")
	if after.LLMChainRevision <= before.LLMChainRevision {
		t.Fatal("model edit did not bump revision")
	}
	transition, err := d.MarkTaskLLMProfileQuotaExhaustedAtRevision(a.ID, pid, before.LLMChainRevision, "insufficient_quota")
	if err != nil || !transition.Stale || transition.Advanced {
		t.Fatalf("late error changed replacement model: %+v %v", transition, err)
	}
	if err := d.ReplaceTaskLLMProfiles(a.ID, []int64{pid}, pid, map[int64]string{}); err != nil {
		t.Fatal(err)
	}
	assertModel(a.ID, "")
	if err := d.ReplaceTaskLLMProfiles(b.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := assertModel(b.ID, ""); len(got.LLMModels) != 0 {
		t.Fatal("removed Provider retained model")
	}
	p, err := d.ProfileByID(pid)
	if err != nil || p.Model != "default" {
		t.Fatalf("global default changed: %+v %v", p, err)
	}
}
