package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestKaliConfigAndVisibility(t *testing.T) {
	t.Setenv("KALI_MCP_ENABLED", "false")
	c, err := kaliMCPConfig()
	if err != nil || c != nil {
		t.Fatal("disabled integration should be a no-op")
	}
	t.Setenv("KALI_MCP_ENABLED", "true")
	t.Setenv("KALI_MCP_URL", "")
	t.Setenv("KALI_MCP_TOKEN", "never-persist-this")
	t.Setenv("KALI_MCP_TOKEN_FILE", "")
	t.Setenv("KALI_MCP_CA_CERT", "/secrets/ca.pem")
	c, err = kaliMCPConfig()
	if err != nil || c.URL != "https://kali-mcp:8081/mcp" || c.Insecure {
		t.Fatalf("config: %+v %v", c, err)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "never-persist-this") || !strings.Contains(string(c.Env), "${env:KALI_MCP_TOKEN}") {
		t.Fatal("token persisted")
	}
	t.Setenv("KALI_MCP_TOKEN_FILE", "/secrets/token")
	c, _ = kaliMCPConfig()
	if !strings.Contains(string(c.Env), "${file:/secrets/token}") {
		t.Fatal("file ref missing")
	}
	for _, key := range []string{"planner", "worker", "pentest"} {
		if canUseMCP(c, true, false) {
			t.Fatalf("%s: direct binding bypassed required skill", key)
		}
		if !canUseMCP(c, false, true) {
			t.Fatalf("%s: assigned skill cannot access", key)
		}
	}
	c.Enabled = false
	if canUseMCP(c, true, true) {
		t.Fatal("user disable ignored")
	}
	other := &db.MCPServer{Name: "browser", Enabled: true}
	if !canUseMCP(other, true, false) {
		t.Fatal("other MCP behavior changed")
	}
	t.Setenv("KALI_MCP_ENABLED", "false")
	c.Enabled = true
	if canUseMCP(c, true, true) {
		t.Fatal("integration disable ignored")
	}
	t.Setenv("KALI_MCP_ENABLED", "true")
	t.Setenv("KALI_MCP_URL", "http://bad")
	if _, err := kaliMCPConfig(); err == nil {
		t.Fatal("plaintext allowed")
	}
}
