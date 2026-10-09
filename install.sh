#!/usr/bin/env bash
# LEVIATHAN install script: ① full Docker  ② local build & run
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── docker detection / auto-install ───────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker and docker compose detected"; return
  fi
  warn "docker / docker compose not detected"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Auto-install Docker? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker installed (group change takes effect after re-login, no sudo needed)"
      else
        die "Please install docker yourself and retry"
      fi ;;
    Darwin) die "On macOS, please install Docker Desktop: https://www.docker.com/products/docker-desktop/" ;;
    MINGW*|MSYS*|CYGWIN*) die "On Windows, please install Docker Desktop: https://www.docker.com/products/docker-desktop/ (run this script again from Git Bash afterwards)" ;;
    *)      die "Please install docker yourself and retry" ;;
  esac
}

# ── ① full Docker ───────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres password (press Enter to generate randomly)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY (can leave blank, configure later in the UI)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env generated (POSTGRES_PASSWORD has been set)"
  else
    info "Using existing .env"
  fi
  info "Building the leviathan image from source (first run can take a few minutes)…"
  docker compose pull postgres || true
  docker compose build leviathan
  docker compose up -d
  ok "Startup complete → http://localhost:8787"
  info "View logs: docker compose logs -f leviathan"
}

# ── ② local build & run ──────────────────────────────
install_local(){
  echo "Database setup method:"
  echo "  1) Connect to an existing PostgreSQL"
  echo "  2) Start a PostgreSQL via Docker (requires docker)"
  case "$(ask 'Choose' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres password (press Enter for random)' "$(rand)")"
      docker run -d --name leviathan-pg -p 5432:5432 \
        -e POSTGRES_USER=leviathan -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=leviathan \
        -v leviathan-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=leviathan DB_PASS="$pw" DB_NAME=leviathan DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'Database host' 127.0.0.1)"
      DB_PORT="$(ask 'Port' 5432)"
      DB_USER="$(ask 'Username' leviathan)"
      DB_PASS="$(ask 'Password' '')"
      DB_NAME="$(ask 'Database name' leviathan)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # generate config.json
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json generated"

  # go environment check
  command -v go >/dev/null 2>&1 || die "Go not detected, please install Go first (>=1.26): https://go.dev/dl/"
  ok "Go: $(go version)"

  # embedded frontend requires node to produce static assets
  if command -v npm >/dev/null 2>&1; then
    info "Building frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Compiling embedded single binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o leviathan ./cmd/leviathan
  else
    warn "npm not detected: will build the backend **without an embedded frontend** (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -o leviathan ./cmd/leviathan
  fi
  ok "Build complete → ./leviathan"

  info "Starting… (Ctrl-C to exit)"
  ./leviathan
}

echo "=============================="
echo "  LEVIATHAN Install"
echo "  1) Full Docker install"
echo "  2) Local run (go build)"
echo "=============================="
case "$(ask 'Choose' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "Invalid choice" ;;
esac
