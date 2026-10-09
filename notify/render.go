package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing the result is valid
// UTF-8 and does not split a character.
//
// Why cutting on character boundaries is mandatory: the WeCom group bot's markdown
// has a hard 4096-**byte** limit (not character count), and a Chinese character is
// 3 bytes. A raw byte slice would split a character in half, producing invalid
// UTF-8 - the platform either rejects the whole message or shows replacement
// boxes. The approach here is to back up from the budget position to the nearest
// rune start byte (utf8.RuneStart detects continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. An ellipsis is appended after truncation, unless max is
// too small to fit it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: drop it and truncate plainly, avoiding a
		// result that actually exceeds max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine flattens multi-line text to a single line: collapse all whitespace, then
// truncate by character count. Used for an IM message's title line - summaries
// often contain newlines, and stuffing them straight into a table/title breaks the
// layout. max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an
// ellipsis when cut. max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's measure: WeCom limits by
// bytes, Telegram by character count. Using the wrong measure does not error, it
// only cuts messages far shorter than expected (1 Chinese character = 3 bytes, so
// a byte cut at 4096 leaves only about 1365 characters), so both functions must be
// kept and chosen per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count and guarantees no
// half-finished tag.
//
// Truncating HTML by characters directly can cut out a broken tag like
// `<a href="htt`; the platform parser either errors and rejects the whole message,
// or swallows the following body as an attribute value. The approach here is to
// truncate by characters first, then check whether the tail has an unclosed `<`
// and back off to before it if so.
//
// Tag balancing (completing </b> and the like) is not done: Telegram's HTML parser
// auto-closes unclosed tags, and implementing balancing oneself would have to
// handle quotes in attributes, comments and self-closing tags, complexity out of
// proportion to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (no `>` after the last `<`), back
	// off to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut HTML entity (e.g. `&amp;` cut to `&amp`), back off too.
	// An entity fragment can make **the whole message** rejected by an
	// entity-only parser - an over-limit digest message is common enough that it is
	// not worth losing the entire notification over it.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items fit **completely** within the budget, for
// packing a digest message whole-item by whole-item.
//
// Why whole-item rather than rendering the entire thing then truncating:
// truncation makes the second half of the items vanish, yet their delivery records
// are still marked delivered - invisible in the message and in the delivery
// history, so the findings are simply gone. With whole-item packing, items that do
// not fit stay in the database for the next batch, and the kept the caller gets is
// the count genuinely delivered by this message.
//
// Parameters: maxSize<=0 means no limit; reserve is the amount reserved for the
// message head/tail; size measures (the measure differs per platform: WeCom/
// DingTalk by bytes, Telegram by character count - using the wrong measure does
// not error, it only compresses a Chinese message far below the limit); render
// turns the idx-th item into its actual text - length varies with content and
// cannot be estimated.
//
// Returns at least 1 (as long as items remain). A single extremely overlong item
// must still be sent, with the caller's final truncation as a backstop, otherwise
// one overlong finding would block the whole batch forever.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are packItemCount's two measures, named to avoid a bare
// func(s string) int closure at the call site, which would make it hard to see at
// a glance which measure is used.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list as one line of display text; past limit it
// omits the rest and notes the total. A finding can be anchored to dozens of
// assets, and listing them all would burst the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " (" + itoa(len(assets)) + " total)"
}

// itoa is a short alias for strconv.Itoa, used only to build display text,
// avoiding importing strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
