package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compression" mechanism introduced in norma v0.4.0,
// exposed to users as an experimental platform feature toggled in system settings. It is
// mutually exclusive with built-in compaction: noaadapter.Enable is the sole entry point,
// and it attaches a context taker-over (Compactor), the Compress tool, and three resident
// prompt sections in one shot; not calling Enable means off (built-in compaction works as
// usual). The toggle is resolved from each agent's injected noaEnabledFn, read once per run,
// so switching only affects runs started afterward with no need to rebuild the agent.

// enableNoa wires noa into opts when the resolver reports it is on. archiveRoot is the
// persistent base directory for compressed source text (it takes the global workDir, so
// every agent lands under <workDir>/noa rather than being scattered per task/intent dir),
// and sessionID names the archive subdirectory beneath it (globally unique, so there is no
// conflict within the same base directory).
//
// noa is experimental: a wiring failure must not interrupt real tasks. On error it reports
// via onWarn and falls back to built-in compaction. On success it clears opts.Compaction to
// avoid agentcore warning about "two context managers set at once".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("failed to enable noa compression, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but the two together make agentcore warn on every
	// turn; clear it explicitly.
	opts.Compaction = nil
}
