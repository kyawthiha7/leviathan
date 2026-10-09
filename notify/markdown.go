package notify

import (
	"fmt"
	"strings"
)

// This file is the message rendering shared by the "Markdown-family" channels
// (DingTalk, WeCom). Feishu uses card JSON, Telegram uses HTML, and email uses
// HTML, each rendered in its own adapter.

// maxAssetsShown is the most assets listed in a message. A finding can be
// anchored to dozens of assets; listing them all would burst the message with no
// informational value - nobody reads the domains past the fourth in an IM.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. An IM
// message is a "go look at the details" nudge, not the report itself; the full
// content lives on the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message head (digest line + severity
// breakdown + possible truncation notice) and tail (platform link). When packing
// the whole message this is deducted from the budget, ensuring the head and tail
// are never cut - once they are, the reader cannot even tell "which batch this is
// and how many rows are still not shown".
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why it is mandatory: a finding's title, summary, class and asset display names
// all come from **untrusted sources** - the title and summary from model output
// (the model reads the response of the target under test), and the asset url from
// the full scanned URL (including the target-controlled query string). Without
// escaping, a finding titled
//
//	Login SQL injection\n[URGENT: click to verify account](http://attacker.tld)
//
// would render in the security engineer's DingTalk/Feishu as a **clickable
// external link**; and `![](http://attacker.tld/beacon)` would be fetched by the
// client at render time, effectively announcing "this finding has been viewed"
// and leaking the reader's IP. Even without malice, injected bold or blockquotes
// can push the serious findings below out of the fold.
//
// The escape set covers the characters in the heading/link/emphasis/list/
// blockquote/strikethrough categories that change structure or produce clickable
// elements. `\` must be handled first, or it would re-escape the backslashes
// added for everything else.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText flattens untrusted text to a single line and escapes it, for use
// in a markdown body. Flattening is the other half beyond escaping: a newline by
// itself can fabricate a new list item or blockquote, and escape characters
// cannot stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the IM platform's title bar / card
// title); the content is the **unescaped original**.
//
// Escaping is deliberately not done here: this title is shared by renderers in
// four contexts - the markdown body, Telegram's HTML, Feishu cards' plain_text,
// and the generic Webhook's JSON plus the email subject. Each context has
// different escaping rules (markdown escaping embedded in HTML leaves visible
// backslashes, and embedded in JSON pollutes the data), so escaping must be the
// responsibility of each output end, see writeItem / feishuItemLines /
// telegramEscape. Markdown escaping was once added in the shared function and
// visible backslashes like `\(1\)` turned up in Telegram messages.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability digest · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and the **number of
// items actually written**.
//
// The returned kept is the number of items genuinely delivered by this send; the
// caller uses it to mark only the first kept as delivered - items kept out by a
// channel's length limit must be left for the next batch, not marked successful
// along with the rest. This is exactly the source of "silent loss": the message
// was truncated, but the delivery record shows everything delivered, and nothing
// reveals that the second half was never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even if it is overlong (with the final truncation
		// as a backstop): partial information about one finding beats sending none.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all on the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the head of a digest message: the time window, the
// count and the severity breakdown. With these, someone receiving a digest can
// judge whether this batch needs immediate attention without opening the platform.
//
// items are the entries **actually packed**, total is the batch's full count. When
// the two differ it must say "how many more are in the next message" - otherwise
// the reader assumes the number in the message head is the whole thing, while the
// entries never sent simply do not exist in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new findings in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new findings**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (showing the first %d here; the remaining %d will continue in the next message)", len(items), extra)
	}
	// Give the breakdown by severity so the reader sees at a glance whether any
	// are critical. Only the entries **actually included here** are counted, keeping
	// "Critical 3" consistent with the entries countable below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single finding entry.
//
// prefix is the index used in a digest list; single=true renders the full version
// (with summary and backlink), while a digest list renders only a one-line
// summary - otherwise 50 digest entries become a long document.
//
// All externally-sourced content (title/class/assets/summary) goes through
// markdownText: flatten + escape. The backlink is built from the admin-configured
// public_base_url, is not untrusted content, and must be a clickable link, so it
// is output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: rendered on one line, with compressed assets and summary
		// following.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s -> %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
