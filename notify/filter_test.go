package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Malformed JSON, empty input, fields of the wrong type—all of these
	// must degrade to a zero-value Filter, i.e. "no filtering". This
	// invariant is where "push more, never miss" lands: if this ever
	// changed to error out or partially parse, a single typo in the user's
	// config would silently drop every critical notification.
	cases := []struct {
		name string
		raw  string
	}{
		{"empty input", ""},
		{"invalid JSON", `{not json`},
		{"truncated JSON", `{"min_severity":`},
		{"type mismatch", `{"min_severity": 123, "task_ids": "abc"}`},
		{"top level is an array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("malformed config should degrade to a zero-value Filter, got %+v", f)
			}
			// A zero-value Filter must match every event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("a zero-value Filter should match all events")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// An unknown severity has ordinal 0, so it should be blocked by any
		// non-empty threshold (when in doubt, don't push).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: expected %v, got %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL Injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"empty scope = unrestricted", Filter{}, true},
		{"task match", Filter{TaskIDs: []int64{7}}, true},
		{"task no match", Filter{TaskIDs: []int64{8}}, false},
		{"multiple tasks including a match", Filter{TaskIDs: []int64{8, 7}}, true},
		{"asset intersection", Filter{AssetIDs: []int64{20, 99}}, true},
		{"asset no intersection", Filter{AssetIDs: []int64{99}}, false},
		{"task and asset both match", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"task matches but asset doesn't", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include empty = accept all", Filter{}, "Any Type", true},
		{"include matches", Filter{VulnClassInclude: []string{"SQL"}}, "SQL Injection", true},
		{"include no match", Filter{VulnClassInclude: []string{"Command Execution"}}, "SQL Injection", false},
		{"include matches any of several words", Filter{VulnClassInclude: []string{"Command Execution", "SQL"}}, "SQL Injection", true},
		{"case insensitive", Filter{VulnClassInclude: []string{"sql"}}, "SQL Injection", true},
		{"exclude match is excluded", Filter{VulnClassExclude: []string{"Information Disclosure"}}, "Information Disclosure", false},
		{"exclude no match passes through", Filter{VulnClassExclude: []string{"Information Disclosure"}}, "SQL Injection", true},
		// Exclusion takes priority over inclusion: a simultaneous match
		// should be disqualified.
		{"exclusion takes priority over inclusion", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"Injection"},
		}, "SQL Injection", false},
		// Pure-whitespace keywords should be ignored, otherwise they'd
		// degrade into "matches any string containing a space".
		{"whitespace-only keywords are ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL Injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Off by default: when most people say "push vulnerabilities", they
	// mean new findings, not a running log of status changes.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("a status-change event should be skipped when not opted in")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("a status-change event should match once on_status_change is enabled")
	}
	// Creation events are unaffected by on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("a creation event shouldn't depend on on_status_change")
	}
}
