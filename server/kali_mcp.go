package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const kaliMCPName = "kali"

func kaliMCPEnabled() bool { on, _ := strconv.ParseBool(os.Getenv("KALI_MCP_ENABLED")); return on }

func kaliMCPConfig() (*db.MCPServer, error) {
	if !kaliMCPEnabled() {
		return nil, nil
	}
	endpoint := os.Getenv("KALI_MCP_URL")
	if endpoint == "" {
		endpoint = "https://kali-mcp:8081/mcp"
	}
	if !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("KALI_MCP_URL must use HTTPS")
	}
	// Persist only references, never the resolved token.
	token := "${env:KALI_MCP_TOKEN}"
	if f := os.Getenv("KALI_MCP_TOKEN_FILE"); f != "" {
		token = "${file:" + f + "}"
	}
	headers, _ := json.Marshal(map[string]string{"Authorization": "Bearer " + token})
	ca := ""
	if os.Getenv("KALI_MCP_CA_CERT") != "" {
		ca = "${env:KALI_MCP_CA_CERT}"
	}
	return &db.MCPServer{Name: kaliMCPName, Transport: "http", URL: endpoint, Env: headers, Enabled: true, CACert: ca}, nil
}

// A failed gateway cannot block ARTEX startup. Retry and refresh changed lists.
func (s *Server) runKaliMCP() {
	cfg, err := kaliMCPConfig()
	if err != nil {
		log.Printf("[kali-mcp] configuration: %v", err)
		return
	}
	if cfg == nil {
		return
	}
	if err := s.m.pg.SeedMCP(cfg); err != nil {
		log.Printf("[kali-mcp] registration: %v", err)
		return
	}
	for {
		all, err := s.m.pg.ListMCP()
		if err == nil {
			for _, m := range all {
				if m.Name != kaliMCPName || !m.Enabled {
					continue
				}
				ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
				err = s.discoverAndCacheMCP(ctx, m)
				cancel()
				if err != nil {
					log.Printf("[kali-mcp] discovery (will retry): %v", err)
				}
			}
		} else {
			log.Printf("[kali-mcp] reading registration: %v", err)
		}
		timer := time.NewTimer(2 * time.Minute)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Kali is always skill-gated, even with a direct MCP binding.
func canUseMCP(m *db.MCPServer, direct, skillGated bool) bool {
	if !m.Enabled {
		return false
	}
	if m.Name == kaliMCPName {
		return kaliMCPEnabled() && skillGated
	}
	return direct || skillGated
}

// Audit via the existing server log sink plus meteredTool ledger, without
// recording arguments, response content, headers, or credentials.
type kaliAuditedTool struct{ actool.CoreTool }

func (k *kaliAuditedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	start := time.Now()
	result, err := k.CoreTool.Call(ctx, in, tc)
	status := "ok"
	if err != nil || result.IsError {
		status = "error"
	}
	log.Printf("[kali-mcp] tool=%s status=%s duration_ms=%d", k.Name(), status, time.Since(start).Milliseconds())
	return result, err
}
