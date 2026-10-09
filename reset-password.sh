#!/usr/bin/env bash
# =============================================================================
# LEVIATHAN admin password reset script
#
# The login username is fixed at LEVIATHAN; the password is stored as a bcrypt
# hash under the `auth.password_hash` key in the database's `settings` table.
# This script connects to the database and uses pgcrypto to generate the
# bcrypt hash in-database and write it back to that key — fully compatible
# with the backend's login check (golang.org/x/crypto/bcrypt).
#
# Two deployment modes:
#   local  (default) -- connect to the database directly from the host via psql.
#                        Connection info is resolved in this priority order:
#                        command-line args > --dsn/$LEVIATHAN_PG_DSN > config.json's database.*
#   docker            -- run psql inside the postgres container via
#                        `docker compose exec` (or `docker exec`) — compose
#                        doesn't expose 5432 to the host by default, hence
#                        going through the container.
#
# Usage examples:
#   ./reset-password.sh                          # local, auto-read config.json/env, prompt for new password
#   ./reset-password.sh -p 'NewPass!'            # local, pass the new password directly
#   ./reset-password.sh --dsn postgres://u:p@h:5432/leviathan
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U autopentest -W pass -d leviathan
#   ./reset-password.sh -m docker                # docker deployment (reads POSTGRES_* from .env)
#   ./reset-password.sh -m docker -c pg_container_name --exec docker
#
# Security: the new password is passed in via an environment variable +
# psql's \getenv (never appears in process argv), and is auto-escaped with
# :'var' (prevents SQL injection); the database password is passed via
# PGPASSWORD, likewise kept out of argv.
# =============================================================================
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker (blank = auto-detect)
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # postgres service/container name in docker mode (default: postgres)
EXEC_KIND=""       # compose | docker (which exec style to use in docker mode; blank = auto)
NEWPASS=""
ASSUME_YES=0

die() { echo "Error: $*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- argument parsing -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "Unknown argument: $1 (use -h for usage)" ;;
  esac
done

# ---- read database.* from config.json (local mode only, and only when
# connection info wasn't given explicitly) -----
# Prefer python3 for parsing (robust); fall back to grep when python3 is
# missing (config.json has consistently formatted fields).
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# Supports either a direct dsn, or individual fields
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # Minimal fallback: grep each key individually (values are strings or numbers)
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- auto-detect mode ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${LEVIATHAN_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "Deployment mode: $MODE"

# ---- collect the new password -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "Enter new password (username is fixed as LEVIATHAN): " NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "Password cannot be empty"
  read -r -s -p "Re-enter to confirm: " NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "The two entries do not match"
fi
[[ -n "$NEWPASS" ]] || die "Password cannot be empty"

# Hand the password to psql via an environment variable (read with \getenv,
# never appears in argv/ps)
export LEVIATHAN_RESET_NEWPASS="$NEWPASS"

# Generate the bcrypt hash in-database and upsert it; the password is
# auto-escaped via :'newpw'. CREATE EXTENSION is idempotent; if the database
# role lacks permission to create extensions this will error here (see the
# hint in the failure branch of `run` below).
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw LEVIATHAN_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- execute -----------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # Connection info priority: command line > --dsn/$LEVIATHAN_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${LEVIATHAN_PG_DSN:-}" ]] && DSN="$LEVIATHAN_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "Reading database config from $cfg"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "psql not found on this machine (install postgresql-client, or use -m docker instead)"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "Missing database user (-U) or a valid config.json/DSN"
    [[ -n "$DBNAME" ]] || die "Missing database name (-d) or a valid config.json/DSN"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "Target database: $target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "Confirm resetting the LEVIATHAN password on this database? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "Cancelled"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "Write failed. If this is a pgcrypto permission/missing-extension error, use a role that can create extensions, or run CREATE EXTENSION pgcrypto manually first."
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "docker not found"
  CONTAINER="${CONTAINER:-postgres}"

  # Choose the exec style: prefer docker compose exec (service name),
  # otherwise docker exec (container name)
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # psql credentials inside the container: command line first, then .env's
  # POSTGRES_*, then fall back to compose's default (leviathan)
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-leviathan}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-leviathan}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "Target: psql -U $DUSER -d $DNAME inside container $CONTAINER (exec=$EXEC_KIND)"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "Confirm resetting the LEVIATHAN password on this container's database? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "Cancelled"
  fi

  # -e with just the name (no value) inherits it from the current
  # environment, so the password never appears in the docker command's argv.
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e LEVIATHAN_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e LEVIATHAN_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "Write failed. Please check the container name (-c), the database account (.env's POSTGRES_*), and that the role has pgcrypto permission."
  fi
fi

unset LEVIATHAN_RESET_NEWPASS
echo "✓ LEVIATHAN admin password has been reset. Log in with username LEVIATHAN + the new password (no service restart needed)."
