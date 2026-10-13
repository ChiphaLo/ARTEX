#!/usr/bin/env python3
"""Run an imported upstream Kali MCP filesystem without a container engine.

The image ENV metadata must come from the same trusted image as the rootfs.
This starts only the Gateway; it does not install tools or enable boot services.
"""
import argparse
import json
import os
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-env", required=True, type=Path)
    parser.add_argument("--token-file", required=True, type=Path)
    parser.add_argument("--port", type=int, default=18081)
    parser.add_argument("--tls-san", default="DNS:localhost,DNS:kali-mcp,IP:127.0.0.1")
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("port must be between 1 and 65535")
    if os.name != "posix" or os.geteuid() != 0:
        parser.error("run inside the native Kali Linux filesystem as root")
    if not args.token_file.is_absolute() or not args.token_file.is_file():
        parser.error("token-file must be an existing absolute Linux path")
    env = os.environ.copy()
    with args.image_env.open(encoding="utf-8") as source:
        entries = json.load(source)
    if not isinstance(entries, list) or any(not isinstance(e, str) or "=" not in e for e in entries):
        parser.error("image-env must contain the image Config.Env JSON array")
    for entry in entries:
        key, value = entry.split("=", 1)
        env[key] = value
    # WSL's inherited PATH misses upstream tool wrappers; HOME is also needed
    # by Playwright and Metasploit. Restore image PATH, not setdefault(PATH).
    env["HOME"] = "/root"
    env.pop("MCP_GATEWAY_TOKEN", None)
    env.update({
        "KALI_MCP_HOME": "/opt/kali-mcp",
        "MCP_GATEWAY_TOKEN_FILE": str(args.token_file),
        "MCP_GATEWAY_PORT": str(args.port),
        "MCP_GATEWAY_IP": "127.0.0.1",
        "MCP_GATEWAY_USE_TLS": "true",
        "MCP_GATEWAY_TLS_SAN": args.tls_san,
        "BURP_ACCEPT_EULA": "false",
        "KALI_MCP_BURP_ENABLED": "false",
        "KALI_MCP_BURP_ACCEPT_EULA": "false",
    })
    os.chdir("/work")
    entrypoint = "/opt/kali-mcp/bin/entrypoint.sh"
    os.execve(entrypoint, [entrypoint], env)


if __name__ == "__main__":
    main()
