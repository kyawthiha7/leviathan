#!/bin/sh
# LEVIATHAN supervisor start script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       run in foreground (Ctrl-C to stop)
#   nohup ./start.sh >leviathan.log 2>&1 &   run in background, persistent
#   ./start.sh -addr :9000           extra args are passed through to leviathan as-is
#
# It does exactly one thing: keep leviathan running, and decide whether to
# relaunch it based on its exit code once the process exits.
#
#   0      user stopped it normally   -> exit the loop
#   75     app requested a restart    -> relaunch immediately (user clicked
#                                        "one-click update" or "rollback" in the UI)
#   other  crash                      -> back off then relaunch (1->2->4… up to 60s)
#
# Deliberately NOT doing downloads, SHA256 verification, or swapping the
# binary here: that logic would have to be written twice (sh and bat), and
# it's exactly the part that can least afford to be wrong — once a binary
# that won't run gets swapped in, this script will faithfully keep relaunching
# it, and the user is stuck fixing it by hand on the machine. So verification/
# swapping all stays in Go (the selfupdate package), done by leviathan itself at
# startup, and this script stays dumb.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./leviathan
[ -x "$BIN" ] || { echo "[leviathan] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
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
		"$RESTART_CODE")
			# Update/rollback is ready: leviathan will finish swapping itself in
			# on this relaunch (see selfupdate.Bootstrap).
			echo "[leviathan] restart requested (applying new version)…"
			delay=1
			;;
		*)
			echo "[leviathan] exited abnormally (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
