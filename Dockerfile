# syntax=docker/dockerfile:1
#
# Runtime image (nothing is compiled inside this image): it only installs the
# usual tools and drops in a **prebuilt Linux single binary**. The binary is
# cross-compiled by the CI "binaries" job (pure Go, no QEMU) and placed per
# target arch at dist/<TARGETARCH>/leviathan in the build context. That way, when
# building for multiple architectures, arm64 only needs to emulate the apt
# layer — it no longer needs to emulate the Next/Go compile step, which is
# much faster.
#
# When building this image manually/locally, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/leviathan ./cmd/leviathan
#   docker build -t leviathan:local .
# (Or just use Dockerfile.local with `docker compose build`, which builds
# from source automatically and needs none of the steps above.)
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a standard recon toolkit (add/remove as needed).
# Node is installed from NodeSource at 20.x: Bookworm's apt nodejs package is 18, and
# Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Pre-install Playwright MCP and CLI (globally) so nothing needs to be fetched
# over the network via npx at runtime.
# @playwright/mcp: the browser MCP is run directly as `npx @playwright/mcp`
# (already installed globally, so no -y/@latest needed).
# @playwright/cli: provides playwright-cli; verify it's executable with --help
# right after installing.
# Then install playwright itself (provides browser management); use --with-deps
# to pre-provision chromium and its system dependencies, so the MCP/CLI inside
# the container works on first launch without downloading a browser.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Prebuilt binary for the matching architecture (dist/amd64/leviathan or dist/arm64/leviathan)
COPY dist/${TARGETARCH}/leviathan /app/leviathan
# Supervisor start script: decides whether to relaunch based on the process's
# exit code, and is what makes the one-click update in the UI work. It's also
# responsible for forwarding SIGTERM to leviathan — `docker stop` only signals
# PID 1, so without forwarding, leviathan never receives it and can't shut down
# gracefully; it would get hard-killed by SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/leviathan /app/start.sh
COPY skills/ /app/skills/
# Persistence mount point for data/ (SQLite + jwt.key)
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
