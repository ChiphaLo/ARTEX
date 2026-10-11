package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestConversationMessagesExposeFullTokenTotal(t *testing.T) {
	f := newSideHTTPFixture(t)
	parent, _ := f.conversation(t)
	id := parent.ConversationID
	appendRow := func(kind string, n int) {
		t.Helper()
		if _, err := f.m.pg.AppendConvActivity(id, db.Activity{Kind: kind, InputTokens: &n}); err != nil {
			t.Fatal(err)
		}
	}
	appendRow("usage", 30)
	appendRow("result", 30)
	for range 220 {
		appendRow("text", 0)
	}
	appendRow("usage", 7)
	for _, query := range []string{"?limit=200", "?since=0", "?since=999999999"} {
		w := f.call(t, "GET", fmt.Sprintf("/api/conversations/%d/messages%s", id, query), nil, 200)
		var response struct {
			Tokens db.ConversationTokenTotal `json:"tokens"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Tokens.InputTokens != 37 || response.Tokens.Turns != 2 {
			t.Fatalf("%s: total depends on loaded window: %+v", query, response.Tokens)
		}
	}
}
