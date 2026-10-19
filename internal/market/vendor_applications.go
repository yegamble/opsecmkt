package market

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-544: vendor onboarding (migration 121). Sign-up chooses a buyer or a vendor account; a vendor account is a buyer
// account with a vendor application, decided under the administrator's vendor policy:
//
//   - vendor_bond_xmr: the bond in atomic piconero, 0 for none. The bond is a NON-REFUNDABLE fee paid as the
//     administrator's instructions (vendor_bond_instructions, free text) say. In this release an administrator records
//     it as received (how 'manual') or waives it ('waived'); the market runs no wallet code for it. 'xmr' is reserved
//     for automatic deposit detection (A-547).
//   - vendor_approval: 'manual' (an administrator approves or rejects) or 'automatic' (approved once no bond is due).
//
// Each application captures the bond and approval mode in force when it is created, so a policy change applies to new
// applications only. States: awaiting_bond → (bond recorded or waived) → awaiting_approval or, automatic, approved;
// awaiting_approval → approved | rejected; either open state → withdrawn (the applicant) or rejected. Approval makes
// the account a vendor through the audited role change the admin role form uses; rejected and withdrawn applicants
// stay buyers, so they cannot list (/listings is role-gated). Every decision notifies the applicant and is audited on
// both accounts. An administrator may also mark a vendor verified: the staff's judgment, not an identity check.

const (
	settingVendorBond          = "vendor_bond_xmr"
	settingVendorApproval      = "vendor_approval"
	settingVendorInstructions  = "vendor_bond_instructions"
	vendorNoteMaxRunes         = 500
	vendorInstructionsMaxRunes = 2000
	vendorOpenStates           = "('awaiting_bond','awaiting_approval')"
)

// activeVendorSQL is the SQL condition "alias is an approved, active vendor": the vendor role (given only by an
// approved application, the admin role form or the -set-role host command; migration 121 grandfathered earlier
// vendors with an approved application) on an account that is not suspended, closed or restricted for closure. A
// banned account is always suspended too (migration 123, A-546), so the suspension test excludes it. alias names a
// users row in the caller's query.
func activeVendorSQL(alias string) string {
	return "(" + alias + ".role='vendor' AND " + alias + ".suspended_at IS NULL AND " + alias + ".closed_at IS NULL AND " +
		alias + ".retention_restricted_at IS NULL)"
}

// VendorPolicyView is the vendor policy for new applications. Bond is in XMR ("0" when none).
type VendorPolicyView struct {
	Bond               string
	HasBond, Automatic bool
	Instructions       string
}

// VendorApplicationView is one application: the applicant's latest on /account, or an open one in the admin queue.
// RoleRemoved (/account only): it was approved, but the account is no longer a vendor.
type VendorApplicationView struct {
	ID, Handle, State, Status, Bond, Created, BondReceived, BondHow, Decided, Note string
	HasBond, Automatic, Open, AwaitingBond, AwaitingApproval, RoleRemoved          bool
}

// VendorAccountView is the /account vendor section (buyers and vendors only).
type VendorAccountView struct {
	Policy   VendorPolicyView
	Latest   *VendorApplicationView
	CanApply bool
	Verified bool
}

// VendorAdminView is the admin vendors panel: the policy form, the open queue (oldest first, at most 100 of
// QueueTotal) and the verified active vendors (at most 100, by handle).
type VendorAdminView struct {
	Policy     VendorPolicyView
	Queue      []VendorApplicationView
	QueueTotal int
	Verified   []string
}

