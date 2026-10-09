#!/usr/bin/env bash
# Dev mode: backend (:8787) + traffic proxy (:8788) run together with the
# frontend's next dev (:5173). The frontend proxies /api to the backend;
# Ctrl-C stops both.
#
# For the single-binary (embedded frontend) approach, see the "Single Binary"
# section of the README — that doesn't go through this script.
set -euo pipefail
cd "$(dirname "$0")"

# On exit, kill every child process in this process group (backend + frontend).
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (plain go run, frontend not embedded); the concurrent work-agent
# count is configured in "System Settings".
go run ./cmd/leviathan -addr :8787 -proxy 127.0.0.1:8788 &

# Frontend hot reload (Vite/Next dev server, /api proxied to :8787).
( cd web && npm run dev ) &

echo "[dev] backend :8787 / proxy :8788 / frontend http://localhost:5173  (Ctrl-C to exit)"
wait
