package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"leviathan/db"
	"leviathan/notify"
)

// Global settings keys (stored in the settings key/value table, no table needed).
const (
	// settingNotifyEnabled is the master push switch. Default on: it is used to stop the
	// bleeding during maintenance, not as the feature's enabling condition — the real
	// enabling condition is "is any channel configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external base URL used to build finding-detail
	// deep links (e.g. https://leviathan.example.com). Empty means messages carry no
	// deep-link button. There is no reusable external-address setting in the project, so
	// this adds a new one.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest-mode interval (minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's poll interval. 3 seconds is the ceiling for
	// this engine's responsiveness and the main source of latency between a finding
	// being persisted and the message reaching IM.
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming a delivery. It must be
	// significantly longer than the worst-case cost of a single delivery (the notify
	// package's HTTP client timeout is 15 seconds), otherwise one row can be delivered
	// by two dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick caps how many events are fanned out per round, so enabling a
	// channel for the first time doesn't expand the entire historical backlog into
	// deliveries in one go.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest interval.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no
	// rate limit. It exists to keep "a channel set to unlimited + a sweep finding
	// thousands of findings" from dragging a single loop into a long block.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the max number of deliveries per channel per
	// round.
	//
	// This cap is derived backwards from the lease duration: claiming a row stamps it
	// with a lease (notifyLease = 3 minutes), and if the serially delivered rows in one
	// round are numerous enough that the worst-case cost exceeds the lease, the last few
	// rows expire the lease before they finish sending. Within a single process that is
	// harmless (Run is a single goroutine running serially, so ticks don't re-enter), but
	// with two processes sharing one database the peer reclaims the expired rows and
	// re-sends them, double-increments attempts, and marks a failure while the original
	// process is still delivering.
	//
	// The value: 3-minute lease / 30-second per-send timeout = 6 uses the lease up
	// exactly with zero headroom and must not be chosen; taking 5 leaves 30 seconds of
	// headroom on a worst-case 150 seconds. This relationship is pinned down by
	// TestNotifyTickBudgetFitsWithinLease — changing notifyLease, notifySendTimeout, or
	// this value will fail that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the
	// value of the constant above; their product must not exceed notifyLease, see
	// TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failed retries, indexed by the number of
// attempts already made. 3 chances (including the first) corresponds to
// db.MaxNotifyAttempts; the two must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for finding notifications.
//
// It runs as an independent goroutine alongside Scheduler (see server.New). It
// deliberately does not reuse Scheduler's tick: notification responsiveness (3 seconds)
// differs from the trigger business cadence, and their failures are mutually isolated —
// a stuck notification must not affect agent triggering.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu guards buckets. Channels are few and contention is low, so one mutex is
	// enough; a finer-grained structure isn't worth it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the token bucket for a single channel.
//
// A token bucket is used instead of a "count per minute then reset" sliding window
// because the latter has terrible boundary effects: send 20 at the end of the window
// and 20 the next instant, and the platform sees 40 in a second and rate-limits you; a
// token bucket refills at a constant rate, naturally avoiding such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx is done. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: fan out new events, then deliver due deliveries.
//
// Any step failing only logs and does not break the loop — a notification-system fault
// must never escalate into a process-level problem. Every tick is independent, so the
// next round retries naturally.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] fanning out events failed: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] reading channels failed: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket's unit is **number of messages** (equivalent to HTTP requests),
		// not number of findings. In realtime mode the two are the same (one finding, one
		// message); in digest mode a whole batch of findings becomes one message, so it
		// consumes a single token.
		//
		// Both modes ask the token bucket first, then claim up to the granted quota — the
		// order must not be reversed, otherwise deliveries blocked by rate limiting have
		// already consumed retry attempts.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns the token cost and the batch-size upper bound for a digest
// channel this round.
//
// The two return values are **two different dimensions**, which is exactly why this is
// its own function:
//
//   - tokens is the number of messages. A batch of findings becomes one message and one
//     HTTP request, so it is always 1. rate_per_min therefore still applies to digest
//     (at most that many digest messages per minute).
//   - claimLimit is how many findings this batch may hold at most. It is bounded only
//     by memory, independent of the request budget.
//
// There was once an attempt to make rate_per_min apply to digest by passing the
// per-round request budget (notifyMaxSendsPerChannelPerTick, derived from the lease)
// straight down as the batch size. The consequence was that a channel with
// rate_per_min=20 only refilled 1 token in a 3-second tick, so each digest message held
// only 1 finding — digest degenerated into "realtime push with digest wording", and
// readers got a stream of "1 new finding in the last 30 minutes" while
// db.MaxDigestBatchSize was forever unreachable.
//
// This symptom is hard to spot in end-to-end tests (existing cases pass a sufficiently
// large limit to stepDigest manually, bypassing the quota computation in step), so the
// decision is collected here and pinned directly by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers a channel's realtime deliveries, one message per
// finding.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] claiming realtime deliveries failed channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A render failure is a local data problem; retrying won't help.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message when the batch
// is due and sends it.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] checking digest batch failed channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] claiming digest batch failed channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot is broken and that didn't make it into the message must
	// be explicitly failed. Otherwise they stay outside included, in neither the message
	// nor the failure list — when sending succeeds their status is missed by the later
	// batch mark and they stay in sending forever until the lease expires and they are
	// repeatedly reclaimed.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot could not be parsed; this finding cannot be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] marking bad-snapshot delivery failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Hand only the ones that made it into the message to send: included[i] corresponds
	// strictly to msg.Items[i], and send relies on that mapping to apply "the channel
	// reported it accepted the first K" to the correct delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions state per the result.
//
// A batch of deliveries (dozens in digest mode) shares one send result: either it is
// delivered, or the whole batch retries. Retrying per-row is not done — the digest
// message is one, and resending part of it would scramble the batch semantics.
//
// The only exception is **segmentation caused by the channel length cap**: the channel
// reports it accepted only the first K, so row K+1 onward must be left for the next
// batch rather than being marked successful along with the rest. Otherwise the
// truncated findings end up neither in the message nor in the failure list — gone
// entirely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap a single delivery so one stuck channel doesn't hold up all the remaining channels this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The count a channel reports can't exceed the number of deliveries; if it really
			// happens, the render layer miscounted — treat it as all delivered and log the
			// issue, which is better than corrupting the records.
			log.Printf("[notify] channel reported %d delivered exceeding %d deliveries channel=%s, treating all as delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] marking delivered failed channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message hit the channel length cap: the rest go back to the queue at once
			// and the next tick continues. Uses DeferDeliveries rather than
			// RescheduleDeliveries — this is not a failure and must not consume retry budget
			// (claiming already optimistically incremented it, and that decrements it back).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message hit the channel length cap; only the first %d were delivered, the rest wait for the next batch", delivered)); err != nil {
				log.Printf("[notify] queuing segmented continuation failed channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel reported neither an error nor how many it delivered. Treat it as a
		// failure (take the backoff) so this delivery isn't repeatedly reclaimed and never
		// marked.
		err = fmt.Errorf("channel did not report a delivered count (delivered=%d)", delivered)
	}

	// Failure disposition is decided **per row**, not by taking the batch's maximum
	// attempt count.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts`, failing the whole
	// batch, but rows in a batch don't share the same attempt count: an old delivery
	// already retried twice (attempts=2) would drag brand-new deliveries in the same
	// batch (attempts=1) into failed — a new finding is lost permanently without a single
	// retry, exactly the opposite of the intent "don't let old rows sink new ones".
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] marking failed status errored channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] marking failed status errored channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Reschedule grouped by delay: there are only 3 backoff tiers, so the number of
	// groups is naturally tiny and there's no need for a separate UPDATE per row (that
	// would turn a 500-row batch into 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] rescheduling deliveries failed channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d exhausted=%d to-retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in all that are not in keep (compared by pointer