func init() {
	registerAction("/account/vendor-application", actionSpec{Roles: []string{"buyer"}, FormNonce: true, Run: vendorApplicantAction})
	registerAction("/admin/vendor-policy", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: vendorPolicyAction})
	registerAction("/admin/vendor-applications", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: vendorDecisionAction})
	registerAction("/admin/vendor-verify", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: vendorVerifyAction})
	registerAction("/admin/vendor-apply", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: vendorApplyForAction})
	registerLoader("register", func(_ context.Context, _ *App, _ *http.Request, d *PageData) error {
		p := vendorPolicyFromSettings(d.Settings)
		d.VendorPolicy = &p
		return nil
	})
	registerLoader("account", vendorAccountLoader)
	registerLoader("admin", vendorAdminLoader)
	registerPreview("register", func(d *PageData) {
		d.VendorPolicy = &VendorPolicyView{Bond: "0.5", HasBond: true}
	})
	registerPreview("account", func(d *PageData) {
		d.VendorAccount = &VendorAccountView{Policy: VendorPolicyView{Bond: "0.5", HasBond: true, Instructions: "Preview only: the administrator's bond payment instructions appear here."},
			Latest: &VendorApplicationView{State: "awaiting_bond", Status: vendorStateLabel("awaiting_bond"), Bond: "0.5", HasBond: true, Created: "2026-01-01 09:00 UTC", Open: true, AwaitingBond: true}}
	})
	registerPreview("admin", func(d *PageData) {
		d.VendorAdmin = &VendorAdminView{Policy: VendorPolicyView{Bond: "0.5", HasBond: true, Instructions: "Preview only."},
			Queue: []VendorApplicationView{
				{ID: "preview-bond", Handle: "new_seller", State: "awaiting_bond", Status: vendorStateLabel("awaiting_bond"), Bond: "0.5", HasBond: true, Created: "2026-01-01 09:00 UTC", Open: true, AwaitingBond: true},
				{ID: "preview-approval", Handle: "paid_seller", State: "awaiting_approval", Status: vendorStateLabel("awaiting_approval"), Bond: "0.5", HasBond: true, Created: "2026-01-01 08:00 UTC", BondReceived: "2026-01-01 10:00 UTC", BondHow: "recorded by an administrator", Open: true, AwaitingApproval: true},
			}, QueueTotal: 2, Verified: []string{"ghost_circuit"}}
	})
}

func vendorStateLabel(state string) string {
	switch state {
	case "awaiting_bond":
		return "Waiting for bond"
	case "awaiting_approval":
		return "Waiting for administrator approval"
	case "approved":
		return "Approved"
	case "rejected":
		return "Rejected"
	case "withdrawn":
		return "Withdrawn"
	}
	return state
}

func vendorBondHowLabel(how string) string {
	switch how {
	case "manual":
		return "recorded by an administrator"
	case "waived":
		return "waived by an administrator"
	case "xmr":
		return "detected on the Monero network"
	}
	return how
}

// vendorBondText names a bond for audit rows and notices: "none" or "0.5 XMR".
func vendorBondText(piconero int64) string {
	if piconero == 0 {
		return "none"
	}
	return amount(piconero, 12) + " XMR"
}

type vendorPolicy struct {
	Bond         int64
	Automatic    bool
	Instructions string
}

func (p vendorPolicy) view() VendorPolicyView {
	return VendorPolicyView{Bond: amount(p.Bond, 12), HasBond: p.Bond > 0, Automatic: p.Automatic, Instructions: p.Instructions}
}

func (p vendorPolicy) approval() string {
	if p.Automatic {
		return "automatic"
	}
	return "manual"
}

func parseVendorPolicy(bond, approval, instructions string) vendorPolicy {
	n, err := strconv.ParseInt(bond, 10, 64)
	if err != nil || n < 0 {
		n = 0 // only the policy form writes it; migration 121 sets "0"
	}
	return vendorPolicy{Bond: n, Automatic: approval == "automatic", Instructions: instructions}
}

func vendorPolicyFromSettings(s map[string]string) VendorPolicyView {
	return parseVendorPolicy(s[settingVendorBond], s[settingVendorApproval], s[settingVendorInstructions]).view()
}

// readVendorPolicy reads the policy for a new application through q (the caller's transaction).
func readVendorPolicy(ctx context.Context, q rowQuerier) (vendorPolicy, error) {
	var bond, approval, instructions string
	err := q.QueryRowContext(ctx, `SELECT coalesce((SELECT value FROM settings WHERE key=$1),'0'),
		coalesce((SELECT value FROM settings WHERE key=$2),'manual'),coalesce((SELECT value FROM settings WHERE key=$3),'')`,
		settingVendorBond, settingVendorApproval, settingVendorInstructions).Scan(&bond, &approval, &instructions)
	return parseVendorPolicy(bond, approval, instructions), err
}

