package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB
// column: a channel instance's filter criteria. All fields are optional,
// and the default is "no filtering"—this is exactly the fallback semantics
// for malformed config too; see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical);
	// empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs: an empty array means unrestricted; non-empty
	// requires the event to intersect with it.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude empty means accept everything; non-empty requires
	// vulnclass to match any one of these keywords.
	// VulnClassExclude excludes on a match against any keyword (exclusion
	// takes priority over inclusion).
	// Matching is case-insensitive substring—safer than regex: a user's
	// botched regex won't silently disable the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives
	// finding-status-change events (only meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses a channel's filter config.
//
// **Never returns an error.** This is a deliberate design choice: a
// malformed filter config always degrades to a zero-value Filter (= no
// filtering = matches everything), because for a vulnerability notification
// system, **sending one extra message is far better than silently dropping
// a critical one**. Turning a parse failure into "don't push" would give
// the user a channel that looks configured but actually pushes nothing—the
// worst possible failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure, f stays at its zero value, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (an
// empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the **constrained-value** fields of a filter config; call
// this when saving a channel.
//
// Why this must be caught on write: Match's handling of an unknown
// threshold is `rank >= 0`, which is always true—meaning a typo in
// min_severity ("hgih") makes the filter **silently degrade** into "push
// everything". This is consistent with the package's "push more, never
// miss" bias (nothing gets dropped), but the consequence is that the user
// thinks they've set up tiered delivery while actually flooding the group
// with every finding, with no sign anything is misconfigured. This kind of
// "silent degradation" is exactly what should be caught at the entry point.
//
// Note that Validate is only used on the **write** path. The read path
// still goes through ParseFilter's permissive semantics, so bad values
// already present in historical data don't make a channel unreadable
// entirely.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("invalid minimum severity %q; must be one of low / medium / high / critical, or empty for no threshold", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel with this
// filter.
//
// **Never returns an error**, for the same reason as ParseFilter: any
// internal anomaly is treated as a match. Evaluation order: event kind ->
// severity threshold -> task/asset scope -> vulnerability-class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are only received by channels that explicitly
	// opt in. Off by default, because most users expect "push" to mean
	// "a new finding was discovered", not a running log of every status
	// transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion takes priority: a match against any excluded keyword is
	// disqualifying, even if it also matched the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan is fine for small sets; both sides are on the order of
	// "a few dozen hand-picked by a human", so building a map would cost
	// more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
