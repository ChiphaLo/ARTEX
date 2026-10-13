package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
	actool "github.com/Autumn-27/norma/tool"
)

// These run only in the dedicated Docker or native lab. No LLM/public target.
func integrationPG(t *testing.T) *db.DB {
	t.Helper()
	if os.Getenv("KALI_TEST_URL") == "" {
		t.Skip("Kali integration lab not configured")
	}
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

// Check the actual running binary/API too, rather than only an in-process mux.
func TestKaliRunningARTEXAPI(t *testing.T) {
	base := os.Getenv("KALI_TEST_ARTEX_URL")
	if base == "" {
		t.Skip("running ARTEX test instance not configured")
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatal("running instance must be the loopback lab")
	}
	key, err := os.ReadFile(os.Getenv("KALI_TEST_JWT_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := signJWT(key)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 40 * time.Second}
	request := func(method, path string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, resp.StatusCode, want, b)
		}
		return b
	}
	request("GET", "/api/health", 200)
	var listing struct {
		Servers []*db.MCPServer `json:"servers"`
	}
	if err := json.Unmarshal(request("GET", "/api/mcp", 200), &listing); err != nil {
		t.Fatal(err)
	}
	var row *db.MCPServer
	for _, m := range listing.Servers {
		if m.Name == kaliMCPName {
			row = m
		}
	}
	if row == nil || row.Insecure {
		t.Fatal("running process lacks verified Kali registration")
	}
	path := "/api/mcp/" + strconv.FormatInt(row.ID, 10) + "/refresh"
	if os.Getenv("KALI_TEST_GATEWAY_DOWN") == "true" {
		request("POST", path, 502)
		request("GET", "/api/health", 200)
		t.Log("running ARTEX remains healthy while Gateway is unavailable")
		return
	}
	var tools struct {
		Tools []db.MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(request("POST", path, 200), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("running API discovered no tools")
	}
	request("GET", "/api/stats", 200)
	// UI must be present in the embedded native/standard test build.
	page := request("GET", "/system/mcp/", 200)
	if !strings.Contains(string(page), "ARTEX") {
		t.Fatal("embedded ARTEX UI unavailable")
	}
	t.Logf("running binary/API/UI discovered %d Gateway tools", len(tools.Tools))
}

func TestKaliDockerGateway(t *testing.T) {
	pg := integrationPG(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cfg, err := kaliMCPConfig()
	if err != nil || cfg == nil {
		t.Fatalf("config: %v", err)
	}
	opts := mcphttp.Options{CACert: cfg.CACert}
	for _, token := range []string{"", "deliberately-incorrect-token"} {
		h := map[string]string{}
		if token != "" {
			h["Authorization"] = "Bearer " + token
		}
		if _, err := mcphttp.NewWithOptions(ctx, "kali", cfg.URL, h, opts); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("unauthorized request: %v", err)
		}
	}
	if err := pg.SeedMCP(cfg); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pg.QueryRow(`SELECT id FROM mcp_servers WHERE name='kali'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	cfg.ID = id
	s := &Server{m: &Manager{pg: pg}}
	if err := s.discoverAndCacheMCP(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cached, err := pg.MCPToolsDetailed(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"nmap", "masscan", "whatweb", "nuclei", "sqlmap", "ffuf", "searchsploit", "metasploit", "playwright", "tshark", "hashcat", "binwalk", "pdtools"} {
		found := false
		for _, tool := range cached {
			if strings.HasPrefix(tool.Name, "mcp__kali__"+prefix+"_") {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing category %s", prefix)
		}
	}
	for _, tool := range cached {
		if strings.Contains(tool.Name, "__burp_") {
			t.Fatal("Burp unexpectedly enabled")
		}
	}
	t.Logf("discovered %d tools; all required categories found", len(cached))
	if output := os.Getenv("KALI_TEST_ARTIFACT_DIR"); output != "" {
		b, _ := json.MarshalIndent(cached, "", "  ")
		if err := os.WriteFile(filepath.Join(output, "tools.json"), b, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Skill assignment is required even with a direct MCP binding. Exercise all
	// three roles via the real assembly hook and inspect their deferred gate.
	skillDir := os.Getenv("KALI_TEST_SKILL_DIR")
	wireAgentAugment(pg, skillDir, nil)
	t.Cleanup(func() { agent.ToolAugment = nil })
	var workerTools []actool.CoreTool
	var workerDef agent.DeferredInfo
	var workerCleanup func()
	for _, key := range []string{"planner", "worker", "pentest"} {
		ag, err := pg.GetAgentByKey(key)
		if err != nil || ag == nil {
			t.Fatalf("agent %s missing", key)
		}
		originalSkills, _ := pg.AgentSkillNames(ag.ID)
		originalMCP, _ := pg.AgentVisible(ag.ID, "mcp")
		t.Cleanup(func() {
			_ = pg.SetAgentSkillVisibility(ag.ID, originalSkills)
			_ = pg.SetAgentVisibilityKind(ag.ID, "mcp", originalMCP)
		})
		_ = pg.SetAgentSkillVisibility(ag.ID, nil)
		_ = pg.SetAgentVisibilityKind(ag.ID, "mcp", []int64{id})
		extra, _, closeUnbound := agent.ToolAugment(ctx, key)
		closeUnbound()
		for _, tool := range extra {
			if strings.HasPrefix(tool.Name(), "mcp__kali__") {
				t.Fatalf("%s leaked tools without Skill", key)
			}
		}
		if err := pg.SetAgentSkillVisibility(ag.ID, []string{"kali-mcp"}); err != nil {
			t.Fatal(err)
		}
		tools, def, closeBound := agent.ToolAugment(ctx, key)
		t.Cleanup(closeBound)
		if len(def.Deferred) < len(cached) || len(def.GlobalNames) != 0 {
			t.Fatalf("%s: schema gating incorrect", key)
		}
		name := "mcp__kali__nmap_port_scan"
		if def.Unlock.Has(name) {
			t.Fatal("tool initially unlocked")
		}
		def.UnlockSkill("kali-mcp")
		if !def.Unlock.Has(name) {
			t.Fatal("Skill did not unlock")
		}
		if key == "worker" {
			workerTools, workerDef = tools, def
			workerCleanup = closeBound
		} else {
			closeBound()
		}
	}
	reg := actool.NewRegistry(workerTools...)
	execute := actool.NewExecuteExtraTool(reg, workerDef.Unlock)
	target := os.Getenv("KALI_TEST_TARGET")
	if os.Getenv("KALI_TEST_MODE") == "native" {
		if target != "127.0.0.1" {
			t.Fatal("native test target must be the dedicated loopback fixture")
		}
	} else if target != "fixture" {
		t.Fatal("Docker test target must be the isolated fixture container")
	}
	name := "mcp__kali__nmap_port_scan"
	in, _ := json.Marshal(map[string]any{"tool_name": name, "params": map[string]any{"target": target, "ports": "8000", "timing": 3, "timeout": 30}})
	result, err := execute.Call(ctx, in, nil)
	b, _ := json.Marshal(result)
	if err != nil || result.IsError || !strings.Contains(string(b), "8000") {
		t.Fatalf("isolated nmap call failed: %s %v", b, err)
	}
	var calls int
	if err := pg.QueryRow(`SELECT count(*) FROM tool_usage WHERE tool_key=$1 AND agent_key='worker'`, name).Scan(&calls); err != nil || calls < 1 {
		t.Fatalf("audit ledger missing: %v", err)
	}
	t.Log("controlled single-port call returned through ExecuteExtraTool and audit ledger")
	workerCleanup()

	// Each Worker/session gets an independent client; do not share one session
	// between runs. This also proves simultaneous discovery/DELETE works.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl, e := connectMCP(ctx, cfg)
			if e != nil {
				t.Error(e)
				return
			}
			defer cl.Close()
			if ts, e := cl.Tools(ctx); e != nil || len(ts) != len(cached) {
				t.Errorf("parallel discovery %d: %v", len(ts), e)
			}
		}()
	}
	wg.Wait()
}

// The lab runs this with the gateway stopped, but the ARTEX service still up.
func TestKaliDockerUnavailable(t *testing.T) {
	pg := integrationPG(t)
	if os.Getenv("KALI_TEST_GATEWAY_DOWN") != "true" {
		t.Skip("negative control phase only")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cfg, _ := kaliMCPConfig()
	if _, err := connectMCP(ctx, cfg); err == nil {
		t.Fatal("stopped gateway unexpectedly answered")
	}
	if _, err := pg.ListAgents(); err != nil {
		t.Fatal(err)
	}
	// Disabled integration never grants the persisted row; other MCPs remain usable.
	t.Setenv("KALI_MCP_ENABLED", "false")
	if canUseMCP(cfg, true, true) {
		t.Fatal("disabled integration usable")
	}
	if !canUseMCP(&db.MCPServer{Name: "browser", Enabled: true}, true, false) {
		t.Fatal("disabled Kali affected other MCP")
	}
	t.Log(fmt.Sprintf("gateway down is isolated; DB/Agent configuration preserved"))
}
