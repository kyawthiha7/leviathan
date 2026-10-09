package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"leviathan/notify"
)

// This file is the channel configuration and event layer for IM push
// notifications. For claiming delivery tasks and state transitions, see
// db/notification_delivery.go.
//
// Two invariants that must be preserved whenever this file is changed:
//
//  1. The transaction that writes a finding (RecordFindingTx) only calls
//     InsertNotificationEventTx to do a single blind insert — it never reads
//     any notification-related table and never does filter matching. Any read
//     introduced here could let a user's misconfigured filter corrupt or even
//     abort the finding-write transaction.
//  2. Filter matching never errors: a malformed config is always treated as
//     a "match" (see notify.Match). Better to over-notify than to miss a
//     notification.

// ErrNotificationChannelNotFound indicates the channel does not exist.
var ErrNotificationChannelNotFound = errors.New("notification channel not found")

// Delivery states.
const (
	NotifyStatePending = "pending" // awaiting send
	NotifyStateSending = "sending" // claimed by a dispatcher, lease not yet expired
	NotifyStateSent    = "sent"    // delivered
	NotifyStateFailed  = "failed"  // retries exhausted or permanently failed; can be resent manually
	NotifyStateSkipped = "skipped" // channel disabled, will not be sent
)

// Push modes.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode validates a push mode against the allow-list (same
// rationale as findings.status: no DB CHECK, to keep it easy to extend later).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel is the configuration for one channel instance. Config
// and Filter are kept as raw JSON — parsing is delegated to the notify
// package, since the db layer doesn't understand their field semantics.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled is a pointer so we can distinguish "field not provided" from
	// "explicitly set to false" — the frontend's toggle control only submits
	// fields that were actually changed.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled reports whether the channel is enabled; when Enabled is nil
// (not loaded), it is treated as enabled.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is a single event fact.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels returns all channel instances, with enabled ones
// first and ties broken by id. The ordering is done in SQL so the UI and the
// dispatcher see the same stable order.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID fetches a single channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel creates or updates a channel.
//
// On update, only fields the caller explicitly provided (non-nil / non-empty)
// are overwritten, so the frontend's edit drawer can submit a partial update
// without having to echo back config fields it doesn't display — echoing
// them back would risk a masked placeholder value clobbering the real secret.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// Deliberately **not** massaging 0 here: 0 is a valid config meaning
	// "unlimited".
	//
	// This used to be written as `if c.RatePerMin <= 0 { c.RatePerMin =
	// <default> }`, intended as "fill in a safe default when unspecified" —
	// but that also swallowed "explicitly set to 0". The docs, the UI hints,
	// and takeTokens all treat 0 as unlimited; only this one spot silently
	// rewrote it to 20 (DingTalk/WeCom/Telegram) or 100 (Feishu), so an
	// operator who thought they'd disabled rate limiting was actually still
	// capped at 20/min with no indication of it.
	//
	// Only the caller can tell "unspecified" apart from "explicitly 0" (a
	// field missing from the request body vs. explicitly set to 0), so the
	// default is filled in by the server layer when the field is absent —
	// see notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("rate limit cannot be negative")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled toggles a channel on or off.
//
// When disabling a channel, its not-yet-sent deliveries are also marked
// skipped: otherwise re-enabling it later would suddenly dump out a batch of
// stale findings that piled up while it was disabled, which have gone stale
// and could easily be mistaken for new ones.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "channel disabled", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel deletes a channel. Its delivery history cascades
// via the foreign key (with the channel config gone, the history has
// nothing left to interpret it against).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx **makes a best effort** to write a push event
// inside the caller's transaction.
//
// This is the only notification-related change on the finding-write path: a
// single INSERT, with no reads of any table, no knowledge of channels, and no
// filter matching. Committing the transaction guarantees "finding persisted"
// and "push task exists" are atomically consistent — there's no window where
// the commit succeeds but the event never got queued and the message is lost
// forever.
//
// Two key design points, neither accidental:
//
//  1. **Why a SAVEPOINT**: in PostgreSQL, any statement erroring inside a
//     transaction puts the whole transaction into an aborted state, after
//     which every subsequent statement (including COMMIT) fails. So "ignore
//     this INSERT's error and let the caller keep committing" is impossible
//     in PG unless a savepoint isolates the error to this one statement.
//     Without a savepoint, the only option left is rolling back the entire
//     transaction.
//
//  2. **Why rolling back the whole transaction would be wrong**: push
//     notifications are a convenience feature; the finding record is the
//     actual product. A problem in the notification tables (an unmigrated
//     old schema, a transient disk fault) shouldn't prevent a critical
//     finding from being persisted. So the error is isolated here, logged,
//     and false is returned so the finding write commits as normal — at the
//     cost of losing this one notification. Returning bool instead of error
//     is deliberate: the caller should never treat this as an error that
//     affects the success of the write.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] failed to marshal push event finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] failed to create savepoint finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] failed to write push event finding=%d (finding record unaffected): %v", findingID, err)
		// Roll back to the savepoint to rescue the transaction from the
		// aborted state.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] failed to roll back to savepoint finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release the savepoint so long transactions don't accumulate unused
	// savepoints.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is the standalone-transaction version of
