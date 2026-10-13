#!/usr/bin/env bash
# A fully isolated real-image lab; no internet targets, LLM, or published ports.
set -euo pipefail
cd "$(dirname "$0")/../.."
export POSTGRES_PASSWORD=artex-isolated-test-only
export KALI_MCP_ENABLED=true
export KALI_MCP_SECRET_DIR=./tests/kali-mcp/secrets
export KALI_MCP_TOKEN_FILE=/run/secrets/kali-mcp/token
export KALI_MCP_BURP_ENABLED=false
export KALI_MCP_BURP_ACCEPT_EULA=false
mkdir -p tests/kali-mcp/{secrets,artifacts}
chmod 700 tests/kali-mcp/secrets
if [[ ! -s tests/kali-mcp/secrets/token ]]; then
  (umask 077; openssl rand -hex 32 > tests/kali-mcp/secrets/token)
fi
# Use staged binaries when testing from a Windows builder; otherwise compile.
if [[ ! -f tests/kali-mcp/artifacts/server.test ]]; then
  CGO_ENABLED=0 go build -o tests/kali-mcp/artifacts/artex ./cmd/artex
  CGO_ENABLED=0 go test -c -o tests/kali-mcp/artifacts/server.test ./server
  CGO_ENABLED=0 go test -c -o tests/kali-mcp/artifacts/db.test ./db
  CGO_ENABLED=0 go test -c -o tests/kali-mcp/artifacts/mcphttp.test ./mcphttp
fi
chmod +x tests/kali-mcp/artifacts/{artex,server.test,db.test,mcphttp.test}
dc() { docker compose --env-file /dev/null -p artex-kali-lab -f docker-compose.yml -f docker-compose.kali-mcp.yml -f tests/kali-mcp/compose.yml --profile kali "$@"; }
cleanup() { dc down --remove-orphans > tests/kali-mcp/artifacts/cleanup.log 2>&1 || true; }
trap cleanup EXIT
dc config --quiet
echo "PASS: compose config (no public ports; dedicated internal bridge)"
dc up -d kali-mcp fixture postgres
ready=false
for i in $(seq 1 150); do
  if dc exec -T kali-mcp test -s /opt/kali-mcp/ssl/service_ca.crt; then ready=true; break; fi
  sleep 2
done
[[ "$ready" == true ]]
dc cp kali-mcp:/opt/kali-mcp/ssl/service_ca.crt tests/kali-mcp/secrets/service_ca.crt
dc up -d --build artex
wait_gateway() {
  for i in $(seq 1 150); do
    # Readiness alone can precede msfrpcd, which makes discovery omit Meta.
    if dc exec -T kali-mcp bash -c 'curl -fsS http://127.0.0.1:15021/healthz/ready >/dev/null && ss -lnt | grep -q ":55553 "' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "Gateway / Metasploit RPC readiness timeout" >&2
  return 1
}
wait_gateway
dc exec -T artex /app/mcphttp.test -test.v | tee tests/kali-mcp/artifacts/mcphttp.log
dc exec -T artex /app/server.test -test.v -test.run '^TestKaliDockerGateway$' | tee tests/kali-mcp/artifacts/gateway.log
dc exec -T artex /app/db.test -test.v -test.run '^TestMCPSeed' | tee tests/kali-mcp/artifacts/seed.log
dc exec -T artex /app/server.test -test.v -test.run '^TestCoreTaskLifecyclePG$' | tee tests/kali-mcp/artifacts/core.log
dc stop kali-mcp
dc exec -e KALI_TEST_GATEWAY_DOWN=true -T artex /app/server.test -test.v -test.run '^TestKaliDockerUnavailable$' | tee tests/kali-mcp/artifacts/unavailable.log
dc exec -T artex python3 -c 'import urllib.request; assert b"artex" in urllib.request.urlopen("http://127.0.0.1:8787/api/health").read()'
echo "PASS: ARTEX remains healthy while Gateway is stopped"
dc restart artex
sleep 5
dc exec -T artex python3 -c 'import urllib.request; assert b"artex" in urllib.request.urlopen("http://127.0.0.1:8787/api/health").read()'
echo "PASS: ARTEX starts with Gateway unavailable"
dc start kali-mcp
wait_gateway
dc exec -T artex /app/server.test -test.v -test.run '^TestKaliDockerGateway$' | tee tests/kali-mcp/artifacts/restart.log
dc logs --no-color artex > tests/kali-mcp/artifacts/artex.log
echo "PASS: rediscovery after restart; Docker integration complete"
