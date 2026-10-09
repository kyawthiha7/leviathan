package agent

import (
	"context"

	"leviathan/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**Finding traffic evidence (optional)**: when calling report_finding to report a finding, if you have HTTP requests/responses you have viewed and confirmed support the finding's conclusion, you may use traffic_refs to bind the real IDs in reproduction order; domain and time are only candidate filters and do not imply association. For non-HTTP findings such as TCP, when nothing was captured or there is no exact match, omit them or pass [], keep other verifiable evidence such as command output and logs in evidence, and explain why nothing was bound. Do not guess IDs, and do not re-probe solely to fill in traffic."

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