// submitVendorApplication creates userID's application under the current policy, in tx after the account row was
// locked or inserted with the buyer role and no open application. With no bond and automatic approval it is approved
// at once and the account becomes a vendor. It returns the new state.
func submitVendorApplication(ctx context.Context, tx *sql.Tx, userID string) (string, error) {
	p, err := readVendorPolicy(ctx, tx)
	if err != nil {
		return "", err
	}
	state := "awaiting_approval"
	switch {
	case p.Bond > 0:
		state = "awaiting_bond"
	case p.Automatic:
		state = "approved"
	}
	id := randomToken()
	_, err = tx.ExecContext(ctx, `INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero,decided_at)
		VALUES($1,$2,$3,$4,$5,CASE WHEN $3='approved' THEN now() END)`, id, userID, state, p.approval(), p.Bond)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", errVendorApplicationOpen
	}
	if err != nil {
		return "", err
	}
	audit := "Vendor application submitted: bond " + vendorBondText(p.Bond) + ", " + p.approval() + " approval; " + strings.ToLower(vendorStateLabel(state))
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", userID, audit); err != nil {
		return "", err
	}
	if state == "approved" {
		if err = approveVendor(ctx, tx, userID, "automatic vendor approval", "", ""); err != nil {
			return "", err
		}
	}
	return state, nil
}

// vendorRoleAssigned keeps the applications consistent when the admin role form or the -set-role host command makes
// an account a vendor directly, in tx after the account row is locked and its role set: any open application is
// closed (withdrawn) and an approved application without a bond records the assignment, decided by adminID ("" for
// the host command) with note. A demotion leaves the approved application; /account then says the role was removed,
// and the account may apply again (an approved application never counts as open).
func vendorRoleAssigned(ctx context.Context, tx *sql.Tx, userID, adminID, note string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE vendor_applications SET state='withdrawn',decided_by=NULLIF($2,''),decided_at=now(),note=$3
		WHERE user_id=$1 AND state IN `+vendorOpenStates, userID, adminID, note); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero,decided_by,decided_at,note)
		VALUES($1,$2,'approved','manual',0,NULLIF($3,''),now(),$4)`, randomToken(), userID, adminID, note)
	return err
}

var errVendorApplicationOpen = fail(http.StatusConflict, "You already have an open vendor application. Withdraw it first to apply again.")

// approveVendor makes an approved applicant a vendor (the application row is already approved), in tx after the
// account row is locked: the role change is audited as the admin role form's is ("Role changed from buyer to vendor by
// <by>", which disputeEvidence also reads), roleChanged runs and the applicant is notified, after lead (the step that
// approved it, or ""), with note.
func approveVendor(ctx context.Context, tx *sql.Tx, userID, by, lead, note string) error {
	var role string
	if err := tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id=$1", userID).Scan(&role); err != nil {
		return err
	}
	body := lead + "Your vendor application was approved. You can publish listings from the vendor desk."
	if role == "buyer" {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET role='vendor' WHERE id=$1", userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", userID, "Role changed from buyer to vendor by "+by); err != nil {
			return err
		}
		if _, err := roleChanged(ctx, tx, userID, "buyer", "vendor", by); err != nil {
			return err
		}
	}
	if note != "" {
		body += " Note from the administrator: " + note
	}
	_, err := insertNotification(ctx, tx, userID, body, "")
	return err
}

// cleanVendorNote checks an administrator's decision note: trimmed, one line of printable characters, at most
// vendorNoteMaxRunes. It is shown to the applicant.
func cleanVendorNote(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return "", fail(400, "Note not saved: write it on one line, with no control or text-direction characters.")
	}
	if utf8.RuneCountInString(v) > vendorNoteMaxRunes {
		return "", fail(400, "Note not saved: use at most 500 characters.")
	}
	return v, nil
}

// cleanVendorInstructions checks the bond payment instructions: trimmed, line breaks kept (CRLF as LF), otherwise only
// printable characters, at most vendorInstructionsMaxRunes.
func cleanVendorInstructions(raw string) (string, error) {
	v := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return r != '\n' && !unicode.IsGraphic(r) }) >= 0 {
		return "", fail(400, "Payment instructions not saved: use printable text and line breaks only, with no control or text-direction characters.")
	}
	if utf8.RuneCountInString(v) > vendorInstructionsMaxRunes {
		return "", fail(400, "Payment instructions not saved: use at most 2000 characters.")
	}
	return v, nil
}