// InsertNotificationEventTx, for call sites not already inside an existing
// transaction (e.g. a channel's "send test message" feature, which has no
// real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal notification event snapshot: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands not-yet-dispatched finding events into
// delivery tasks for each currently enabled channel, and returns the number
// of events processed and deliveries created this round.
//
// The whole round runs in one transaction: events are claimed with FOR
// UPDATE SKIP LOCKED, so multiple processes running concurrently each claim
// different rows (the same technique is used for claiming jobs in the
// archive queue elsewhere in this project — see completeNextArchiveJob in
// db/task_archives.go).
//
// Filter matching is deliberately done in Go rather than SQL: a channel's
// filter conditions are a JSONB blob of optional fields, and expressing the
// six possible combinations as SQL would make the query unmaintainable,
// whereas the number of channels is "a handful of hand-configured entries" —
// loading them all and comparing in memory is both faster and easier to
// test.
//
// Events that match no channel are still marked fanned_out — otherwise they
// would stay in the pending set forever, rescanned every tick.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// The snapshot is written by us, so it should in theory always
		// parse; a parse failure doesn't block the delivery pipeline, but
		// this event will end up skipped by every channel that has filter
		// conditions, since all its fields are empty — better to miss one
		// notification than to let one bad row jam the whole queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind is taken from the row, not the snapshot: the copy inside the
		// snapshot is for rendering and may have been written by an older
		// version.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// Mark this round's events as dispatched. Events matching no channel are
	// marked too (see function comment).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx fetches the enabled channels inside a
// transaction. The count is small, so no pagination and no caching —
// caching would introduce the extra timing question of "when does a config
// change take effect."
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames resolves asset ids into short display names for
// use in push messages.
//
// The return order matches the input order, and its length may be shorter
// than the input (missing ids are skipped). Preserving input order keeps
// the asset ordering stable across multiple deliveries of the same
// finding's message — otherwise a retried message with a different asset
// order could be misread as "the assets changed."
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName picks the most identifiable label for an asset type.
// Falls back to an empty string, leaving it to the caller to decide how to
// render "an asset with no resolvable name" — this function never fabricates
// a placeholder, since noise like "Asset#42" slipping into a push message
// could be mistaken by the reader for a real domain.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify updates a finding's disposition status and, in
// the same transaction, records a status-change push event.
//
// Returns from=the status before the change; found=whether the finding
// exists; notified=whether the event was successfully recorded.
//
// Three deliberate behaviors:
//   - No event is recorded when the status doesn't actually change. A
//     frontend drawer resubmitting the same value, or an automation script
//     replaying idempotently, shouldn't produce push noise.
//   - When the finding doesn't exist, returns found=false with no writes at
//     all, leaving it to the caller to translate that into a 404.
//   - A failure to record the event doesn't affect the status update (see
//     the savepoint explanation in RecordNotificationEventTx), so when
//     notified=false the status change still succeeded and the caller
//     should not treat it as an error.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx updates a finding's status and records a status-change
// push event, inside **the caller's transaction**.
//
// This is pulled out as a transaction-level function so every status-change
// path shares the same semantics — previously only patchFinding went through
// the notifying version, while the `UPDATE findings SET status=...` in
// finding_retests (triggered when a retest concludes "fixed") wrote directly
// to the table. As a result, channels configured with `on_status_change`
// never received a push for that kind of transition: the status quietly
// changed in the UI, and operators wouldn't notice until they opened the
// platform.
//
// Returns from=the status before the change, found=whether the finding
// exists, changed=whether the status actually changed, and
// notified=whether the event was successfully recorded (a failure to record
// doesn't affect the status update — see RecordNotificationEventTx).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// No event is recorded when the status hasn't actually changed:
		// resubmitting the same value or an idempotent replay shouldn't
		// produce push noise.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats is the overview counts shown at the top of the
// notifications page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // age in milliseconds of the oldest pending delivery
}

// NotificationStatsSnapshot summarizes the health of the notification
// system. BacklogAgeMS is the most direct signal of "is push stuck" — far
// more useful than the pending count alone, since a backlog of 3 items
// could mean anywhere from 3 seconds to 3 hours.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
