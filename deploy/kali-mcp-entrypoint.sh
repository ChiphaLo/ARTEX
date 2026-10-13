#!/usr/bin/env bash
set -euo pipefail
# Upstream Nmap carries cap_net_admin=eip. Linux refuses exec when that
# file capability is outside Docker's bounding set, even for root. Drop
# this file's capabilities, not the container restriction: root retains
# the explicitly granted NET_RAW, and NET_ADMIN stays unavailable.
if [[ -f /usr/lib/nmap/nmap ]] && [[ -n "$(getcap /usr/lib/nmap/nmap)" ]]; then
  setcap -r /usr/lib/nmap/nmap
fi
if [[ "${KALI_MCP_BURP_ENABLED:-false}" == true ]]; then
  if [[ "${KALI_MCP_BURP_ACCEPT_EULA:-false}" != true ]]; then
    echo "Kali MCP: Burp needs explicit KALI_MCP_BURP_ACCEPT_EULA=true" >&2
    exit 1
  fi
  export BURP_ACCEPT_EULA=true
else
  export BURP_ACCEPT_EULA=false
fi
exec /opt/kali-mcp/bin/entrypoint.sh
