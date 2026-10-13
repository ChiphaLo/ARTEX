package db

import (
	"encoding/json"
	"testing"
)

func TestMCPSeedPreservesEditsAndChangingToolCache(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c := &MCPServer{Name: "test-kali-seed", Transport: "http", URL: "https://kali:8081/mcp", Enabled: true, Env: json.RawMessage(`{"Authorization":"Bearer ${env:TEST_TOKEN}"}`), CACert: "/tmp/ca"}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM mcp_servers WHERE name=$1`, c.Name) })
	if err := d.SeedMCP(c); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := d.QueryRow(`SELECT id FROM mcp_servers WHERE name=$1`, c.Name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	c.ID = id
	c.Enabled = false
	c.URL = "https://user-edited"
	c.CACert = "/user/ca"
	if _, err := d.SaveMCP(c); err != nil {
		t.Fatal(err)
	}
	if err := d.SeedMCP(&MCPServer{Name: c.Name, Transport: "http", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	all, err := d.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	var got *MCPServer
	for _, m := range all {
		if m.ID == id {
			got = m
		}
	}
	if got == nil || got.Enabled || got.URL != c.URL || got.CACert != c.CACert {
		t.Fatalf("edits lost: %+v", got)
	}
	for _, tools := range [][]MCPTool{{{Name: "a"}, {Name: "b"}}, {{Name: "a", Description: "changed"}}, nil} {
		if err := d.SaveMCPTools(id, tools); err != nil {
			t.Fatal(err)
		}
		got, err := d.MCPToolsDetailed(id)
		if err != nil || len(got) != len(tools) {
			t.Fatalf("cache: %v %v", got, err)
		}
	}
}
