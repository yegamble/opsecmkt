package market

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A-654: an administrator raises the lifetime moderation budget on the private moderation panel. A raise applies to
// admission at once (it reads the capacity row), so targets waiting for budget are captured on the worker's next
// pass without a restart. The panel only raises; lowering stays a host-only procedure (docs/moderation-agent.md).
const moderationBudgetPath = "/admin/moderation/budget"

const (
	moderationBudgetMaxRecords int64 = 1000000 // ConfigureModerationLimits' bounds
	moderationBudgetMaxMiB     int64 = 1 << 20 // 1 TiB
)

var moderationBudgetNumberPattern = regexp.MustCompile(`^[1-9][0-9]{0,6}$`)

func init() {
	registerAction(moderationBudgetPath, actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ConfirmedFormNonce: true, OneAtATime: true, ReceiptAuthorize: moderationReviewAuthorize,
		Run: moderationBudgetAction})
}

// moderationMiB is bytes in MiB, without a fraction when it is whole.
func moderationMiB(bytes int64) string {
	return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', -1, 64)
}

// MaxMiB and AllocatedMiB are the reserved-byte limit and its use, in MiB.
func (v ModerationCapacityView) MaxMiB() string       { return moderationMiB(v.MaxBytes) }
func (v ModerationCapacityView) AllocatedMiB() string { return moderationMiB(v.AllocatedBytes) }

// MaxMiBInput is the byte limit as the form's whole MiB, rounded up.
func (v ModerationCapacityView) MaxMiBInput() int64 {
	return min(moderationBudgetMaxMiB, (v.MaxBytes+(1<<20)-1)>>20)
}

// ByteAllocations is how many allocations the byte limit holds.
func (v ModerationCapacityView) ByteAllocations() int64 {
	return v.MaxBytes / moderationAllocationBytes
}

// moderationBudgetNumber parses a plain whole number from 1 to limit.
func moderationBudgetNumber(s string, limit int64) (int64, bool) {
	if !moderationBudgetNumberPattern.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 1 && n <= limit
}

func moderationBudgetAction(c *actionCtx) (actionResult, error) {
	if err := moderationReviewAuthorize(c); err != nil {
		return actionResult{}, err
	}
	invalid := fail(400, "Enter the maximum lifetime allocations (1 to 1000000) and the maximum reserved bytes in whole MiB (1 to 1048576). Nothing was changed.")
	allowed := []string{"csrf", formNonceField, "password", "code", "max_records", "max_reserved_mib"}
	for key, values := range c.Form {
		if !slices.Contains(allowed, key) || len(values) != 1 {
			return actionResult{}, invalid
		}
	}
	records, okRecords := moderationBudgetNumber(c.Form.Get("max_records"), moderationBudgetMaxRecords)
	mib, okBytes := moderationBudgetNumber(c.Form.Get("max_reserved_mib"), moderationBudgetMaxMiB)
	if !okRecords || !okBytes {
		return actionResult{}, invalid
	}
	res, err := confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		// The capacity row first, the only row taken after the administrator's own account row (migration 148): a
		// concurrent raise or worker restart waits here and then sees this one.
		if _, err := c.Tx.ExecContext(c.Ctx(), `SELECT 1 FROM moderation_capacity WHERE singleton FOR UPDATE`); err != nil {
			return actionResult{}, err
		}
		var cur ModerationLimits
		var usedRecords, usedBytes, pending int64
		if err := c.Tx.QueryRowContext(c.Ctx(), `SELECT max_pending,max_records,max_bytes,allocated_records,allocated_bytes,pending
		 FROM moderation_capacity WHERE singleton`).Scan(&cur.MaxPending, &cur.MaxRecords, &cur.MaxBytes, &usedRecords, &usedBytes, &pending); err != nil {
			return actionResult{}, err
		}
		next := ModerationLimits{MaxPending: cur.MaxPending, MaxRecords: records, MaxBytes: mib << 20}
		if next.MaxRecords < cur.MaxRecords || next.MaxBytes < cur.MaxBytes || next == cur {
			return actionResult{}, fail(409, "The moderation budget can only be raised here: enter at least the current limits ("+
				strconv.FormatInt(cur.MaxRecords, 10)+" allocations, "+moderationMiB(cur.MaxBytes)+" MiB) and raise at least one. "+
				"Lowering is a host-only procedure. Nothing was changed.")
		}
		err := ConfigureModerationLimits(c.Ctx(), c.Tx, next)
		if errors.Is(err, ErrModerationLimitsBelowUse) {
			return actionResult{}, fail(409, "These limits are below what is already in use ("+strconv.FormatInt(usedRecords, 10)+" allocations, "+
				moderationMiB(usedBytes)+" MiB reserved, "+strconv.FormatInt(pending, 10)+" active jobs). Enter higher values. Nothing was changed.")
		}
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/moderation", Audit: "Moderation budget raised from " + strconv.FormatInt(cur.MaxRecords, 10) +
			" allocations / " + moderationMiB(cur.MaxBytes) + " MiB to " + strconv.FormatInt(next.MaxRecords, 10) + " / " +
			moderationMiB(next.MaxBytes) + " MiB by administrator " + c.User.Handle}, nil
	})
	// Every refusal leaves the budget as it was, including a wrong password or authenticator code; say so.
	var he *httpError
	if errors.As(err, &he) && he.Render == nil && !strings.Contains(strings.ToLower(he.Msg), "nothing was changed") {
		refused := *he
		refused.Msg = strings.TrimSuffix(refused.Msg, ".") + ". Nothing was changed."
		return actionResult{}, &refused
	}
	return res, err
}
