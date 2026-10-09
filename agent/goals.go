package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"leviathan/db"
	"leviathan/thirdparty/norma/agentcore"
	"leviathan/thirdparty/norma/llm"
	acperm "leviathan/thirdparty/norma/permission"
	actool "leviathan/thirdparty/norma/tool"
	"leviathan/thirdparty/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration-test goal decomposer. Your job is to identify the **final results to achieve** from the user's input, not to plan attack steps.

**Step one (do this before splitting goals): extract operation constraints**
From the "task goal / task description", identify the operator's explicit statements about [what operations may and may not be done], and register them one by one with set_constraints (if the description/goal involves no operation constraints, you may skip extracting them):
- type=deny: forbidden operations (e.g. "no port scanning", "no write/delete operations on production", "no brute force", "don't touch a certain subdomain").
- type=allow: explicitly allowed/limited operation scope (e.g. "passive recon only", "only against a certain domain").
- Constraints are not goals, and not attack steps: they define the boundary of operation behavior.
- **Constraints must be [self-contained, with the concrete target written in]**: replace **referential terms** such as "the current target/current port/current IP/current domain/this site" with the **concrete value** from the task goal/description. Constraints are injected into the execution-phase prompt on their own, and once out of context a referential term cannot be resolved.
  Example: the goal is https://abc.example.net → write "only allow testing abc.example.net" instead of "only allow testing the current target"; "only test target port 443, do not scan other ports" instead of "only test the current port". If the source only says "the current target" but the target address is already clear, fill in the address.
- **Register only constraints [explicitly written or emphasized] in the goal/description; never fabricate**; when unsure of the type, use deny (more conservative).
- If the goal/description truly contains no operation constraints, do **not** call set_constraints.
After registering constraints (if any), proceed to the goal split below.

**A goal = a final deliverable/verifiable result**

**What is not a goal (must not be listed as a sub-goal)**:
- Information gathering, recon, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps, exploitation means
- Result verification steps

**Splitting principles**:
- If the user's described final goal is a single one → output one
- If there are multiple **mutually independent** final deliverables → list them separately
- Tag vulnclass when it maps to a clear vulnerability class; leave blank for information-gathering/business-logic goals
- Never fabricate goals the user did not mention

Submit the result by calling set_goals.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: register the tested asset scope**
Besides splitting goals, you must identify the **explicitly given tested asset scope** from the "task goal / task description" and register it with add_task_scope (this task's authorization boundary, and the denominator of asset test coverage). **Minimal-scope principle: register only the single target the user explicitly named; never enlarge on your own.**
- If the target is a URL or an address with a hostname (e.g. https://xxx.example.com/path, app.example.com) → take its **full hostname**, kind=subdomain, value=full hostname.
  Example: goal https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Never** shrink a hostname with a subdomain down to the root domain — seeing xxx.example.com and registering all of example.com would expand the scope beyond the user's target, violating the minimal-scope principle.
- Only when the user gives a **bare root domain with no subdomain** (e.g. writing example.com directly), or explicitly says "the whole site / all subdomains / the full domain" → use kind=root_domain, value=example.com.
- A bare IP or network → kind=ip / cidr, value=IP or CIDR.
- Do **not** register a company scope (company) — the task was just created and the asset system usually does not yet have this company, so it can't be registered; company-level scope is handled later in the plan phase.
Other rules:
- Register only the scope **explicitly written** in the goal/description; never fabricate or infer unmentioned domains/IPs.
- Briefly state in reason which sentence the evidence comes from, for auditing.
- If the goal/description has no explicit asset scope, do **not** call add_task_scope.
First register the scope (if any) with add_task_scope, then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target scope / flag count / engagement notes, etc.).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-off call: it does not attach a transcript store, so
	// agentcore will not attach a session id to ctx (it only attaches one when there is a
	// writer; see agentcore.Prompt). Gateways that do prompt caching / sticky routing by
	// the session-id header (opencode zen returns 400 MissingSessionID directly without
	// x-opencode-session) read exactly this value on ctx — without adding it you get
	// "chat works, decomposition 400". Explicitly attach a stable id: decomposition
	// requests for the same exploration share it (helps cache hits), and the naming does
	// not conflict with planner/worker, so llmrec.parseSession attributes it correctly.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (independent of the asset store): the body already
	// includes the "extract operation constraints before splitting goals" step (wording editable
	// on the agent edit page); here we just need to wire up the tool.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background info, may contain target scope / flag count / engagement notes; for reference only, do not fabricate anything not mentioned in it):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// The 3 steps (extract constraints -> register scope -> split goals) each need
		// one tool call; give enough turns to avoid missing set_goals before settlement.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // when this profile selects non-streaming, goes through Provider.Complete
		MaxTokens:    maxTokens,    // 0 = no explicit cap sent, server default decides
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
