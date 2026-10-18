package transcript

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestContextExclusionKeepsAuditAndToolPairs(t *testing.T) {
	s := NewStore(t.TempDir())
	w := s.NewWriter("fixture", "")
	w.RecordMessage(llm.UserText("input to remove"), llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "thought to remove"}, {Type: llm.BlockToolUse, ID: "cmd", Name: "probe", Input: []byte("{}")}}}, llm.Usage{InputTokens: 7})
	w.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("cmd", "output", false)}}, llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("keep this")}}, llm.Usage{})
	before, _ := s.Load(s.MainPath("fixture"))
	live, _ := Messages(before)
	for _, selection := range []struct{ kind, text, id string }{{"user", "input to remove", ""}, {"thinking", "thought to remove", ""}, {"tool_use", "", "cmd"}} {
		if err := s.ExcludeSelection("fixture", selection.kind, selection.text, selection.id); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := s.Load(s.MainPath("fixture"))
	msgs, usage := Messages(after)
	if len(after) != 7 || len(msgs) != 1 || msgs[0].Text() != "keep this" || usage.InputTokens != 7 {
		t.Fatalf("audit/usage/context wrong: %d %+v %+v", len(after), msgs, usage)
	}
	filtered, err := s.FilterContext("fixture", live)
	if err != nil || len(filtered) != 1 || filtered[0].Text() != "keep this" {
		t.Fatalf("live filter: %+v %v", filtered, err)
	}
	if before[0].Message.Text() != "input to remove" {
		t.Fatal("source was overwritten")
	}
}

func TestContextExclusionRejectsAmbiguousText(t *testing.T) {
	s := NewStore(t.TempDir())
	w := s.NewWriter("fixture", "")
	w.RecordMessage(llm.UserText("same"), llm.Usage{})
	w.RecordMessage(llm.UserText("same"), llm.Usage{})
	if err := s.ExcludeSelection("fixture", "user", "same", ""); err == nil || !strings.Contains(err.Error(), "唯一") {
		t.Fatal("ambiguous removal accepted", err)
	}
}

func TestContextExclusionCompactionKeepsLatestTail(t *testing.T) {
	s := NewStore(t.TempDir())
	w := s.NewWriter("fixture", "")
	w.RecordMessage(llm.UserText("remove"), llm.Usage{})
	keep := llm.UserText("recent input")
	w.RecordMessage(keep, llm.Usage{})
	w.RecordMessage(llm.BoundaryMessage(llm.BoundaryMeta{Trigger: "auto"}), llm.Usage{})
	w.RecordMessage(llm.UserText("old summary contains remove"), llm.Usage{})
	w.RecordMessage(keep, llm.Usage{})
	if err := s.ExcludeSelection("fixture", "user", "remove", ""); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Load(s.MainPath("fixture"))
	msgs, _ := Messages(recs)
	if len(msgs) != 1 || msgs[0].Text() != "recent input" {
		t.Fatalf("deleted content survived old summary: %+v", msgs)
	}
	w.RecordMessage(llm.BoundaryMessage(llm.BoundaryMeta{Trigger: "auto"}), llm.Usage{})
	w.RecordMessage(llm.UserText("new clean summary"), llm.Usage{})
	w.RecordMessage(keep, llm.Usage{})
	recs, _ = s.Load(s.MainPath("fixture"))
	msgs, _ = Messages(recs)
	api := llm.MessagesForAPI(msgs)
	if len(api) != 2 || api[0].Text() != "new clean summary" || api[1].Text() != "recent input" {
		t.Fatalf("retained tail lost after new boundary: %+v", api)
	}
}

func TestTranscriptConcurrentStoresDoNotInterleaveRecords(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewStore(dir)
			w := s.NewWriter("fixture", "")
			for j := range 10 {
				w.RecordMessage(llm.UserText(fmt.Sprintf("%d/%d:%s", i, j, strings.Repeat("payload", 2000))), llm.Usage{})
				if _, err := NewStore(dir).Load(s.MainPath("fixture")); err != nil {
					t.Error("read a partial append", err)
				}
			}
			if err := w.Err(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s := NewStore(dir)
	recs, err := s.Load(s.MainPath("fixture"))
	if err != nil || len(recs) != 40 {
		t.Fatal(len(recs), err)
	}
}

func TestContextExclusionDuringCompactionDropsStaleSummary(t *testing.T) {
	s := NewStore(t.TempDir())
	w := s.NewWriter("fixture", "")
	remove := llm.UserText("remove while compacting")
	w.RecordMessage(remove, llm.Usage{})
	keep := llm.UserText("keep")
	w.RecordMessage(keep, llm.Usage{})
	// The compactor started before the removal and returns afterward.
	working := []llm.Message{remove, keep, llm.BoundaryMessage(llm.BoundaryMeta{Trigger: "auto"}), llm.UserText("stale summary containing removed text"), keep}
	if err := s.ExcludeSelection("fixture", "user", remove.Text(), ""); err != nil {
		t.Fatal(err)
	}
	filtered, err := s.FilterContext("fixture", working)
	if err != nil || len(filtered) != 1 || filtered[0].Text() != "keep" {
		t.Fatalf("stale summary reached live context: %+v %v", filtered, err)
	}
}
