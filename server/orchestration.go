package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"leviathan/agent"
	"leviathan/db"
	"leviathan/thirdparty/norma/permission"
	actool "leviathan/thirdparty/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// This file implements the P2 "cross-task orchestration toolset" (docs/benchmark-orchestration §2 P2).
// These are host tools — they need access to the Manager (any task's Store), the Engine
// (pause), and the task-creation flow, so they live in the server layer. The read tools
// redirect the existing per-task tools onto the target task's store (build a temporary
// ToolSet and Call the corresponding tool), reusing the exact same logic; the control
// tools (spawn/pause) call the Manager/Engine directly. Like the traffic tools they are
// seeded into the tools table and bound per agent (visible only to orchestration agents
// they're bound to).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // platform-operation tools (create/update skill/tool/MCP, for the Auto agent)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] loading failed: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id is required"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task not found: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // generic wake-up (write operations without a dedicated callback use it; no-op for read tools)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → records a "the human added N strategic hints: …" trigger and wakes the planner
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"List all tasks (id/description/goal/status/run duration/parent task/LLM profile); the orchestration agent uses it to grasp the whole picture, see which tasks are stuck too long, and which LLM each uses. Run duration: running = created→now, terminal = created→last activity (seconds). llm_profile: the profile name the task's planner/worker uses, (active profile) = follows the globally active one.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(active profile)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"List the available LLM profiles: id, name, model, format, and whether it is the currently active profile. Use the id to give spawn_task's llm_profile_id parameter a subtask-specific LLM (e.g. a cheap model for recon, a strong model for exploitation). No API keys included.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Create a subtask and start its exploration engine, returning task_id. Use it to dispatch one thing (e.g. a challenge/one objective) as its own task. parent_ref is optional: fill in the id of the parent task this orchestration relates to for a parent-child link.",
		objSchema(map[string]any{
			"description":            strParam("task description (short title)"),
			"goal":                   strParam("task goal (what to achieve)"),
			"parent_ref":             strParam("optional: parent task id (for a parent-child link)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("optional: list of source task ids to inherit read-only (at most %d). The subtask may read-only reference the assets/conclusions already explored by these tasks as a starting point; unlike the pure parent-child pointer of parent_ref, this is content inheritance.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "optional: the LLM profile id for this subtask's planner/worker (see list_llm_profiles); blank inherits the parent task, then falls back to the globally active profile"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "optional: task-level timeout (seconds). On expiry it triggers a graceful wrap-up and enters the timeout terminal state; blank or 0 = no limit"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "optional: planner heartbeat trigger interval (seconds). Once this much time has passed since the last planning round ended / the task started with no trigger in between → trigger a planning round (deadlock fallback + wake-up to supervise in-flight workers). blank or 0 = default 600 (10min);"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "optional: can be enabled for simple tasks; on creation it directly issues one seed intent (content = description + goal) so a worker can start testing without waiting for the first planner round; default false (goes through the standard plan-then-execute)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "unnamed task"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal is required"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// Read-only inheritance of source tasks: count cap + each id valid/deduplicated/exists, with the same validation rules as HTTP task creation.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("at most %d related tasks may be selected", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("related task id is invalid or duplicated"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("related task #%d does not exist", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM profile #%d does not exist or has no API key set", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// Shared post-creation flow, reusing the same launchTask as HTTP task creation
			// (server.go createTask): seed + visible goal decomposition in the background
			// (round 0 / LLM step / per-goal) + engine.Run. seed_first_intent defaults to
			// false (standard plan-then-execute); simple tasks may enable it to issue one
			// work test directly.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Pause the specified task (stops its planner/worker loop).",
		objSchema(map[string]any{"task_id": strParam("id of the task to pause")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task not found: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Read the exploration graph overview of the specified task (same as graph_overview: asset counts/frontier/findings/coverage etc.), using task_id to select the task.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Read the confirmed findings of the specified task (including flag/PoC; each carries id/task_id/intent_id/vulnclass/severity/summary/status), using task_id to select the task.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Inject a strategic hint into the specified task (the task's planner reads it when generating intents next round).\n"+
		"★ Prefer batching: put multiple hints into the hints array and submit at once (returns an ids array the same length and order as hints, id=0 for failed items); for a single hint, omit hints and pass the top-level text directly.",
		objSchema(map[string]any{
			"task_id":      strParam("task id"),
			"hints":        map[string]any{"type": "array", "description": "[prefer this] hint array; each element's fields are the same as the top-level ones (text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("hint content"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[single] hint content"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "anchored asset ids (optional, 0/1/many; asset ids within this task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"Inspect the execution of one work (intent) within the specified task: get_task_worker_trace(task_id, intent_id) shows a step summary; then pass step_ids=[...] to fetch the full content of those steps (at most 5 at a time, extra are truncated to the first 5).",
		objSchema(map[string]any{
			"task_id":   strParam("task id"),
			"intent_id": map[string]any{"type": "integer", "description": "intent id (a work within this task)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "optional: step ids whose full content to fetch (at most 5 at a time, extra are truncated to the first 5 and the rest are listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "List which works (intents) ran in the specified task + each one's step count, to spot which works are worth digging into (then use get_task_worker_trace).",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search all works' execution traces in the specified task by keyword (returns matching step summaries + intent_id).",
		objSchema(map[string]any{"task_id": strParam("task id"), "q": strParam("search keyword")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Read the full content of one exploration-graph node in the specified task (finding/fact/intent/goal: summary + detail/evidence/PoC). id is the exploration node id (as returned by report_finding, or from list_task_findings). Use it to fetch a finding's full evidence before writing its report.",
		objSchema(map[string]any{
			"task_id": strParam("task id"),
			"id":      map[string]any{"type": "integer", "description": "exploration-graph node id (not an asset id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write/update the [detailed report] of an already-registered finding (full Markdown, overwriting the old content wholesale). finding_id is the id report_finding returned (the number in \"finding recorded: <id>\"). The report should include: finding overview, impact and harm, reproduction steps, evidence/PoC, remediation advice.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "target finding id (the id returned by report_finding)"},
			"report":           strParam("full detailed report, Markdown format"),
			"evidence_version": map[string]any{"type": "integer", "description": "the evidence version returned by get_finding_traffic; used to prevent the report from overwriting newer evidence changes"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // reuse the "number or numeric string" parser
			if nodeID <= 0 {
				return actool.Errorf("invalid finding_id"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("no finding record found for finding_id=%d (register it with report_finding first)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (which is made
	// for operating the platform). SeedTool takes effect on first insert; rows already
	// seeded in old DBs are backfilled by seedAutoDefaultBindings.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces unbind from worker's defaults (one-time)
	s.seedWorkerReadbackRebind()  // fix an old migration's mistaken removal: re-bind search_all_worker_traces/get_worker_trace/node_detail onto worker (one-time)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals prompt gains the "extract operation constraints" step → old DBs get one more default version (one-time)
	s.reseedMainAgentPrompt()         // mainagent prompt gains "after goals are met, add_intent asks whether to register a goal" (one-time)
	s.reseedPlannerPrompt()           // planner prompt: rewrite the "0 intent" justification + add quantified acceptance checks (one-time)
	s.reseedWorkerPrompt()            // worker prompt: add the evidence threshold for negative conclusions (one-time)
	s.seedReporterAgent()             // preset the "report writer" agent + tool bindings + finding trigger (one-time)
	s.upgradeReporterTriggerMessage() // old-DB migration: make reporter return evidence_version (one-time)
	s.seedFindingTrafficTools()       // add optional evidence params and read-only evidence tools, preserving user config
	s.seedFindingWorkflowTools()
	// Note: pentest's default tool bindings need no migration — on a fresh init
	// BuiltinToolSeeds already seeds list_assets/insert_assets/report_finding/list_findings/list_companies
	// together with pentest (the project has no old DBs yet, so no migration is done).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// Also refresh some built-in agent tools to the code defaults:
	//   - goal_met: the description seeded in old DBs misleads with "end this planning
	//     round", making the planner treat it as a way to "end an empty round" and
	//     wrongly judge the whole task complete just after it starts.
	//   - insert_assets: adds a related param (marks whether an asset is relevant to the
	//     current task and decides whether it enters coverage); SeedTool is first-insert
	//     only, so a schema already seeded in old DBs would otherwise miss this new param.
	//   - list_facts: switched to pagination with new limit/before/q params; an empty
	//     schema already seeded in old DBs would otherwise show "no parameters" on the
	//     tool-management page, and the model couldn't get descriptions for these params.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform tool schemas refreshed to code defaults (one-time)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] unbinding goal_met from planner failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt refreshes the goals goal-decomposer prompt to the [current code
// default] — the default body gained a "first extract operation constraints
// (set_constraints), then decompose goals" step, but SeedPromptIfEmpty is first-insert
// only, so an existing version 1 in old DBs misses it. This uses version management to
// [append a new version] and switch to it (ResetPromptToDefault); the old version stays
// in history, so a user who customized it can recover it from the version record. A
// settings flag guards it → done only once; bump this flag if the default changes again.
// Fresh DBs need no handling (SeedPromptIfEmpty already seeds the latest default).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so no migration is needed
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// A fresh DB's seedPrompts already seeded the latest default → the current version
	// already equals the code default, so there's no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] refreshing goals prompt to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] goals prompt appended a new default version (adds the extract-constraints step, one-time)")
}

// reseedMainAgentPrompt refreshes the mainagent prompt to the [current code default] —
// the default body gained a passage guiding that "after all goals are met, add_intent
// with a direct intent asks the human whether to register it as a formal goal", but
// SeedPromptIfEmpty is first-insert only, so existing versions in old DBs miss it. Uses
// version management to [append a new version] and switch to it (ResetPromptToDefault);
// the old version stays in history, so a user who customized it can recover it from the
// version record. A settings flag guards it → done only once. Fresh DBs need no handling
// (SeedPromptIfEmpty already seeds the latest default). Exactly isomorphic to
// reseedGoalsPrompt.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so no migration is needed
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// A fresh DB's seedPrompts already seeded the latest default → the current version
	// already equals the code default, so there's no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] refreshing mainagent prompt to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] mainagent prompt appended a new default version (adds the post-goal ask-to-register-goal, one-time)")
}

// reseedPlannerPrompt refreshes the planner prompt to the [current code default] — the
// default body was compactly refactored, downgrading "restraint" to dedup-only and adding
// "depth over coverage", "hard floor: must produce when the goal is unmet and no intent is
// running", and an upper bound on the negative-conclusion review. Bump the flag below
// (currently v2) whenever the default changes materially, so existing old DBs refresh
// again. SeedPromptIfEmpty is first-insert only, so existing versions in old DBs miss it,
// hence version management [appends a new version] and switches to it
// (ResetPromptToDefault); the old version stays in history, so a user who customized it
// can recover it from the version record. A settings flag guards it → done only once.
// Fresh DBs need no handling (SeedPromptIfEmpty already seeds the latest default).
// Exactly isomorphic to reseedGoalsPrompt.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so no migration is needed
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// A fresh DB's seedPrompts already seeded the latest default → the current version
	// already equals the code default, so there's no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] refreshing planner prompt to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] planner prompt appended a new default version (compact refactor + restraint downgraded to dedup + depth-first + negative-review upper bound, one-time)")
}

// reseedWorkerPrompt refreshes the worker prompt to the [current code default] — the
// default body's record_fact section deletes the whole sentence "for negative
// conclusions write observation + tentative reading" and decouples confidence
// (observed/inferred) from "whether the intent's means are exhausted" (these mislead the
// planner), while tightening the facts array into the rare exception of "fully
// independent of each other and impossible to merge". Bumps the flag to v3 so existing
// old DBs refresh again. SeedPromptIfEmpty is first-insert only, so existing versions in
// old DBs miss it, hence version management [appends a new version] and switches to it;
// the old version stays in history and can be recovered. A settings flag guards it → done
// only once. Fresh DBs need no handling. Exactly isomorphic to reseedGoalsPrompt.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so no migration is needed
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// A fresh DB's seedPrompts already seeded the latest default → the current version
	// already equals the code default, so there's no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] refreshing worker prompt to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] worker prompt appended a new default version (context section narrowed to list_assets/list_findings, dropping list_facts/node_detail/asset_neighbors, one-time)")
}

// reporterToolCallMessage must unconditionally require reading get_finding_traffic once
// before writing the report. That tool is read-only and "independent of the capture
// toggle", so it can read manually-bound evidence whether or not auto-binding is on. If
// this said "read only when auto-binding is enabled", then under the default off config
// the reporter wouldn't pass evidence_version, SetFindingReportVersionByNodeID would
// write -1 with legacy semantics, and the finding detail + Markdown export would
// permanently show "evidence changed, report pending update" with no UI entry to clear it.
const reporterToolCallMessage = "A finding was just registered by report_finding above. Read finding_id (the standalone finding record ID) and finding_node_id (the exploration node ID) from the returned JSON. " +
	"First use get_finding_traffic(finding_id) to read the current evidence list and its version (an empty list is normal, write the report as usual); " +
	"if the run instructions enable auto-binding, verify and associate this finding's traffic before reading. Use finding_node_id for node detail. " +
	"Finally call update_finding_report(finding_id=finding_node_id, report, evidence_version=<the version actually read>) to save; " +
	"evidence_version must be passed, otherwise the report will be permanently marked as pending update. Do not mix up the two ids."

// Legacy trigger message (0.3.8 and earlier). Only records still byte-identical to it are
// overwritten by the migration; user-modified ones are left as-is.
const reporterToolCallMessageV1 = "A finding was just registered by report_finding above. Take finding_id" +
	"(the number in the tool's return \"finding recorded: <id>\") and the task id from the trigger context, write the finding's detailed report per your role, " +
	"and finally call update_finding_report(finding_id, report) to save."

// upgradeReporterTriggerMessage refreshes old DBs' reporter trigger messages that are
// still the default wording to the new version. seedReporterAgent is guarded by
// reporter_agent_seed_v1 and only writes the trigger when creating the agent, so
// upgraded DBs don't get the new wording — the tool schema's evidence_version is added by
// seedFindingTrafficTools, but nothing tells the reporter to use it. One-time, and only
// overwrites unmodified wording.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] reading triggers failed: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // user-modified or not a finding trigger; leave it alone.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] upgrading trigger message failed: %v", err)
			return
		}
		log.Printf("[reporter] trigger message upgraded to read and return evidence_version")
	}
}

// seedReporterAgent presets a "report writer" custom agent (builtin=false, editable/
// deletable in the UI): binds update_finding_report + task-query tools, and attaches a
// "triggered whenever report_finding is called" trigger — so every registered finding
// wakes it to write a detailed report. One-time (settings flag guard): once the user
// deletes it, it is not recreated.
// Dependency: the orchestration tools are already seeded into the tools table by
// SeedTool above this function, so they can be bound.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key already taken (user created one by hand) — don't overwrite
	}
	a, err := s.m.pg.CreateAgent("reporter", "Report writer",
		"Detailed finding report writing: triggered automatically when a finding is discovered, fetches evidence and execution trace, then writes and writes back a Markdown report.")
	if err != nil {
		log.Printf("[reporter] creating agent failed: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seeding prompt failed: %v", err)
	}
	// Trigger run policy: parallel + none — one report per finding, multiple findings write concurrently.
	// merge must be none: otherwise (default all) a wave of findings gets merged into one run,
	// making parallelism pointless. maxParallel=5: at most 5 report sessions at once, avoiding
	// too many simultaneous LLM calls.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] setting trigger run policy failed: %v", err)
	}
	// Bind the tools it needs: write report + read evidence/execution trace/situational awareness.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] binding tools failed: %v", err)
	}
	// Trigger: fires whenever report_finding is called (the tool returns "finding recorded: <id>"
	// carrying finding_id; the task id is also in the trigger message).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] creating trigger failed: %v", err)
	}
	log.Printf("[reporter] seeded \"report writer\" agent + finding trigger")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] default binding of report_finding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] default binding of report_finding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] default binding of list_assets failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // default binding switch worker→planner
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] default binding of add_company_scope failed: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] unbinding add_company_scope failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] unbinding %s from worker failed: %v", k, err)
			return // on error, don't set the flag; retry on next startup
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: adds node_detail
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] re-binding look-back/detail tools failed: %v", err)
		return // on error, don't set the flag; retry on next startup
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: replace old asset tool names, add insert_assets/add_company_scope
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Asset tools: the Auto agent operating the platform often needs to view/register assets and manage company scopes.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