// parseVendorBond reads the policy form's bond in XMR: 0 (or 0.0…) for none, else up to 12 decimals.
func parseVendorBond(raw string) (int64, error) {
	v := strings.TrimSpace(raw)
	if v != "" && strings.Trim(v, "0.") == "" && strings.Count(v, ".") <= 1 && v[0] == '0' {
		return 0, nil
	}
	n, err := parseAmount(v, 12)
	if err != nil {
		return 0, fail(400, "Bond not saved: enter an amount in XMR with at most 12 decimals, or 0 for no bond.")
	}
	return n, nil
}

// vendorApplicantAction serves the applicant's own form: apply under the current policy, or withdraw the open
// application. It takes no application id: it acts only on the caller's own rows.
func vendorApplicantAction(c *actionCtx) (actionResult, error) {
	ctx, tx, uid := c.Ctx(), c.Tx, c.User.ID
	choice := c.Form.Get("action")
	if choice != "apply" && choice != "withdraw" {
		return actionResult{}, fail(400, "Choose apply or withdraw")
	}
	var role string
	if err := tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id=$1 FOR NO KEY UPDATE", uid).Scan(&role); err != nil {
		return actionResult{}, err
	}
	if role != "buyer" {
		return actionResult{}, errAccountChanged
	}
	if choice == "withdraw" {
		var id string
		err := tx.QueryRowContext(ctx, `UPDATE vendor_applications SET state='withdrawn',decided_by=$1,decided_at=now()
			WHERE user_id=$1 AND state IN `+vendorOpenStates+` RETURNING id`, uid).Scan(&id)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(409, "You have no open vendor application; nothing was changed.")
		}
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/account", Notice: "vendor-withdrawn", Audit: "Withdrew vendor application"}, nil
	}
	var open bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM vendor_applications WHERE user_id=$1 AND state IN "+vendorOpenStates+")", uid).Scan(&open); err != nil {
		return actionResult{}, err
	}
	if open {
		return actionResult{}, errVendorApplicationOpen
	}
	state, err := submitVendorApplication(ctx, tx, uid)
	if err != nil {
		return actionResult{}, err
	}
	notice := "vendor-applied"
	if state == "approved" {
		notice = "vendor-approved"
	}
	return actionResult{Redirect: "/account", Notice: notice}, nil
}

