package market

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
)

// A-648: the /admin overview warns while moderation capture needs the operator, so an exhausted lifetime pool is
// not visible only on the private moderation panel. The pool only grows: once it is spent, nothing new is captured
// until the budget is raised on that panel (A-654) or with the worker's --max-records / --max-reserved-bytes.
//
// One plain read of counters: no row lock, so the overview never waits on (or delays) admission and the worker,
// which serialise on the capacity row. It returns no row while capture is off.
//
// Waiting targets are the active markers (moderation_backfill_due) and the records whose capture found the capacity
// row busy (moderation_deferred, migration 148), each target once (A-662): a parked marker is an unflagged payment
// kept until it is flagged again, not a target waiting for budget (QA87-1), so a parked target counts only while it
// is also deferred. A deferred record was refused nothing, so it never raises the alert on its own and is not part of
// the overflow comparison.
//
// Overflow counters never reset, so a capacity overflow counts only if it was recorded at or after the oldest active
// marker's first refusal: a refusal notes the overflow and inserts its marker in one transaction
// (moderation_mark_missed), so the two times are equal for the marker it left, and an old overflow cannot turn later
// per-account throttle markers into a "raise the limits" alert (A-662). Rescans carry no time and are also the
// ordinary path past an account's marker bound, so they are counted but never tied to an overflow; an overflow that
// flags a rescan finds that account's own earlier active markers, and an exhausted pool alerts on its own.
//
// Known limit (QA87): when the marker table is full (max_backfill, 65536 markers by default) and none of its markers
// is active, as when it is full of parked payment markers, a throttle refusal (an account's three active jobs, the
// pending limit or its daily share) records a backfill_capacity overflow and flags the account for a rescan, which
// still captures the account's records later; with no active marker to tie that overflow to, the alert does not show
// until the lifetime pool runs low. Raising the budget would not empty the table. The private panel shows the full
// table under "Targets awaiting capture", the overflow and the accounts awaiting a rescan (docs/moderation-agent.md).
const moderationBudgetAlertSQL = `SELECT c.allocated_records,c.max_records,c.allocated_bytes,c.max_bytes,GREATEST(8,c.max_records*2/100),m.waiting,
 (SELECT count(*) FROM moderation_actor_allocations WHERE rescan),
 COALESCE(o.oldest<=(SELECT max(last_seen) FROM moderation_overflow WHERE reason IN ('retained_capacity','backfill_capacity') AND occurrences>0),false)
 FROM moderation_capacity c,(SELECT min(first_missed) AS oldest FROM moderation_backfill WHERE NOT parked) o,
 (SELECT count(*) AS waiting FROM (SELECT kind,target_id FROM moderation_backfill WHERE NOT parked UNION SELECT kind,target_id FROM moderation_deferred) u) m
 WHERE c.singleton AND EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true')`

// ModerationBudgetView is the overview's moderation capture alert: counts only, never an account, target, kind or
// time. Private is set on the private administrator connection; elsewhere the alert is one sentence without numbers.
// "Elsewhere" is a deployment without ADMIN_SOCKET: with it set the public listener refuses /admin (privateAdminRoute),
// and without it there is no private moderation panel. Capture can still be on there, because the separate
// moderation worker process enables it and does not know the web process's ADMIN_SOCKET (A-662).
type ModerationBudgetView struct {
	Used, Limit, Waiting, Rescans int64
	Private                       bool
}

func init() {
	registerLoader("admin", loadModerationBudgetAlert)
}

func loadModerationBudgetAlert(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	d.ModerationBudget = nil
	if a.preview || a.db == nil || d.AdminSection != "" || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	var used, maxRecords, usedBytes, maxBytes, share, waiting, rescans int64
	var overflow bool
	err := a.db.QueryRowContext(ctx, moderationBudgetAlertSQL).Scan(&used, &maxRecords, &usedBytes, &maxBytes, &share, &waiting, &rescans, &overflow)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Admission refuses a new allocation once either budget lacks a whole one (moderation_admit_job).
	remaining := max(0, min(maxRecords-used, (maxBytes-usedBytes)/moderationAllocationBytes))
	if remaining > 2*share && !overflow { // overflow implies an active marker waits
		return nil
	}
	d.ModerationBudget = &ModerationBudgetView{Used: used, Limit: used + remaining, Waiting: waiting, Rescans: rescans,
		// The condition that sets PrivateModerationAccess (moderation_admin.go): the counts stay on the private connection.
		Private: a.adminSocket != "" && privateAdminIngress(ctx)}
	return nil
}

// Text is the alert's fixed wording.
func (v ModerationBudgetView) Text() string {
	if !v.Private {
		return "Moderation capture needs attention. This deployment has no private administrator connection, so it has no " +
			"moderation panel: restart the moderation worker with higher --max-records and --max-reserved-bytes, or, on a Tor " +
			"deployment, set up the private administrator connection (ADMIN_SOCKET) to use the panel."
	}
	s := "Moderation capture needs attention: " + strconv.FormatInt(v.Used, 10) + " of " + strconv.FormatInt(v.Limit, 10) +
		" lifetime allocations used; " + strconv.FormatInt(v.Waiting, 10) + " target(s) waiting for capture"
	if v.Rescans > 0 {
		s += "; " + strconv.FormatInt(v.Rescans, 10) + " account(s) awaiting a rescan"
	}
	return s + ". Raise the limits under Moderation budget on the private moderation panel, or --max-records and " +
		"--max-reserved-bytes on the moderation worker (each allocation reserves " +
		strconv.FormatInt(moderationAllocationBytes, 10) + " bytes)."
}
