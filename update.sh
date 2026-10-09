#!/usr/bin/env bash
# LEVIATHAN update script: ① Docker update (pull new image and rebuild)  ② local
# build update (rebuild the binary).
# Pairs with install.sh: install handles the first-time setup, update handles
# upgrading to a new version.
# No manual DB migration needed — leviathan idempotently reruns schema.sql
# (ADD COLUMN / CREATE INDEX IF NOT EXISTS, etc.) on every startup, so
# "restart = migrate". Data (the pgdata volume, ./data, ./skills) is unaffected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync the repo to the latest code (updates compose files,
# scripts, and local build source) ───────────────────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or diverged branch) — resolve manually and retry; continuing with the current code for now"
  fi
}

# ── ① Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not detected, please deploy with ./install.sh first"
  [ -f .env ] || die ".env not found, please run ./install.sh first to complete initial setup"

  # Optional: upgrade to a specific image tag (leave blank to keep .env's
  # LEVIATHAN_TAG, defaulting to latest)
  local tag; tag="$(ask 'Target image tag (press Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^LEVIATHAN_TAG=' .env; then
      sed -i.bak "s|^LEVIATHAN_TAG=.*|LEVIATHAN_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nLEVIATHAN_TAG=%s\n' "$tag" >> .env
    fi
    ok "LEVIATHAN_TAG set to ${tag}"
  fi

  # Only touch leviathan: postgres is pinned to 16-alpine and doesn't need to be
  # upgraded alongside it (pulling it would just waste bandwidth, and a major
  # version bump carries compatibility risk). leviathan declares depends_on
  # postgres, so running `up` with the service name will auto-start postgres
  # if it isn't running, and leave it alone untouched if it already is.
  info "Rebuilding the leviathan image from source (first build after an update can take a few minutes)…"
  docker compose build --no-cache leviathan
  info "Starting (leviathan migrates the schema automatically on restart)…"
  docker compose up -d leviathan
  ok "Update complete → http://localhost:8787"
  info "View logs: docker compose logs -f leviathan"
  info "Clean up old images (optional): docker image prune -f"
}

# ── ② local build update ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go not detected (>=1.26): https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found — if this is a first-time deployment, use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompiling embedded single binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o leviathan ./cmd/leviathan
  else
    warn "npm not detected: building the backend **without an embedded frontend** (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -o leviathan ./cmd/leviathan
  fi
  ok "Build complete → ./leviathan"
  warn "Please restart the running leviathan process for the update to take effect (schema migrates automatically on restart)"
}

echo "=============================="
echo "  LEVIATHAN Update"
echo "  1) Docker update (pull new image and rebuild)"
echo "  2) Local update (go rebuild)"
echo "=============================="
case "$(ask 'Choose' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid choice" ;;
esac
