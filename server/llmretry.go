package server

import (
	"time"

	"leviathan/agent"
	"leviathan/db"
)

// Server-side resolution of the retry policy, see docs/llm-retry-design.md. Of the five
// layers:
//   - connect / empty response / same-provider safety window follow the endpoint, so
//     each LLM profile may override the global default (a blank field on the profile
//     inherits the global; if the global is unset too, the built-in default applies);
//   - circuit breaker / intent replay are process-level and have a single global copy.
//
// The global policy reads one settings row from the DB, and every caller is on a
// low-frequency path (building a provider, finishing a work, saving a profile), so it
// is not worth adding a cache layer; the breaker parameters are the exception — they
// are read on every failure path, so applyRetryPolicy pushes them into the Registry.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// The attempt counts keep their raw semantics here (0=default / negative=off):
		// the SDK's MaxRetries / EmptyResponseRetries are exactly isomorphic, so we
		// just hand them over for it to resolve.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// The default circuit-breaker (pool cooldown) values match the ones built into
// llmpool — we only override when the user configured a value.
// The intent-replay defaults live in engine.go's modelErrorRetries /
// modelErrorRetryBackoff.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob — "empty
// response retries": the two are two means to the same end. The SDK layer handles
// "not a single content block", by resending the same request as-is; this one handles
// "only thinking, no text and no tools", by appending an instruction so the model
// carries on with the thinking it already has (resending as-is is pointless for this
// kind of idle turn, which is shaped by the context). The emptiness check differs
// because the SDK goes by "did it ever yield an event", and thinking deltas are
// themselves events — but when a user configures "how many times to retry an empty
// response" they mean "run again if the model produced no substance", and sharing one
// count across both layers matches that mental model.
//
// Reads the global policy rather than a profile's override: a run may fail over to a
// different profile mid-flight, and this is the total throttle for the whole intent,
// which should not change with the endpoint. Semantics are isomorphic to the SDK's
// emptyRetries(): 0 = default defaultEmptyTurnNudges; -1 (negative) = turn empty-turn
// continuation off; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
