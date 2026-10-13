# Minimal test runtime; never installed over a running deployment.
# Compile the actual ARTEX cmd and test binaries per run.sh before building.
FROM python:3.12-slim-bookworm
WORKDIR /app
COPY tests/kali-mcp/artifacts/artex /app/artex
COPY tests/kali-mcp/artifacts/server.test /app/server.test
COPY tests/kali-mcp/artifacts/db.test /app/db.test
COPY tests/kali-mcp/artifacts/mcphttp.test /app/mcphttp.test
COPY skills /app/skills
ENTRYPOINT ["/app/artex"]
CMD ["-addr", ":8787"]
