package server

import (
	"github.com/Autumn-27/artex/db"
	"testing"
)

func TestExternalMCPHasNoReservedNames(t *testing.T) {
	for _, name := range []string{"kali", "browser", "custom-gateway"} {
		m := &db.MCPServer{Name: name, Enabled: true}
		if !canUseMCP(m, true, false) || !canUseMCP(m, false, true) || canUseMCP(m, false, false) {
			t.Fatal("inconsistent external visibility", name)
		}
		m.Enabled = false
		if canUseMCP(m, true, true) {
			t.Fatal("disabled external server is visible")
		}
	}
}
