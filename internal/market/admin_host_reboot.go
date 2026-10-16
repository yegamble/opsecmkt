package market

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"opsecmkt/internal/hostcontrol"
)

// hostRebootController grants only the host agent's fixed reboot capability.
// The app receives no root credentials, Docker socket or unlock material.
type hostRebootController interface {
	Status(context.Context) (hostcontrol.Status, error)
	RequestReboot(context.Context, hostcontrol.Request) (hostcontrol.Receipt, error)
}

func newHostRebootController(socket string) hostRebootController {
	return hostcontrol.NewClient(socket)
}

type HostControlView struct {
	Ready, MFARequired bool
	Reason             string
	Latest             *HostRebootRequestView
	AgentReceipt       *HostRebootRequestView
}

type HostRebootRequestView struct {
	ID, State, RequestedAt, AcceptedAt, RebootAt string
}

func init() {
	registerAction("/admin/reboot", actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ConfirmedFormNonce: true, OneAtATime: true, ReceiptAuthorize: hostRebootAuthorize,
		Off: func(a *App) bool { return a.hostControl == nil }, Run: adminHostRebootAction})
	registerLoader("admin", loadAdminHostControl)
	registerPreview("admin", func(d *PageData) {
		d.HostControl = &HostControlView{Reason: "Host reboot is unavailable in this read-only preview."}
	})
}

func hostControlSocketConfig(getenv func(string) string) (string, error) {
	path := getenv("HOST_CONTROL_SOCKET")
	if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == "/" ||
		len(path) > 100 || strings.ContainsAny(path, "\x00\r\n")) {
		return "", errors.New("HOST_CONTROL_SOCKET must be a clean absolute Unix socket path inside a private directory, at most100bytes")
	}
	return path, nil
}

// Called after confirmedTx has locked/rechecked the actor and before it claims
// or replays a receipt. A completed receipt never grants new reboot authority.
func hostRebootAuthorize(c *actionCtx) error {
	if c.A.hostControl == nil || !privateAdminIngress(c.Ctx()) {
		return fail(http.StatusNotFound, "Unknown action")
	}
	var enrolled bool
	if err := c.Tx.QueryRowContext(c.Ctx(), "SELECT totp_enabled FROM users WHERE id=$1", c.User.ID).Scan(&enrolled); err != nil {
		return err
	}
	if !enrolled {
		return fail(http.StatusForbidden, "Enable an authenticator on your administrator account before requesting a host reboot.")
	}
	return nil
}

func adminHostRebootAction(c *actionCtx) (actionResult, error) {
	if c.A.hostControl == nil || !privateAdminIngress(c.Ctx()) {
		return actionResult{}, fail(http.StatusNotFound, "Unknown action")
	}
	allowed := map[string]bool{"csrf": true, formNonceField: true, "password": true, "code": true, "confirm_reboot": true}
	for name, values := range c.Form {
		if !allowed[name] || len(values) != 1 {
			return actionResult{}, fail(400, "Reload the host reboot form and confirm its stated scope.")
		}
	}
	if c.Form.Get("confirm_reboot") != "app-host-manual-unlock" {
		return actionResult{}, fail(400, "Confirm application-host downtime and manual disk unlock before rebooting.")
	}
	// Read-only readiness work stays outside the account's transaction. The host
	// agent rechecks independently before acceptance and again before shutdown.
	status, statusErr := c.A.hostControl.Status(c.Ctx())
	created := false
	request := hostcontrol.Request{Version: 1, Action: "reboot-app-host", RequestID: c.Form.Get(formNonceField)}
	res, err := confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		if err := hostRebootAuthorize(c); err != nil {
			return actionResult{}, err
		}
		if statusErr != nil || !status.Ready || status.Reason != "ready" || status.Pending != nil {
			return actionResult{}, fail(409, "Host reboot is unavailable. Check encryption, the host agent and the manual-unlock rehearsal before requesting a new reboot.")
		}
		if err := c.Tx.QueryRowContext(c.Ctx(), `INSERT INTO host_reboot_requests(id,user_id,state,requested_at)
			VALUES($1,$2,'recorded',clock_timestamp()) RETURNING requested_at`, request.RequestID, c.User.ID).Scan(&request.RequestedAt); err != nil {
			return actionResult{}, err
		}
		created = true
		return actionResult{Redirect: "/admin#host-control", Audit: "Requested application-host OS reboot; request " + request.RequestID + "; acknowledged downtime and manual disk unlock"}, nil
	})
	if err != nil || !created {
		// This also covers a lost database Commit acknowledgement. There is no
		// dispatch on uncertainty and no dispatch when a form receipt is replayed.
		return res, err
	}
	request.RequestedAt = request.RequestedAt.UTC()
	// Record that dispatch may begin before contacting the host. If this write
	// fails, do not send; if the process dies later, the page reports uncertainty.
	marked, markErr := c.A.db.ExecContext(c.Ctx(), "UPDATE host_reboot_requests SET state='dispatching',updated_at=clock_timestamp() WHERE id=$1 AND state='recorded'", request.RequestID)
	if markErr == nil {
		var count int64
		count, markErr = marked.RowsAffected()
		if markErr == nil && count != 1 {
			markErr = errors.New("reboot request is no longer pending dispatch")
		}
	}
	if markErr != nil {
		return actionResult{}, fail(500, "The reboot request was recorded, but dispatch could not be confirmed. Check its request ID on the admin page; it will not be retried automatically.")
	}
	receipt, dispatchErr := c.A.hostControl.RequestReboot(c.Ctx(), request)
	state := "unknown"
	var accepted, reboot any
	if dispatchErr == nil && receipt.RequestID == request.RequestID && receipt.State == "accepted" &&
		!receipt.AcceptedAt.IsZero() && receipt.RebootAt.After(receipt.AcceptedAt) {
		state, accepted, reboot = "accepted", receipt.AcceptedAt.UTC(), receipt.RebootAt.UTC()
	}
	// The reboot may already be scheduled. Keep all later diagnostics generic
	// and never convert a missing response into a second request or retry.
	if err := recordHostRebootDispatch(c.Ctx(), c.A.db, request.RequestID, c.User.ID, state, accepted, reboot); err != nil {
		return actionResult{}, fail(500, "The host may have accepted the reboot, but its receipt could not be saved. Check the existing request ID with the operator; do not submit a new reboot to retry it.")
	}
	return res, nil
}

