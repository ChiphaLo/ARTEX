package db

// SeedMCP only inserts an absent row; it never changes edits, disable switches,
// or Agent bindings. A UNIQUE name makes concurrent/repeated startups safe.
func (d *DB) SeedMCP(m *MCPServer) error {
	args, env := string(m.Args), string(m.Env)
	if args == "" {
		args = "[]"
	}
	if env == "" {
		env = "{}"
	}
	_, err := d.Exec(`INSERT INTO mcp_servers(name,transport,command,args,env,url,enabled,insecure,ca_cert)
 VALUES ($1,$2,NULLIF($3,''),$4,$5,NULLIF($6,''),$7,$8,$9) ON CONFLICT (name) DO NOTHING`,
		m.Name, m.Transport, m.Command, args, env, m.URL, m.Enabled, m.Insecure, m.CACert)
	return err
}
