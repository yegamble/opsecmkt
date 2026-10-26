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
// until the worker's --max-records / --max-reserved-bytes are raised.
//
// One plain read of counters: no row lock, so the overview never waits on (or delays) admission and the worker,
// which serialise on the capacity row. It returns no row while capture is off.
//
// Waiting targets are the active markers only (moderation_backfill_due): a parked marker is an unflagged payment
// kept until it is flagged again, not a target waiting for budget (QA87-1). Overflow counters never reset, so a
// capacity overflow counts only if it was recorded at or after the oldest active marker's first refusal: a refusal
// notes the overflow and inserts its marker in one transaction (moderation_mark_missed), so the two times are equal
// for the marker it left, and an old overflow cannot turn later per-account throttle markers into a "raise the
// limits" alert (A-662). Rescans carry no time and are also the ordinary path past an account's marker bound, so
// they are counted but never tied to an overflow; an overflow that flags a rescan finds that account's own earlier
// active markers, and an exhausted pool alerts on its own.
const moderationBudgetAlertSQL = `SELECT c.allocated_records,c.max_records,c.allocated_bytes,c.max_bytes,GREATEST(8,c.max_records*2/100),m.waiting,
 (SELECT count(*) FROM moderation_actor_allocations WHERE rescan),
 COALESCE(m.oldest<=(SELECT max(last_seen) FROM moderation_overflow WHERE reason IN ('retained_capacity','backfill_capacity') AND occurrences>0),false)
 FROM moderation_capacity c,(SELECT count(*) AS waiting,min(first_missed) AS oldest FROM moderation_backfill WHERE NOT parked) m
 WHERE c.singleton AND EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true')`

// ModerationBudgetView is the overview's moderation capture alert: counts only, never an account, target, kind or
// time. Private is set on the private administrator connection; elsewhere the alert is one sentence without numbers.
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
		return "Moderation capture needs attention: open the private moderation panel on the private administrator connection."
	}
	s := "Moderation capture needs attention: " + strconv.FormatInt(v.Used, 10) + " of " + strconv.FormatInt(v.Limit, 10) +
		" lifetime allocations used; " + strconv.FormatInt(v.Waiting, 10) + " target(s) waiting for capture"
	if v.Rescans > 0 {
		s += "; " + strconv.FormatInt(v.Rescans, 10) + " account(s) awaiting a rescan"
	}
	return s + ". Raise --max-records and --max-reserved-bytes on the moderation worker (each allocation reserves " +
		strconv.FormatInt(moderationAllocationBytes, 10) + " bytes)."
}