// vendorApplyForAction opens a vendor application for a buyer account on its behalf, from the account's page in the
// user directory (A-545: "vendor, not approved yet or bond unpaid"). The administrator picks the starting state (owner
// decision 2026-10-19, "Admin picks"), whatever the policy's approval mode: start=awaiting_bond asks for the current
// policy's bond (refused while the policy sets none: set a bond under Vendors first), start=awaiting_approval asks for
// no bond. The application is always recorded with manual approval, so nothing on this path approves it: recording the
// bond moves it to waiting for approval, and an administrator approves or rejects it as any other. It refuses a
// non-buyer, a suspended, banned, closed or closing account and an account with an open application; administrators,
// the caller included, are never buyers. The administrator confirms with their password (and authenticator code when
// enrolled); both accounts are audited with the chosen state and the account is notified.
func vendorApplyForAction(c *actionCtx) (actionResult, error) {
	handle := strings.TrimSpace(c.Form.Get("handle"))
	if handle == "" {
		return actionResult{}, fail(400, "Enter an account handle")
	}
	if handle == c.User.Handle {
		return actionResult{}, fail(400, "You cannot open a vendor application for your own account.")
	}
	state := c.Form.Get("start")
	if state != "awaiting_bond" && state != "awaiting_approval" {
		return actionResult{}, fail(400, "Choose whether the application starts waiting for the bond or waiting for approval. Nothing was changed.")
	}
	notFound := fail(404, "No account has the handle \""+handle+"\". Check the spelling: handles are case-sensitive.")
	// The handle is looked up before the password is checked, so a typo does not use a confirmation attempt.
	var target string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT id FROM users WHERE handle=$1", handle).Scan(&target)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		// The account row, then its application: the order the account's own form locks them in.
		var role string
		var suspended, banned, closed bool
		err := tx.QueryRowContext(ctx, `SELECT role,suspended_at IS NOT NULL,banned_at IS NOT NULL,
			closed_at IS NOT NULL OR deletion_requested_at IS NOT NULL OR retention_restricted_at IS NOT NULL
			FROM users WHERE id=$1 FOR NO KEY UPDATE`, target).Scan(&role, &suspended, &banned, &closed)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case banned:
			return actionResult{}, fail(409, handle+" is banned; nothing was changed.")
		case closed:
			return actionResult{}, fail(409, handle+" is closed or closing; nothing was changed.")
		case suspended:
			return actionResult{}, fail(409, handle+" is suspended: restore the account first. Nothing was changed.")
		case role != "buyer":
			return actionResult{}, fail(409, handle+" is a "+role+" account: only a buyer account can apply to sell. Nothing was changed.")
		}
		var open bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM vendor_applications WHERE user_id=$1 AND state IN "+vendorOpenStates+")", target).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open {
			return actionResult{}, fail(409, handle+" already has an open vendor application; decide it on this page. Nothing was changed.")
		}
		var bond int64
		if state == "awaiting_bond" {
			p, err := readVendorPolicy(ctx, tx)
			if err != nil {
				return actionResult{}, err
			}
			if p.Bond == 0 {
				return actionResult{}, fail(409, "The vendor policy sets no bond, so an application cannot wait for one. Choose waiting for approval, or set a bond under Vendors first. Nothing was changed.")
			}
			bond = p.Bond
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero) VALUES($1,$2,$3,'manual',$4)`,
			randomToken(), target, state, bond); err != nil {
			return actionResult{}, err
		}
		chosen := strings.ToLower(vendorStateLabel(state)) + ", bond " + vendorBondText(bond) + ", manual approval"
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target,
			"Vendor application opened on the account's behalf by administrator "+c.User.Handle+": "+chosen); err != nil {
			return actionResult{}, err
		}
		body := "An administrator opened a vendor application for your account. Its status: " + strings.ToLower(vendorStateLabel(state)) + "."
		if state == "awaiting_bond" {
			body += " The bond is " + vendorBondText(bond) + ", a non-refundable fee; your account page says how to pay it."
		} else {
			body += " No bond is asked; an administrator decides it."
		}
		if _, err = insertNotification(ctx, tx, target, body, ""); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserURL(handle), Notice: "vendor-opened", Audit: "Opened a vendor application for " + handle + ": " + chosen}, nil
	})
}

// vendorPolicyAction saves the vendor policy (bond, approval mode and bond payment instructions) for new applications.
// The administrator confirms with their password (and authenticator code when enrolled); the change is audited.
func vendorPolicyAction(c *actionCtx) (actionResult, error) {
	bond, err := parseVendorBond(c.Form.Get(settingVendorBond))
	if err != nil {
		return actionResult{}, err
	}
	approval := c.Form.Get(settingVendorApproval)
	if approval != "manual" && approval != "automatic" {
		return actionResult{}, fail(400, "Choose manual or automatic approval")
	}
	instructions, err := cleanVendorInstructions(c.Form.Get(settingVendorInstructions))
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		for _, key := range []string{settingVendorBond, settingVendorApproval, settingVendorInstructions} {
			if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'') ON CONFLICT(key) DO NOTHING", key); err != nil {
				return actionResult{}, err
			}
		}
		var oldBond, oldApproval, oldInstructions string
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=$1 FOR UPDATE),(SELECT value FROM settings WHERE key=$2 FOR UPDATE),
			(SELECT value FROM settings WHERE key=$3 FOR UPDATE)`, settingVendorBond, settingVendorApproval, settingVendorInstructions).Scan(&oldBond, &oldApproval, &oldInstructions); err != nil {
			return actionResult{}, err
		}
		old := parseVendorPolicy(oldBond, oldApproval, oldInstructions)
		var changes []string
		if old.Bond != bond {
			changes = append(changes, "bond from "+vendorBondText(old.Bond)+" to "+vendorBondText(bond))
		}
		if old.approval() != approval {
			changes = append(changes, "approval from "+old.approval()+" to "+approval)
		}
		if old.Instructions != instructions {
			changes = append(changes, "payment instructions updated")
		}
		if len(changes) == 0 {
			return actionResult{}, fail(409, "The vendor policy is already that; nothing was changed.")
		}
		for key, value := range map[string]string{settingVendorBond: strconv.FormatInt(bond, 10), settingVendorApproval: approval, settingVendorInstructions: instructions} {
			if err := setSetting(c, key, value); err != nil {
				return actionResult{}, err
			}
		}
		return actionResult{Redirect: "/admin/vendors", Notice: "vendor-policy", Audit: "Vendor policy changed: " + strings.Join(changes, "; ") + " (applies to new applications)"}, nil
	})
}

