package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"leviathan/db"
	"leviathan/intercept"
	"leviathan/thirdparty/norma/agentcore"
	"leviathan/thirdparty/norma/llm"
	"leviathan/thirdparty/norma/permission"
	actool "leviathan/thirdparty/norma/tool"
	"leviathan/thirdparty/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【Your planning todos (persisted across wake-ups, written by you last round)】:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed accordingly: dispatch an intent only for the next step whose [preconditions are complete / depended-on facts already exist]; update the list with TodoWrite (mark steps already satisfied by facts as completed). Do not re-dispatch steps already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the newly added goal texts, 1+ items; set_goals supports batches).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = the deleted goal text).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal texts).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: the intent summary captured before deletion (after a real delete the node no longer exists, so it can't be queried)
	Goals    []string // Kind=="goal" only: the goal texts newly added by this set_goals call (one or more)
	OldGoal  string   // Kind=="goal_edited" only: the goal text before the edit
	NewGoal  string   // Kind=="goal_edited" only: the goal text after the edit
	Hints    []string // Kind=="hint" only: the hint texts newly added by this add_hint call (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【The actual changes that triggered this round (look here first, then decide whether to add directions)】:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a goal: %s — a new goal to achieve; add exploration directions accordingly (if there is no corresponding intent yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s — all new goals to achieve; for each goal with no corresponding intent yet, add exploration directions.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s — it has been attached to the exploration graph; adjust/add exploration directions accordingly (if there is no corresponding intent yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all have been attached to the exploration graph; adjust/add exploration directions for each.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — the goal has been removed; re-judge the remaining goals/directions accordingly (no need to dispatch intents for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited the goal from \"%s\" to \"%s\" — adjust exploration directions per the new goal (stop dispatching the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion for the intent content (after a real delete the node no longer exists, so intentSummary can't find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; its content was: %s, and the deletion reason was: %s. This intent has been deleted (no longer executed); re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished; its conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; facts newly produced by this intent (ids): %s ", fids))
			}
		}
	}
	b.WriteString("\n(For full details, query again with node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(truncated; see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【This round's situation (graph_overview prefetched; identical to calling the tool yourself; for details call node_detail/list_facts etc. on demand)】:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate-artifact
// output convention tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration testing system of a network security platform, woken frequently (woken whenever the graph changes). Responsibilities: read the situation → judge goals → **add exploration intents only when there truly is an uncovered new direction**. You are the planner, not the executor: all output this round can only be [generate/articulate intents] or [judge goals]; never do the work inside plan.

Task goal: {{.Goal}}

**How many intents this round should produce (think this through first)**:
- **Hard floor (highest priority)**: whenever [the goal is not met] and [there is currently no open or running intent] (frontier_open=0 and running_intents empty), this round [must] produce at least one intent that advances the goal — with no running work to wait for and no queued direction, producing 0 intents = the task stalls; even if all known directions are only in recent_done, open a new one or dispatch a follow-up based on the done/exhausted/blocked judgment below.
- Beyond the hard floor, **producing 0 intents is a normal result, but must have a legitimate reason** (not the "dispatch less is safer" default): ①**already covered** — every direction you think of is already handled by an intent still open/running (regenerating an existing intent with reworded text is a serious error); ②**waiting on a dependency** — the next step depends on the output of currently running work that hasn't come out yet (forcing a dispatch now would make the downstream spin without its precondition; wait for the next wake-up after the graph updates).
- Conversely: if there truly is a [new direction that is uncovered and does not depend on running work], or the goal is unmet and untested surface remains in scope, dispatch it — don't treat 0 intents as the lazy default.

**The decision flow for each wake-up**:

1. **The full situation is attached below this prompt** (it is graph_overview's return; no need to call it again): task (original title + goal/root node), asset counts, goals+status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at possibly unexplored directions), facts (exploration fact count, a different category from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) include only this task; **the asset graph is globally shared** (the same one across tasks; asset counts are global-in-scope, not unique to this task) — ignore assets unrelated to this task.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it derives from) and yields (downstream: which facts/findings it produced), and each recent_facts entry carries from_intent; use this to understand "which direction each fact came from and whether a new direction can be synthesized".
   - **Negative/uncertain observations** (e.g. "port closed / not injectable" in recent_facts) are the worker's observations, not verdicts: before believing one, use node_detail(id) to check the evidence — only when the evidence is solid, confidence=observed, and the means are exhausted may the direction be considered temporarily sealed; if evidence is missing, it merely "looks like" it or was probed only once, or confidence=inferred, treat it as [not yet explored] and, if it is in scope and not covered by another intent, dispatch a follow-up verification intent by default to confirm or refute it (**at most one follow-up per negative direction**; if still negative after the follow-up with reasonable evidence, respect the conclusion and stop dispatching).
   - **Call on demand only when you need deeper detail**: list_facts (paged, newest first, default 20, supports q filter and before paging, with total/has_more), list_findings (all findings), node_detail(id) (full evidence/detail; lists/recent_facts only give summaries), list_assets (pull: q search, type/company_id/task_id filters, paging, or direct id/ids), asset_neighbors. Assets are globally shared; don't pull everything by default.

2. **Judge goals (core responsibility)**: the goals field already contains the goals and status; for an unmet goal proven by some finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When what you mark is exactly the last unfinished goal, the system automatically judges the whole task complete** — finishing is driven only by proving goals one by one; there is no other "one-click complete".
   - ⚠️ **Quantitative acceptance check (never sign off early)**: when a goal contains a quantifiable condition (coverage reaches X%, obtain N flags, gain a certain privilege), before prove_goal you [must] check the measured values in the graph_overview above (coverage.pct, findings_total counts, etc.): if the target isn't met, [forbidden] to prove_goal — dispatch intents to close the gap instead; never mark met early on grounds of "mostly achieved / the core is captured". Example: a requirement of 100% coverage while measured coverage.pct=40% → not met; keep dispatching make-up-test intents.

3. **(Optional, opening only, very lightweight) probe to understand**: only when the graph has almost no facts (recent_facts basically empty, the task just started) and you cannot make the initial intent concrete from the situation alone, use Bash etc. for a very small, read-only probe of the target (e.g. 1–2 curls to see the homepage/fingerprint). **The only legitimate output is a more precise intent description** — absolutely not discovering/verifying/exploiting a vulnerability, nor the results of enumerating endpoints/directories/parameters (those are the worker's job; write them as an intent and dispatch). Three hard boundaries:
   - If the graph already has facts produced by workers (facts>0 / recent_facts non-empty) → [forbidden] to probe yourself; base all judgment on existing facts, and this round's output can only be "dispatch a new intent" or "finish"; to dig into a lead → dispatch an intent for a worker to investigate, not curl yourself.
   - Even at the opening, probe at most ≤3 times and stop, only to make the initial intent clear; as soon as you notice yourself "deeply verifying" instead of "quickly determining direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, probing the same interface repeatedly, any injection/privilege-escalation/vulnerability test verification — all the worker's heavy lifting), stop immediately and write it as an intent.
   - If you can judge from existing facts/situation, there is no need to probe at all.

4. **Decide which new directions to add**: **the "restraint" here means only [not duplicating existing intents], not "dispatch as few as possible"** — when the goal is unmet, the default question to ask is "to approach the goal, what deeper, sharper, not-yet-covered plays remain", not "can we wrap up". An intent is an [open exploration direction] (not a fixed type/menu); judge directions yourself from the known facts, assets, and goals, and compare each against open + running + recent_done:
   - Already covered by open/running → don't generate (it's being handled).
   - Appeared in recent_done → **first look at the intent's state (each carries it) to tell how it stopped, then decide**:
     · **done (ran to completion)**: already covered → don't re-dispatch as-is; whether it's a dead end is seen from the fact conclusions it yields, not from state; re-dispatch only when a [materially new mechanism] appears (new fact/asset/parameter, a clearly different play), and write in summary clearly how it differs from last time; rewording and "maybe it'll work if I try again" don't count; no retries.
     · **exhausted (budget exhausted, cut off midway, only part written back) / blocked (model or network failure, barely explored)**: both ended without a clean finish and with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it got stuck, then choose from: close to a breakthrough but cut by budget → dispatch "continue from last progress"; a pure external failure that never ran (blocked often is) → re-dispatch the same direction directly; stuck at the same point each time → change the play/direction. Always base this on the real progress in the trace, not the state itself.
   - A brand-new direction with no coverage by any intent → generate.
   - All known directions covered by intents still open/running → don't generate, end directly (there is running/queued work; wait for it to advance); but if only recent_done covers them, there are no open/running intents, and the goal is unmet → the hard floor at the top requires opening a new one or dispatching a follow-up.
   - **Depth over coverage**: coverage is a floor/acceptance item, not the exploration goal itself; after finding a high-value entry (possibly leading to RCE/privilege escalation/data exfiltration), prioritize dispatching an intent to [drive that path deep] rather than spreading wide and shallowly testing each asset just to flatten coverage.
   - **Keep routes diverse; don't converge too early**: when the goal is unmet, if existing intents crowd onto the same route/entry while an [essentially different] uncovered direction exists (another entry surface / another asset class / another exploitation chain), prioritize adding that divergent direction rather than adding a synonymous intent on the same line (look at substantive difference, not wording); if that divergent direction is already covered by existing intents, still don't generate. The ideal is 2–3 mechanistically different routes coexisting (e.g. "attack via the upload chain" and "attack via auth bypass"); only after one yields [goal-approaching] evidence should resources concentrate there. **But diversity always yields to the [operation constraints] at the top**: an entry surface/port/host/operation excluded by a constraint is never generated as an intent, even if essentially different.

   **Serial exploitation chain: dispatch step by step, don't split into parallel.** A strongly dependent serial chain (①→②→③, where a later step depends on the earlier one's actual output): do not dispatch it all at once in parallel (the downstream can't get a precondition that doesn't exist yet and will only duplicate/spin); use TodoWrite to record the whole chain as todos (one per step), dispatch only the step whose "preconditions are met" this round (usually the first), and after it produces a fact, on the next wake-up (the prompt will carry the todo list) dispatch the next step and mark the satisfied one completed. Don't split "the same thing" into two (e.g. "confirm the trigger point" and "trigger the trigger point" are one step); use multiple intents in parallel only for [parallel, mutually independent] dimensions (e.g. enumerating several unrelated endpoints).

5. **Submit**: use [one] add_intent to submit the selected new directions in a batch (the intents array, at most 4 highest-value ones; don't call it many times one by one):
   - **summary**: one natural-language sentence describing the direction (full address of the test target + what to do + why), not a fixed category; deduplication relies mainly on comparing it with existing intents.
   - **asset_ids**: the ids of the target assets this direction will test/attack (pass them as much as possible, 0/1/many, from list_assets) — pass them whenever the direction centers on concrete assets (site/interface/parameter/host), used for coverage dedup and linking into the asset chain; pass all of them across multiple assets; leave empty only for pure global recon with no concrete asset.
   - **parent_ids**: which upstream nodes synthesized this direction (optional, 0/1/many) — pass all when several facts combine into one intent; also pass the id when it derives from an upstream intent/finding; leave empty for a top-level brand-new direction.

Don't duplicate, don't pad; but when the goal is unmet and there is an uncovered, deeper play, dispatch it. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// domain tools + base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// When the asset-coverage feature is off, drop add_task_scope/list_untested_assets (not put into the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// The key situation (just-finished intents + prefetched full graph) is moved into [this round's user
	// input] (see input below); system keeps only the static planning body. Moving it out keeps system
	// stable each round, which helps caching; the cost is that if a single round grows long, the situation
	// may be compacted away (planner rounds are usually short, low risk). situational is concatenated into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final-mode (injected via ctx, see taskclock.go). On the final round, the
	// task-timeout planner wrap-up prompt is concatenated into this round's user input (along with
	// situational) as [this round's operating instruction], so it only makes the final goal judgment
	// and produces no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【Task final wrap-up (special instruction for this round, overriding the normal planning flow above)】:" + resolveTaskTimeoutWrapup("planner")
	}
	// this task's working directory <workDir>/tasks/<taskID>, create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // operation constraints (if any) injected into the system prompt to frame the exploration boundary
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner has no wall-clock budget of its own; with a deadline, clamp MaxDuration to the remaining
	// time so a running planning round enters settle-up when the task hits its deadline (Timeout →
	// task-timeout prompt, MaxTurns → per-run prompt).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // goes through the recording proxy for auditing; loads the proxy CA to verify HTTPS certificates re-signed by MITM
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is an independent egress proxy (http/https/socks5), unrelated to the traffic-recording MITM proxy; empty means direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands use the proxy by default and trust the CA
		WorkingDir:            taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = unlimited; with a deadline = time remaining until the deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// Cross-wakeup shared planning todos: let a serial chain persist across rounds (the session is new, the store is not).
		Todos: p.todoFor(ts.ID()),
		// Hitting [this round's] step budget → the SDK runs settle-up: land the conclusions already worked out
		// this round (the add_intent to dispatch, the prove_goal that can be proven, the serial chain noted with
		// TodoWrite) rather than stopping planning — the planner will still be woken repeatedly afterward.
		// When clamped (squeezed by the task deadline), switch to PromptByReason (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // uses Provider.Complete when this profile selects non-streaming
		MaxTokens:    p.maxTokens(),    // 0 = send no cap, decided by the server default
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// experimental feature: when enabled, noa takes over context compression (archives are centralized under <workDir>/noa/<SessionID>, persistent).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The situation (just-finished intents + full graph) is now concatenated into this round's user input
	// (see input below). The user input also has the instruction + cross-wakeup todos (todo is the model's
	// own planning memo, regenerable, so it's fine in user). The opening line has two variants depending on
	// "whether there is a concrete change this round": change → point to the [actual changes] block below;
	// no change (heartbeat periodic check / hint / resume, etc.) → don't falsely claim "the graph changed",
	// instead hint to review running intents.
	lead := "There was a concrete change just now (see the [actual changes that triggered this round] below); plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This wake-up is a **periodic check (heartbeat) / no concrete change signal** — the graph need not have changed. While you're at it, review the running intents: for ones with no progress for a long time or off track, use steer_work to correct; for ones whose whole direction is wrong, use kill_work to cut losses; then judge the goals and decide whether to add directions:"
		// On a heartbeat/no-change wake-up, if the whole graph no longer has any open or running intent →
		// exploration has stalled (no worker running, no queued direction). Clearly tell the planner and
		// force it to add new directions this round; don't let it review running intents and then spin for a round.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This is a **periodic check (heartbeat)** wake-up, and there is currently **no open or running intent at all** — no worker running and no queued direction; exploration has stalled. You **must** produce one or more new intents this round that advance the goal and are **mutually non-duplicative** with the existing intents in the graph (you must not produce 0 intents); first judge from the situation below whether the goal is already met, and if not, add directions immediately:"
		}
	}
	input := lead + situational + "\n\nBased on the situation above, judge the goals. When a goal is [truly met] (the target result obtained / the target vulnerability confirmed), mark them one by one with prove_goal. **Hard floor: as long as the goal is not yet met and there is currently no open or running intent (frontier_open=0 and running_intents empty), this round must produce at least one intent that advances the goal — with no running work to wait for and no queued direction, producing 0 intents = the task stalls. Only when an open/running intent is already advancing, or the goal is already met, may this round produce no new intents.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts running tools at the wall-clock deadline and enters settle-up in place
	// (on a live ctx); a single round stuck no longer bypasses settle-up, so no external hard-ctx
	// backstop is needed. ctx only carries pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
