#!/bin/sh
# LEVIATHAN supervisor start script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       run in foreground (Ctrl-C to stop)
#   nohup ./start.sh >leviathan.log 2>&1 &   run in background, persistent
#   ./start.sh -addr :9000           extra args are passed through to leviathan as-is
#
# It does exactly one thing: keep leviathan running, and relaunch it after a
# crash with a backoff delay.
#
#   0      user stopped it normally   -> exit the loop
#   other  crash                      -> back off then relaunch (1->2->4… up to 60s)
set -u

cd "$(dirname "$0")" || exit 1

BIN=./leviathan
[ -x "$BIN" ] || { echo "[leviathan] executable not found: $BIN" >&2; exit 1; }

MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the leviathan process itself.
#
# This is required under Docker: `docker stop` only sends SIGTERM to PID 1
# (i.e. this script), not to child processes. Without forwarding, leviathan never
# sees the signal and can't shut down gracefully — it gets hard-killed by
# SIGKILL after 10 seconds, and any in-flight task is cut off mid-way.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. At that point the
	# child is actually still doing its graceful shutdown, so we need to wait
	# again to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[leviathan] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[leviathan] exited normally"
			exit 0
			;;
		*)
			echo "[leviathan] exited abnormally (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