// vendorDecisionAction records an administrator's step on one application: bond received or waived (awaiting_bond),
// approve (awaiting_approval) or reject (either open state), with an optional note shown to the applicant. The
// administrator confirms with their password (and authenticator code when enrolled). Both accounts are audited and
// the applicant is notified.
func vendorDecisionAction(c *actionCtx) (actionResult, error) {
	id, choice := c.Form.Get("id"), c.Form.Get("action")
	if choice != "bond" && choice != "waive" && choice != "approve" && choice != "reject" {
		return actionResult{}, fail(400, "Choose record bond received, waive bond, approve or reject")
	}
	note, err := cleanVendorNote(c.Form.Get("note"))
	if err != nil {
		return actionResult{}, err
	}
	notFound := fail(404, "Vendor application not found")
	// The application is looked up before the password is checked, so a stale form does not use a confirmation attempt.
	var exists bool
	if err := c.A.db.QueryRowContext(c.Ctx(), "SELECT EXISTS(SELECT 1 FROM vendor_applications WHERE id=$1)", id).Scan(&exists); err != nil {
		return actionResult{}, err
	}
	if !exists {
		return actionResult{}, notFound
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx, admin := c.Ctx(), c.Tx, c.User
		// Applicant row, then application row: the order the applicant's own form locks them in.
		var target, handle, role string
		var closed bool
		err := tx.QueryRowContext(ctx, `SELECT u.id,u.handle,u.role,u.closed_at IS NOT NULL OR u.deletion_requested_at IS NOT NULL OR u.retention_restricted_at IS NOT NULL
			FROM users u JOIN vendor_applications a ON a.user_id=u.id WHERE a.id=$1 FOR NO KEY UPDATE OF u`, id).Scan(&target, &handle, &role, &closed)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		if target == admin.ID {
			return actionResult{}, fail(403, "You cannot decide your own vendor application.")
		}
		var state, approval string
		var bond int64
		if err = tx.QueryRowContext(ctx, "SELECT state,approval,bond_piconero FROM vendor_applications WHERE id=$1 FOR UPDATE", id).Scan(&state, &approval, &bond); err != nil {
			return actionResult{}, err
		}
		if state != "awaiting_bond" && state != "awaiting_approval" {
			return actionResult{}, fail(409, "This vendor application is "+strings.ToLower(vendorStateLabel(state))+"; nothing was changed.")
		}
		by := "administrator " + admin.Handle
		withNote := func(s string) string {
			if note != "" {
				return s + " Note from the administrator: " + note
			}
			return s
		}
		var actorAudit string // the administrator's row, written by confirmedTx
		audit := func(applicant, actor string) error {
			actorAudit = actor
			_, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, applicant)
			return err
		}
		switch choice {
		case "bond", "waive":
			if state != "awaiting_bond" {
				return actionResult{}, fail(409, "No bond is due on this vendor application; nothing was changed.")
			}
			how, next := "manual", "awaiting_approval"
			if choice == "waive" {
				how = "waived"
			}
			if approval == "automatic" {
				next = "approved"
				if role != "buyer" && role != "vendor" || closed {
					return actionResult{}, fail(409, handle+" is no longer an active buyer account; reject the application instead.")
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE vendor_applications SET bond_received_at=now(),bond_received_by=$2,bond_received_how=$3,state=$4,
				decided_at=CASE WHEN $4='approved' THEN now() END,note=$5 WHERE id=$1`, id, admin.ID, how, next, note); err != nil {
				return actionResult{}, err
			}
			applicantAudit, adminAudit := "Vendor bond of "+vendorBondText(bond)+" recorded as received by "+by, "Recorded vendor bond of "+vendorBondText(bond)+" received from "+handle
			body := "An administrator recorded your vendor bond of " + vendorBondText(bond) + " as received."
			if how == "waived" {
				applicantAudit, adminAudit = "Vendor bond waived by "+by, "Waived vendor bond of "+vendorBondText(bond)+" for "+handle
				body = "An administrator waived your vendor bond."
			}
			if err = audit(applicantAudit, adminAudit); err != nil {
				return actionResult{}, err
			}
			if next == "approved" { // one notice: the bond step, then the approval
				err = approveVendor(ctx, tx, target, "automatic vendor approval", body+" ", note)
			} else {
				_, err = insertNotification(ctx, tx, target, withNote(body+" Your application now waits for administrator approval."), "")
			}
			if err != nil {
				return actionResult{}, err
			}
		case "approve":
			if state != "awaiting_approval" {
				return actionResult{}, fail(409, "Record or waive the bond before approving this vendor application; nothing was changed.")
			}
			if role != "buyer" && role != "vendor" || closed {
				return actionResult{}, fail(409, handle+" is no longer an active buyer account; reject the application instead.")
			}
			if _, err = tx.ExecContext(ctx, "UPDATE vendor_applications SET state='approved',decided_by=$2,decided_at=now(),note=$3 WHERE id=$1", id, admin.ID, note); err != nil {
				return actionResult{}, err
			}
			if err = audit("Vendor application approved by "+by, "Approved vendor application of "+handle); err != nil {
				return actionResult{}, err
			}
			if err = approveVendor(ctx, tx, target, by, "", note); err != nil {
				return actionResult{}, err
			}
		case "reject":
			if _, err = tx.ExecContext(ctx, "UPDATE vendor_applications SET state='rejected',decided_by=$2,decided_at=now(),note=$3 WHERE id=$1", id, admin.ID, note); err != nil {
				return actionResult{}, err
			}
			if err = audit("Vendor application rejected by "+by, "Rejected vendor application of "+handle); err != nil {
				return actionResult{}, err
			}
			body := "Your vendor application was rejected by an administrator. Your account stays a buyer account."
			if bond > 0 {
				body += " The bond is a non-refundable fee."
			}
			if _, err = insertNotification(ctx, tx, target, withNote(body), ""); err != nil {
				return actionResult{}, err
			}
		}
		return actionResult{Redirect: adminUserBack(c, handle, "/admin/vendors"), Notice: "vendor-decision", Audit: actorAudit}, nil
	})
}

// vendorVerifyAction marks a vendor verified or removes the mark, looked up by handle. Verifying needs the vendor
// role; removing works on any account still marked. The administrator confirms with their password (and
// authenticator code when enrolled); both accounts are audited and the vendor is notified.
func vendorVerifyAction(c *actionCtx) (actionResult, error) {
	choice := c.Form.Get("action")
	verify := choice == "verify"
	if !verify && choice != "unverify" {
		return actionResult{}, fail(400, "Choose verify or remove the mark")
	}
	handle := strings.TrimSpace(c.Form.Get("handle"))
	if handle == "" {
		return actionResult{}, fail(400, "Enter a vendor handle")
	}
	notFound := fail(404, "No account has the handle \""+handle+"\". Check the spelling: handles are case-sensitive.")
	// The handle is looked up before the password is checked, so a typo does not use a confirmation attempt.
	var target string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT id FROM users WHERE handle=$1", handle).Scan(&target)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx, admin := c.Ctx(), c.Tx, c.User
		var role string
		var verified, banned bool
		err := tx.QueryRowContext(ctx, "SELECT role,vendor_verified,banned_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", target).Scan(&role, &verified, &banned)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case verify && banned: // A-546: a ban removed the mark; lifting it does not bring it back by itself
			return actionResult{}, fail(409, handle+" is banned; nothing was changed.")
		case verify && role != "vendor":
			return actionResult{}, fail(409, handle+" is not a vendor account; nothing was changed.")
		case verify && verified:
			return actionResult{}, fail(409, handle+" is already a verified vendor; nothing was changed.")
		case !verify && !verified:
			return actionResult{}, fail(409, handle+" is not marked as a verified vendor; nothing was changed.")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET vendor_verified=$2,vendor_verified_set_by=$3,vendor_verified_set_at=now() WHERE id=$1", target, verify, admin.ID); err != nil {
			return actionResult{}, err
		}
		applicant, actor, body := "Marked as a verified vendor by administrator "+admin.Handle, "Marked "+handle+" as a verified vendor",
			"An administrator marked your account as a verified vendor. Your listings and vendor page show the badge."
		if !verify {
			applicant, actor, body = "Verified vendor mark removed by administrator "+admin.Handle, "Removed the verified vendor mark of "+handle,
				"An administrator removed the verified vendor mark from your account."
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, applicant); err != nil {
			return actionResult{}, err
		}
		if _, err = insertNotification(ctx, tx, target, body, ""); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserBack(c, handle, "/admin/vendors"), Notice: "vendor-verify", Audit: actor}, nil
	})
}

const vendorAppColumns = `a.id,u.handle,a.state,a.approval,a.bond_piconero,to_char(a.created,'YYYY-MM-DD HH24:MI "UTC"'),
	coalesce(to_char(a.bond_received_at,'YYYY-MM-DD HH24:MI "UTC"'),''),coalesce(a.bond_received_how,''),
	coalesce(to_char(a.decided_at,'YYYY-MM-DD HH24:MI "UTC"'),''),a.note`

func scanVendorApp(rows interface{ Scan(...any) error }, extra ...any) (VendorApplicationView, error) {
	var v VendorApplicationView
	var approval, how string
	var bond int64
	err := rows.Scan(append([]any{&v.ID, &v.Handle, &v.State, &approval, &bond, &v.Created, &v.BondReceived, &how, &v.Decided, &v.Note}, extra...)...)
	v.Status, v.Bond, v.HasBond, v.Automatic = vendorStateLabel(v.State), amount(bond, 12), bond > 0, approval == "automatic"
	v.BondHow = vendorBondHowLabel(how)
	v.AwaitingBond, v.AwaitingApproval = v.State == "awaiting_bond", v.State == "awaiting_approval"
	v.Open = v.AwaitingBond || v.AwaitingApproval
	return v, err
}

// vendorAccountLoader fills the /account vendor section for a buyer or vendor: the latest application and the policy
// a new one would follow.
func vendorAccountLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "buyer" && d.User.Role != "vendor" {
		return nil
	}
	view := &VendorAccountView{Policy: vendorPolicyFromSettings(d.Settings)}
	if err := a.db.QueryRowContext(ctx, "SELECT vendor_verified FROM users WHERE id=$1", d.User.ID).Scan(&view.Verified); err != nil {
		return err
	}
	latest, err := scanVendorApp(a.db.QueryRowContext(ctx, "SELECT "+vendorAppColumns+` FROM vendor_applications a JOIN users u ON u.id=a.user_id
		WHERE a.user_id=$1 ORDER BY a.created DESC,a.id DESC LIMIT 1`, d.User.ID))
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if latest.State == "approved" && d.User.Role != "vendor" {
			latest.RoleRemoved, latest.Status = true, "Vendor role removed by an administrator"
		}
		view.Latest = &latest
	}
	view.CanApply = d.User.Role == "buyer" && (view.Latest == nil || !view.Latest.Open)
	d.VendorAccount = view
	return nil
}

// vendorAdminLoader fills the admin vendors panel.
func vendorAdminLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	view := &VendorAdminView{Policy: vendorPolicyFromSettings(d.Settings)}
	rows, err := a.db.QueryContext(ctx, "SELECT "+vendorAppColumns+`,count(*) OVER () FROM vendor_applications a JOIN users u ON u.id=a.user_id
		WHERE a.state IN `+vendorOpenStates+` ORDER BY a.created,a.id LIMIT 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanVendorApp(rows, &view.QueueTotal)
		if err != nil {
			return err
		}
		view.Queue = append(view.Queue, v)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	verified, err := a.db.QueryContext(ctx, "SELECT u.handle FROM users u WHERE u.vendor_verified AND "+activeVendorSQL("u")+" ORDER BY u.handle LIMIT 100")
	if err != nil {
		return err
	}
	defer verified.Close()
	for verified.Next() {
		var h string
		if err = verified.Scan(&h); err != nil {
			return err
		}
		view.Verified = append(view.Verified, h)
	}
	if err = verified.Err(); err != nil {
		return err
	}
	d.VendorAdmin = view
	return nil
}
