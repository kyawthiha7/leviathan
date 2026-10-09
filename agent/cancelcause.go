package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "User paused the task",
		"The user paused the task via the task control endpoint (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; in-flight intents are returned to the frontier (open) and will be re-claimed and restarted from scratch after the task resumes")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestrator agent paused the task",
		"The orchestrator agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; in-flight intents are returned to the frontier (open) and re-executed after resume")
	AbortTaskDeleted = cause("task_deleted", "Task deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier has cancelled this task's running Planner, Workers, and main Agent. This run's results will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task's paused state",
		"The backend restored the task's paused state from state persisted in the database on startup. This run was cancelled; normally no Agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "Planner determined the task goal was met",
		"The planner determined the task goal was met and set the task to done, then cancelled the still-running Workers; these intents are marked stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Timeout settle-up drain grace exhausted",
		"After the task reached timeout the engine waited for running Workers to finish gracefully, but the 90-second drain grace period was still insufficient, so a hard cancel was performed; intents are marked exhausted, and facts and assets written during settle-up are preserved")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent",
		"The planner called kill_work to actively terminate this intent, usually meaning the direction went off track or there is no further value; the intent is marked stopped and will not be re-claimed automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused a running Worker. This invocation was cancelled and the intent becomes paused; all registered intents, facts, findings, and activity records are preserved and it restarts from scratch after resume")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this Worker intent",
		"The user deleted a running Worker. This invocation was cancelled; after the Worker exits its write region the server handles the intent according to the deletion mode the user chose. A soft delete only marks it deleted and preserves all outputs, while a hard delete cascades away the intent and any downstream nodes supported only by it")
	AbortWorkFinished = cause("work_finished", "Worker finished and released its context",
		"The Worker finished normally and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, a race occurred between cancellation and settle-up events")
	AbortPausedRaceGuard = cause("paused_race_guard", "Refused to start a run while paused",
		"When the task is paused the engine refuses to hand out a new execution context, preventing a race between claim and pause from letting a Worker start; claimed intents are returned to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this conversation turn",
		"The user clicked stop, actively aborting this main Agent or conversation Agent run. Activity already produced is preserved, and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task paused, aborted main Agent chat",
		"When the user paused the task, the running main Agent conversation was cancelled along with it. Activity already produced is preserved; this turn's messages will not be replayed automatically after the task resumes")
	AbortChatTurnFinished = cause("chat_turn_finished", "Chat turn finished and released context",
		"This conversation turn finished normally and the server is releasing that turn's context resources. This is not a run interruption; if it appears in an interruption message, a race occurred between cancellation and settle-up events")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running Agents are cancelled; leftover running intents are reset to open and re-executed after restart")
	AbortRunHardTimeout = cause("run_hard_timeout", "Single-run hard timeout backstop",
		"A single run exceeded its soft wall-clock budget plus extra grace, meaning a model request or some tool did not return for a long time, preventing normal turn-boundary settle-up. Focus on the last tool call that had not returned before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context reached its deadline",
			"The upstream context reached its deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceller did not attach a named cause",
			"The upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; register a cause in agent/cancelcause.go and wire it into that cancellation site", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
