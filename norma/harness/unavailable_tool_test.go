package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestUnavailableToolReasonIsDiagnosticOnly(t *testing.T) {
	for _, reason := range []string{"", "This automatic report is read-only; wait for a new human turn."} {
		t.Run(reason, func(t *testing.T) {
			var got json.RawMessage
			l := &loop{ctx: t.Context(), toolCtx: t.Context(), in: QueryInput{
				Tools:                 tool.NewRegistry(recordingSpec("Allowed", false, &got)),
				UnavailableToolReason: reason,
				PermissionMode:        permission.ModeBypass,
			}}
			for _, name := range []string{"add_intent", "add_task_scope", "Bash", "ExecuteExtraTool"} {
				res, extra := l.execOne(llm.ContentBlock{Type: llm.BlockToolUse, ID: "blocked", Name: name, Input: json.RawMessage(`{}`)}, nil)
				if !res.IsError || res.ToolUseID != "blocked" || len(extra) != 0 || got != nil {
					t.Fatal("missing tool executed or lost error pairing", res)
				}
				text := res.Content[0].Text
				if reason == "" {
					if text != `Error: unknown tool "`+name+`"` {
						t.Fatal("default diagnostic changed", text)
					}
				} else if !strings.Contains(text, reason) || !strings.Contains(text, name) || strings.Contains(text, "unknown tool") {
					t.Fatal("missing restriction explanation", text)
				}
			}
			res, _ := l.execOne(llm.ContentBlock{Type: llm.BlockToolUse, ID: "allowed", Name: "Allowed", Input: json.RawMessage(`{"content":"fixture"}`)}, nil)
			if res.IsError || got == nil {
				t.Fatal("diagnostic option affected available tool", res)
			}
		})
	}
}
