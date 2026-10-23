package transcript

import (
	"github.com/Autumn-27/norma/llm"
	"testing"
)

func TestLatestRunRecordsSeparatesRootsAndKeepsContinuationAndControls(t *testing.T) {
	store := NewStore(t.TempDir())
	old := store.NewWriter("planner", "")
	old.RecordMessage(llm.UserText("old round"), llm.Usage{})
	current := store.NewWriter("planner", "")
	current.RecordMessage(llm.UserText("current round"), llm.Usage{})
	current.RecordMessage(llm.UserText("retained"), llm.Usage{})
	recs, _ := store.Load(store.MainPath("planner"))
	continued := store.ResumeWriter("planner", LastUUID(recs))
	continued.RecordMessage(llm.UserText("human continuation"), llm.Usage{})
	if err := store.ExcludeSelection("planner", "user", "current round", ""); err != nil {
		t.Fatal(err)
	}
	recs, _ = store.Load(store.MainPath("planner"))
	selected := LatestRunRecords(recs)
	msgs, _ := Messages(selected)
	if len(msgs) != 2 || msgs[0].Text() != "retained" || msgs[1].Text() != "human continuation" {
		t.Fatal(msgs)
	}
	if len(recs) != 5 || len(selected) != 4 {
		t.Fatal("audit changed or control dropped", len(recs), len(selected))
	}
}
