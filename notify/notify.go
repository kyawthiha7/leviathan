// Package notify implements the IM / email delivery adapter layer for
// vulnerability findings.
//
// Layering: this package is a **leaf package**, depending only on the
// standard library. It knows nothing about the database or the server
// package. Channel config comes in as map[string]any (corresponding to the
// notification_channels.config JSONB column), and the content to deliver
// comes in as a Message. The benefit of this separation: the genuinely
// error-prone parts—signature computation, UTF-8 truncation, filter
// matching—can be unit-tested without PostgreSQL, and the host only needs
// to handle orchestration on the server side.
//
// Concurrency contract: Channel implementations must be **stateless**. The
// same Channel instance is shared concurrently across multiple channel
// configs (even multiple bot instances of the same channel), so all
// credentials must come in through the cfg parameter—caching things like a
// webhook URL into the implementation's own fields is not allowed.
package notify

// Channel type identifiers. These values are also the valid set for
// notification_channels.kind, enforced by a whitelist on the server side
// (same approach as findings.status—no DB CHECK, to make adding channels
// easier later).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (incl. Lark) custom bot
	KindWeCom    = "wecom"    // WeCom (Enterprise WeChat) group bot
	KindWebhook  = "webhook"  // Generic webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event kinds, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for an empty kind in config.
const InitKind = KindDingTalk

// severityRank maps a vulnerability severity to a comparable ordinal.
// Unknown severities return 0, so any min_severity setting blocks unknown
// severities too—when in doubt, don't push, to avoid flooding on
// misclassified findings.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the severity's ordinal; unknown severities return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the severity name with an emoji, used for message
// titles and card colors. Unknown severities are echoed back as-is rather
// than guessed at.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel translates a disposition status into a display label, used
// for status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In Progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False Positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk Accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min
// means no threshold is set, so everything passes. Note that an unknown
// severity has ordinal 0 and is rejected by any non-empty min (see the
// severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