// identity). Used to find the deliveries that "didn't make it into the message" — they
// must be explicitly disposed of, not left in a grey zone.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt fetches the channel implementation and parses its config.
// ok=false means the kind is not registered; the delivery should be failed outright
// rather than retried forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// Give an empty map when config parsing fails: the channel's own Validate will
		// report "which field is missing", and that error guides the user to a fix better
		// than a JSON parse error would.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single-finding message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders the digest message. Snapshots are parsed one by one — a broken
// one only skips that row, it doesn't sink the whole batch.
//
// The returned included and msg.Items are **strictly one-to-one** (i-th delivery ↔
// i-th item). This correspondence is a hard requirement: the caller uses "the channel
// reported it accepted the first K" to mark the first K deliveries as delivered. If a
// broken snapshot is skipped here without removing the skipped delivery from included,
// the indices shift — a broken item that should fail would be marked delivered while a
// good item is misjudged as undelivered. The broken ones are explicitly failed by the
// caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A broken snapshot goes into neither the message nor included — its disposition
			// is the caller's responsibility (explicitly mark it failed rather than letting it
			// slip through among the "delivered").
			log.Printf("[notify] skipping unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch are unparseable", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a to-be-pushed item, resolving asset names
// and the detail deep link along the way.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// An asset-name resolution failure must not block the push: missing names is far
		// lighter than a missing notification, just one fewer asset line in the message.
		log.Printf("[notify] resolving asset names failed finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// For the detail page route see web/src/app/(main)/function/findings/detail/page.tsx,
		// which reads the finding id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens removes **at most want** tokens from a channel's token bucket and returns
// the number actually taken.
//
// One token = one message (one HTTP request). In realtime mode the caller passes
// however many it wants; in digest mode a whole batch of findings is a single message,
// so it passes 1.
//
// The bucket capacity is the channel's per-minute limit, refilled at a constant rate.
// ratePerMin<=0 means unlimited, returning a finite but sufficiently large value so a
// single round isn't dragged down by an unbounded backlog.
//
// The want cap is necessary: without it we could only drain the whole bucket, while the
// caller also has its own per-round cap, so extra tokens taken are neither usable nor
// survive until the next refill — accumulated burst capacity becomes forever
// unreachable, and even "no pending deliveries this round" would be charged.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill by the real elapsed time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: token counts accumulate as floats, so
	// refilling in two steps can yield 0.9999999999 from 0.5 + 0.5, which a direct int()
	// would truncate to 0 — a mathematically full bucket yields no token. 1e-9 is far
	// smaller than one token and won't mask a real shortfall.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external base URL for deep links, with the trailing slash
// removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest interval, falling back to the default when invalid
// or unset.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of the event corresponding to a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("delivery %d has an empty event snapshot", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("parsing the event snapshot for delivery %d failed: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event kind follows the event row; the copy in the snapshot may have been
		// written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
