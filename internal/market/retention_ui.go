package market

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

type RetentionView struct {
	Enabled                    bool
	LastRun, DeletionRequested string
	MaintenanceError           string
	Holds                      []RetentionHoldView
	AccountExceptions          []RetentionAccountExceptionView
	EscrowExceptions           []RetentionEscrowException
	WalletCleanup              []RetentionWalletCleanup
}

type RetentionHoldView struct {
	ID, Scope, RecordID, Authority, Reason, Owner, Readers, ReleaseCondition, Review string
	Overdue                                                                          bool
}

type RetentionAccountExceptionView struct {
	UserID, Reason, Owner, Review string
	Overdue                       bool
}

func init() {
	for _, page := range []string{"account", "admin", "canary", "messages", "register", "order"} {
		registerLoader(page, retentionLoader)
	}
	off := func(a *App) bool { return a.retention == nil }
	registerAction("/account/delete", actionSpec{Roles: []string{"buyer", "vendor", "moderator", "admin"}, OwnTx: true, ConfirmedFormNonce: true, Off: off, Run: retentionDeleteAction})
	registerAction("/admin/retention/hold", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Off: off, Run: retentionHoldAction})
	registerAction("/admin/retention/release", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Off: off, Run: retentionReleaseAction})
	registerAction("/admin/retention/review", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Off: off, Run: retentionReviewAction})
	registerAction("/admin/retention/account-review", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Off: off, Run: retentionAccountReviewAction})
}

func retentionLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	v := &RetentionView{Enabled: a.retention != nil}
	v.MaintenanceError = a.retentionMaintenanceError()
	d.Retention = v
	if !v.Enabled {
		return nil
	}
	err := a.db.QueryRowContext(ctx, `SELECT to_char(completed,'YYYY-MM-DD HH24:MI "UTC"') FROM retention_runs ORDER BY id DESC LIMIT 1`).Scan(&v.LastRun)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if d.User != nil && d.Page == "account" {
		if err = a.db.QueryRowContext(ctx, `SELECT COALESCE(to_char(deletion_requested_at,'YYYY-MM-DD HH24:MI "UTC"'),'') FROM users WHERE id=$1`, d.User.ID).Scan(&v.DeletionRequested); err != nil {
			return err
		}
	}
	if d.User == nil || d.User.Role != "admin" || d.Page != "admin" {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id,scope,record_id,authority,reason,owner,readers,release_condition,
	 to_char(review_at,'YYYY-MM-DD HH24:MI "UTC"'),review_at<=now() FROM retention_holds WHERE released_at IS NULL ORDER BY review_at,id LIMIT 100`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var h RetentionHoldView
		if err = rows.Scan(&h.ID, &h.Scope, &h.RecordID, &h.Authority, &h.Reason, &h.Owner, &h.Readers, &h.ReleaseCondition, &h.Review, &h.Overdue); err != nil {
			rows.Close()
			return err
		}
		v.Holds = append(v.Holds, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	accountRows, err := a.db.QueryContext(ctx, `SELECT user_id,reason,owner,to_char(review_due,'YYYY-MM-DD HH24:MI "UTC"'),review_due<=now() FROM retention_account_exceptions WHERE resolved_at IS NULL ORDER BY review_due,user_id LIMIT 100`)
	if err != nil {
		return err
	}
	for accountRows.Next() {
		var item RetentionAccountExceptionView
		if err = accountRows.Scan(&item.UserID, &item.Reason, &item.Owner, &item.Review, &item.Overdue); err != nil {
			accountRows.Close()
			return err
		}
		v.AccountExceptions = append(v.AccountExceptions, item)
	}
	err = accountRows.Err()
	accountRows.Close()
	if err != nil {
		return err
	}
	v.EscrowExceptions, err = LoadRetentionEscrowExceptions(ctx, a.db, time.Now().UTC(), 100)
	if err != nil {
		return err
	}
	v.WalletCleanup, err = LoadRetentionWalletCleanup(ctx, a.db, time.Now().UTC(), 100)
	return err
}

// The external operation is committed only after the application's confirmed
// transaction and audit commit. An ambiguous failure remains a startup gate.
func retentionConfirmedMutation(c *actionCtx, run func(*actionCtx) (string, actionResult, error)) (actionResult, error) {
	var op string
	res, err := confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		var result actionResult
		var runErr error
		op, result, runErr = run(c)
		return result, runErr
	})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Ctx()), 5*time.Second)
	defer cancel()
	if err != nil {
		if abortErr := c.A.abortRetentionOperation(ctx, op); abortErr != nil {
			return actionResult{}, errors.Join(err, abortErr)
		}
		return actionResult{}, err
	}
	if err = c.A.finalizeRetentionOperation(ctx, op); err != nil {
		// The prepared external intent and database commit marker are durable.
		// A later sweep reconciles them; do not report an accepted request as failed.
		log.Print("retention operation committed; external finalization pending maintenance")
	}
	return res, nil
}

func retentionDeleteAction(c *actionCtx) (actionResult, error) {
	if c.Form.Get("confirm_delete") != "yes" {
		return actionResult{}, fail(400, "Confirm that you have saved the records you need before requesting account deletion.")
	}
	return retentionConfirmedMutation(c, func(c *actionCtx) (string, actionResult, error) {
		if c.User.Role == "admin" {
			var others int
			if err := c.Tx.QueryRowContext(c.Ctx(), `SELECT count(*) FROM users WHERE role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND password_hash<>'' AND deletion_requested_at IS NULL AND retention_restricted_at IS NULL AND id<>$1`, c.User.ID).Scan(&others); err != nil {
				return "", actionResult{}, err
			}
			if others == 0 {
				return "", actionResult{}, fail(409, "Create another active administrator before deleting this administrator account.")
			}
		}
		id, err := c.A.requestAccountDeletionTx(c.Ctx(), c.Tx, c.User.ID)
		return id, actionResult{Redirect: "/account", Notice: "retention-delete", Audit: "Requested account deletion"}, err
	})
}

func retentionHoldAction(c *actionCtx) (actionResult, error) {
	if c.Form.Get("confirm_hold") != "yes" {
		return actionResult{}, fail(400, "Confirm the preservation duty before registering a hold.")
	}
	now := time.Now().UTC()
	h := retentionHold{ID: strings.TrimSpace(c.Form.Get("hold_id")), Scope: strings.TrimSpace(c.Form.Get("scope")), RecordID: strings.TrimSpace(c.Form.Get("record_id")),
		Authority: strings.TrimSpace(c.Form.Get("authority")), Reason: strings.TrimSpace(c.Form.Get("reason")), Owner: c.User.ID,
		Readers: strings.TrimSpace(c.Form.Get("readers")), ReleaseCondition: strings.TrimSpace(c.Form.Get("release_condition")), BeganAt: now, ReviewAt: now.Add(30 * 24 * time.Hour)}
	if h.ID == "" {
		h.ID = randomToken()
	}
	if err := validateRetentionHold(h); err != nil {
		return actionResult{}, fail(400, "Enter a valid preservation scope, record identifier, authority, reason, authorized readers and release condition.")
	}
	return retentionConfirmedMutation(c, func(c *actionCtx) (string, actionResult, error) {
		id, err := c.A.setRetentionHoldTx(c.Ctx(), c.Tx, h)
		return id, actionResult{Redirect: "/admin/system", Notice: "retention-hold", Audit: "Registered or reviewed a scoped preservation hold"}, err
	})
}

func retentionReleaseAction(c *actionCtx) (actionResult, error) {
	if c.Form.Get("confirm_release") != "yes" {
		return actionResult{}, fail(400, "Confirm that the preservation duty has ended before releasing the hold.")
	}
	return retentionConfirmedMutation(c, func(c *actionCtx) (string, actionResult, error) {
		id, err := c.A.releaseRetentionHoldTx(c.Ctx(), c.Tx, c.Form.Get("hold_id"), c.User.ID)
		return id, actionResult{Redirect: "/admin/system", Notice: "retention-release", Audit: "Released a scoped preservation hold"}, err
	})
}

func retentionReviewAction(c *actionCtx) (actionResult, error) {
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		err := reviewRetentionEscrowExceptionTx(c.Ctx(), c.Tx, c.Form.Get("order_id"), time.Now().UTC())
		return actionResult{Redirect: "/admin/system", Notice: "retention-review", Audit: "Reviewed a financial retention blocker; funds remain protected"}, err
	})
}

func retentionAccountReviewAction(c *actionCtx) (actionResult, error) {
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		err := reviewRetentionAccountException(c.Ctx(), c.Tx, c.Form.Get("user_id"), c.User.ID)
		return actionResult{Redirect: "/admin/system", Notice: "retention-review", Audit: "Reviewed an account-closure exception; necessary access remains"}, err
	})
}
