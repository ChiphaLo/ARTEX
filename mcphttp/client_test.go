package mcphttp

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSecretReferences(t *testing.T) {
	t.Setenv("TEST_MCP_TOKEN", "test-secret-123")
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		in, want string
		fail     bool
	}{
		{"Bearer ${env:TEST_MCP_TOKEN}", "Bearer test-secret-123", false},
		{"Bearer ${file:" + f + "}", "Bearer file-secret", false},
		{"unchanged", "unchanged", false},
		{"${env:NOT_SET_MCP_TOKEN}", "", true},
		{"${file:/missing/token}", "", true},
	} {
		got, err := ResolveReferences(tt.in)
		if (err != nil) != tt.fail || (!tt.fail && got != tt.want) {
			t.Fatalf("resolution: %q %v", got, err)
		}
	}
	if _, _, err := newHTTPClient(map[string]string{"Authorization": "bad\r\nheader"}, Options{}); err == nil {
		t.Fatal("header injection accepted")
	}
}

func testRPCServer(t *testing.T, tlsServer bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var seq, closed atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "echo "+r.Header.Get("Authorization"), 401)
			return
		}
		if r.Method == "DELETE" {
			if r.Header.Get("Mcp-Session-Id") == "" {
				t.Error("close missing session")
			}
			closed.Add(1)
			w.WriteHeader(204)
			return
		}
		var q rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var result any
		switch q.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", seq.Add(1)))
			result = map[string]any{"protocolVersion": protocolVersion}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") == "" {
				t.Error("list missing session")
			}
			p, _ := q.Params.(map[string]any)
			name, cursor := "echo", "second"
			if p["cursor"] == "second" {
				name, cursor = "fail", ""
			}
			result = map[string]any{"tools": []remoteTool{{Name: name, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}}}, "nextCursor": cursor}
		case "tools/call":
			p, _ := q.Params.(map[string]any)
			if p["name"] == "rpc-error" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{"code": -32602, "message": "invalid arguments"}})
				return
			}
			if p["name"] == "slow" {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(time.Second):
				}
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "isError": p["name"] == "fail"}
		}
		b, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n\n", q.ID, b)
	})
	if tlsServer {
		return httptest.NewTLSServer(h), &closed
	}
	return httptest.NewServer(h), &closed
}

func TestStreamableLifecycleConcurrentCalls(t *testing.T) {
	srv, closed := testRPCServer(t, false)
	defer srv.Close()
	c, err := New(t.Context(), "kali", srv.URL, map[string]string{"Authorization": "Bearer test-secret"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := c.Tools(t.Context())
	if err != nil || len(ts) != 2 || ts[0].Name() != "mcp__kali__echo" {
		t.Fatalf("tools: %v %v", ts, err)
	}
	wrapped, err := ts[1].Call(t.Context(), json.RawMessage("{}"), nil)
	if err != nil || !wrapped.IsError {
		t.Fatal("tool wrapper lost isError")
	}
	invalid, err := ts[0].Call(t.Context(), json.RawMessage("{invalid"), nil)
	if err != nil || !invalid.IsError {
		t.Fatal("invalid tool arguments were sent")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, e := c.Call(t.Context(), "echo", map[string]any{"value": "hello"})
			if e != nil || s != "ok" {
				t.Errorf("call %q %v", s, e)
			}
		}()
	}
	wg.Wait()
	if _, err := c.Call(t.Context(), "fail", nil); err == nil {
		t.Fatal("isError lost")
	}
	if _, err := c.Call(t.Context(), "rpc-error", nil); err == nil || !strings.Contains(err.Error(), "-32602") {
		t.Fatal("RPC error lost")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "slow", nil); err == nil {
		t.Fatal("timeout lost")
	}
	_ = c.Close()
	_ = c.Close()
	if closed.Load() != 1 {
		t.Fatalf("session deleted %d times", closed.Load())
	}
}

func TestIndependentWorkerSessions(t *testing.T) {
	srv, closed := testRPCServer(t, false)
	defer srv.Close()
	a, err := New(t.Context(), "kali", srv.URL, map[string]string{"Authorization": "Bearer test-secret"}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(t.Context(), "kali", srv.URL, map[string]string{"Authorization": "Bearer test-secret"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.sessionID == "" || a.sessionID == b.sessionID {
		t.Fatal("worker sessions shared")
	}
	_ = a.Close()
	if _, err := b.Tools(t.Context()); err != nil {
		t.Fatal("closing worker A affected B")
	}
	_ = b.Close()
	if closed.Load() != 2 {
		t.Fatal("worker sessions were not closed")
	}
}

func TestPerConnectionCAAndAuthRedaction(t *testing.T) {
	srv, _ := testRPCServer(t, true)
	defer srv.Close()
	headers := map[string]string{"Authorization": "Bearer test-secret"}
	if _, err := New(t.Context(), "kali", srv.URL, headers, false); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewWithOptions(t.Context(), "kali", srv.URL, headers, Options{CACert: ca})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Adding a private root to this connection must not lower another's trust.
	if _, err := New(t.Context(), "other", srv.URL, headers, false); err == nil {
		t.Fatal("CA trust leaked globally")
	}
	_, err = NewWithOptions(t.Context(), "kali", srv.URL, map[string]string{"Authorization": "Bearer wrong-secret"}, Options{CACert: ca})
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("auth error not safe: %v", err)
	}
}