func recordHostRebootDispatch(ctx context.Context, db *sql.DB, id, actor, state string, accepted, reboot any) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE host_reboot_requests SET state=$2,accepted_at=$3,reboot_at=$4,updated_at=clock_timestamp() WHERE id=$1`, id, state, accepted, reboot); err != nil {
		return err
	}
	action := "Application-host reboot request " + id + " outcome unknown; verify its host receipt before requesting another reboot"
	if state == "accepted" {
		action = "Host agent accepted application-host reboot request " + id + "; OS reboot remains pending"
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", actor, action); err != nil {
		return err
	}
	return tx.Commit()
}

func loadAdminHostControl(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" || d.User.RetentionRestricted {
		return nil
	}
	view := &HostControlView{Reason: "Host reboot is not configured. The operator must finish encrypted-host migration, configure the local host agent and rehearse manual unlock."}
	d.HostControl = view
	if a.hostControl == nil {
		return nil
	}
	if !privateAdminIngress(r.Context()) {
		view.Reason = "Host reboot is available only through the private administrator connection."
		return nil
	}
	var enrolled bool
	err := a.db.QueryRowContext(ctx, `SELECT totp_enabled FROM users WHERE id=$1 AND role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL`, d.User.ID).Scan(&enrolled)
	if err == sql.ErrNoRows {
		d.HostControl = nil
		return nil
	}
	if err != nil {
		return err
	}
	view.MFARequired = !enrolled
	var latest HostRebootRequestView
	var requested time.Time
	var accepted, reboot sql.NullTime
	err = a.db.QueryRowContext(ctx, "SELECT id,state,requested_at,accepted_at,reboot_at FROM host_reboot_requests ORDER BY requested_at DESC,id DESC LIMIT 1").Scan(&latest.ID, &latest.State, &requested, &accepted, &reboot)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		latest.RequestedAt = requested.UTC().Format(time.RFC3339)
		latest.State = hostRebootStateLabel(latest.State)
		if accepted.Valid {
			latest.AcceptedAt = accepted.Time.UTC().Format(time.RFC3339)
		}
		if reboot.Valid {
			latest.RebootAt = reboot.Time.UTC().Format(time.RFC3339)
		}
		view.Latest = &latest
	}
	status, err := a.hostControl.Status(ctx)
	if err != nil {
		view.Reason = "The host agent is unavailable. An earlier request may still be pending; check its request ID before trying again."
		return nil
	}
	view.Reason = hostRebootReason(status.Reason)
	view.Ready = status.Ready && status.Reason == "ready" && status.Pending == nil && enrolled
	if view.MFARequired {
		view.Reason = "Enable an authenticator on this administrator account before requesting a host reboot."
	}
	receipt := status.Pending
	if receipt == nil {
		receipt = status.Last
	}
	if receipt != nil {
		view.AgentReceipt = &HostRebootRequestView{ID: receipt.RequestID, State: hostRebootStateLabel(receipt.State)}
		if !receipt.AcceptedAt.IsZero() {
			view.AgentReceipt.AcceptedAt = receipt.AcceptedAt.UTC().Format(time.RFC3339)
		}
		if !receipt.RebootAt.IsZero() {
			view.AgentReceipt.RebootAt = receipt.RebootAt.UTC().Format(time.RFC3339)
		}
	}
	return nil
}

func hostRebootStateLabel(state string) string {
	switch state {
	case "accepted":
		return "Accepted; reboot pending"
	case "executing":
		return "Reboot command requested"
	case "boot-changed":
		return "A later host boot was observed"
	case "failed", "cancelled", "abandoned":
		return "Not completed; operator review required"
	default:
		return "Outcome unconfirmed; verify this request before requesting another reboot"
	}
}

func hostRebootReason(reason string) string {
	switch reason {
	case "ready":
		return "The host agent reports verified storage and a recorded manual-unlock rehearsal."
	case "storage-unverified":
		return "Encrypted storage or its expected identities could not be verified."
	case "unlock-not-rehearsed":
		return "The operator must record a successful manual-unlock rehearsal for this host."
	case "reboot-pending":
		return "An application-host reboot is already pending."
	case "cooldown":
		return "A recent reboot request is still within the host agent's cooldown."
	default:
		return "The host agent is not ready for a reboot."
	}
}
