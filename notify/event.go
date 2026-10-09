package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column.
// The writer is the db layer's finding-persistence transaction; the readers are
// the server layer's delivery engine and filter matching. It is defined in this
// package because it is the payload of the "notification domain": db only
// serializes it and does not understand the field meanings.
//
// Why store redundant finding fields rather than re-querying at render time:
// findings get renamed, re-graded and re-statused afterward, but a push should
// reflect the conclusion **as of the moment it happened** - re-querying would
// yield the dangerous misdirection of "changed to low afterward". Additionally
// fan-out and rendering therefore need not JOIN the findings/tasks/assets tables.
type Snapshot struct {
	// Event kind: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is one finding to be pushed, for a channel to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are the resolved asset display names (e.g. domain/IP). Filled by the
	// server layer - this package does not touch the database and cannot get the
	// names.
	Assets []string
	// DetailURL is the finding's detail backlink; empty means public_base_url is
	// unset and it is omitted at render time.
	DetailURL string
	// Specific to status-change events; when both are non-empty it renders as
	// "Open -> Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: prefer the human-facing name, fall back
// to the vulnclass, and use a placeholder when both are empty - never output an
// empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed finding)"
}

// Message is the full content of one channel send.
type Message struct {
	// Length 1 for a single push; a whole batch for a digest push. An empty
	// slice is illegal; the caller must guarantee at least one.
	Items []Item
	// When Batch=true it renders as a digest message (different title, with the
	// time window and count).
	Batch bool
	// WindowMinutes is the digest period (minutes), used for the "last N minutes"
	// wording only when Batch=true. It is passed in explicitly from config rather
	// than computed with time.Since at render time: keeping rendering deterministic
	// makes it testable.
	WindowMinutes int
	// HomeURL is the platform panel address (global public_base_url); empty means
	// no panel entry.
	HomeURL string
}
