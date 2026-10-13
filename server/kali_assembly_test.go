package server

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// Real assembly with a TLS fixture, independent of the large Kali image.
func TestKaliSkillGatingWithTLSFixture(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("isolated Postgres required")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	t.Setenv("KALI_MCP_ENABLED", "true")
	t.Setenv("KALI_TEST_SECRET", "fixture-token")
	var deleted atomic.Int32
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("secret not resolved")
			w.WriteHeader(401)
			return
		}
		if r.Method == "DELETE" {
			deleted.Add(1)
			w.WriteHeader(204)
			return
		}
		var q struct {
			ID     int
			Method string
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.ID == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch q.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fixture")
			result = map[string]any{"protocolVersion": "2025-06-18"}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "nmap_port_scan", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "fixture-ok"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
	}))
	defer gateway.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gateway.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "kali-mcp"), 0755)
	if err := os.WriteFile(filepath.Join(dir, "kali-mcp", "SKILL.md"), []byte("---\nname: kali-mcp\ndescription: fixture\nmcps: kali\n---\nfixture"), 0644); err != nil {
		t.Fatal(err)
	}
	all, err := pg.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	row := &db.MCPServer{Name: "kali", Transport: "http", URL: gateway.URL, Enabled: true, CACert: ca, Env: json.RawMessage(`{"Authorization":"Bearer ${env:KALI_TEST_SECRET}"}`)}
	for _, m := range all {
		if m.Name == "kali" {
			original := *m
			row.ID = m.ID
			t.Cleanup(func() { _, _ = pg.SaveMCP(&original) })
		}
	}
	id, err := pg.SaveMCP(row)
	if err != nil {
		t.Fatal(err)
	}
	row.ID = id
	if !strings.Contains(string(row.Env), "${env:") {
		t.Fatal("fixture stored plaintext")
	}
	if row.ID == 0 {
		t.Fatal("missing MCP id")
	}
	found := false
	for _, m := range all {
		if m.ID == id {
			found = true
		}
	}
	if !found {
		t.Cleanup(func() { _ = pg.DeleteMCP(id) })
	}
	wireAgentAugment(pg, dir, nil)
	t.Cleanup(func() { agent.ToolAugment = nil })
	for _, key := range []string{"planner", "worker", "pentest"} {
		ag, err := pg.GetAgentByKey(key)
		if err != nil || ag == nil {
			t.Fatalf("%s: %v", key, err)
		}
		oldS, _ := pg.AgentSkillNames(ag.ID)
		oldM, _ := pg.AgentVisible(ag.ID, "mcp")
		t.Cleanup(func() { _ = pg.SetAgentSkillVisibility(ag.ID, oldS); _ = pg.SetAgentVisibilityKind(ag.ID, "mcp", oldM) })
		_ = pg.SetAgentSkillVisibility(ag.ID, nil)
		_ = pg.SetAgentVisibilityKind(ag.ID, "mcp", []int64{id})
		tools, _, cleanup := agent.ToolAugment(t.Context(), key)
		cleanup()
		for _, tool := range tools {
			if strings.HasPrefix(tool.Name(), "mcp__kali__") {
				t.Fatal("direct binding bypassed Skill assignment")
			}
		}
		_ = pg.SetAgentSkillVisibility(ag.ID, []string{"kali-mcp"})
		tools, def, cleanup := agent.ToolAugment(t.Context(), key)
		if len(def.Deferred) != 1 || len(def.GlobalNames) != 0 {
			t.Fatalf("gating: %+v", def)
		}
		reg := actool.NewRegistry(tools...)
		ex := actool.NewExecuteExtraTool(reg, def.Unlock)
		in := json.RawMessage(`{"tool_name":"mcp__kali__nmap_port_scan","params":{}}`)
		before, err := ex.Call(t.Context(), in, nil)
		if err != nil || !before.IsError {
			t.Fatal("unloaded Skill callable")
		}
		def.UnlockSkill("kali-mcp")
		after, err := ex.Call(t.Context(), in, nil)
		if err != nil || after.IsError {
			t.Fatalf("unlocked call: %v %v", after, err)
		}
		cleanup()
	}
	if deleted.Load() != 3 {
		t.Fatal(fmt.Sprintf("expected 3 closed sessions, got %d", deleted.Load()))
	}
}
