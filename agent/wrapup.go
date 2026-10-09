package agent

import (
	"strings"

	"leviathan/thirdparty/norma/harness"
)

// Wrap-up / settlement prompt: when an agent is terminated because [its step
// budget is exhausted (MaxTurns)] or [it times out (run_seconds/MaxDuration)],
// the SDK's settlement phase injects this prompt so the agent first persists the
// content it identified but did not write back, then outputs a one-sentence
// summary, avoiding a sloppy finish.
//
// Each agent's wrap-up prompt can be overridden in the backend as needed (stored
// in agents.wrapup_prompt); when empty, the built-in default here is used. Only
// the [prompt body] is editable; which tools are disabled and how many turns the
// wrap-up itself gets are code-fixed policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. worker reuses the
// historically hard-coded settleWrapUpPrompt (defined in worker.go); planner and
// mainagent each have a version; anything unmatched (custom agents) uses the
// generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: the built-in default turn budget for each agent's wrap-up
// phase itself (overridable in the backend when >0). All get 10 turns, ensuring
// the wrap-up phase has enough steps to persist. Unmatched keys use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "You are about to run out of steps for this planning round — note this only ends [this round]; the system will still wake you again as the situation changes to continue planning. This is not task termination, so you do not need to wrap up the whole plan here. Persist the conclusions you have already worked out this round so the round isn't wasted, but also [do not pad out intents just to finish] (0 intents this round is still a perfectly normal result): (1) if you have determined an exploration direction that [should be dispatched right now], submit it in a batch with one add_intent (don't hold back what you've worked out); (2) for goals proven met by some finding/fact, call prove_goal to mark them met (don't miss any); (3) if you identified a serial exploitation chain that needs step-by-step work, note it with TodoWrite so the next wake-up can continue dispatching. When done, end this round directly without outputting a summary text."

const mainAgentWrapUpDefault = "You are about to run out of steps and this interaction is ending. Do not start any new exploration/operations. Please **in a single plain-text sentence** summarize for the user the current progress, key conclusions, and suggested next steps."

const genericWrapUpDefault = "You are about to be terminated because your budget is exhausted. First write back the results you have completed but not yet persisted, then **in a single plain-text sentence** summarize what you did and the key conclusions you reached (this sentence will be shown as the result of this run)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up prompt (see the task-timeout and wrap-up design doc) ----------
//
// This is a [separate pair] from the per-run wrap-up prompt: per-run means "your
// budget for this one run is used up"; task timeout means "the whole task has hit
// its deadline and is ending". The semantics are often opposite (especially the
// planner: per-run says "don't stop, keep planning", task timeout says "stop
// planning at the deadline and make the final judgment"). Configured only for
// worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB overrides for the task-timeout
// wrap-up prompt and turn count (wired to agents.task_timeout_wrapup_prompt /
// _max_turns; worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has hit its timeout limit and is ending** (this is not this run's budget, it's the entire exploration reaching its deadline). This is the last chance: (1) persist [all] content you have identified but not written back — new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed findings via report_finding; (2) do not start any new commands/probes; (3) **finally, in a single plain-text sentence**, summarize your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The whole task has hit its timeout limit and is ending** (not this round — the entire task terminates). Based on the current [full] set of facts and findings, make the final goal judgment: call prove_goal to mark met any goal proven by evidence (don't miss any). **Do not generate any new intents** (dispatching intents now would not be executed anyway). Wrap up once the judgment is done, with no summary text needed."

// TaskTimeoutWrapupDefault returns the built-in task-timeout wrap-up prompt for an
// agent (for the backend placeholder / restore-default value).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // unconfigured (mainagent/chat) returns an empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. An empty
// string means the agent has no task-timeout prompt (non-worker/planner), in which
// case the caller should fall back to the per-run prompt.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // default reuses the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is squeezed by the task deadline: settling due to
//     Timeout = the task hit its deadline → task-timeout prompt; settling due to
//     MaxTurns = steps ran out first within the squeeze window while the task still
//     has a few minutes left → fall back to the per-run prompt.
//   - clamped=false → the task is still far off: both reasons use the per-run prompt
//     (i.e. degrades to wrapupSettlement).
//
// The PromptByReason given to harness picks on the fly by the [actual] reason at
// settle-up time, avoiding a build-time mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // the task hit its deadline
				harness.ReasonMaxTurns: perRun, // steps ran out first, the task still has time
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
