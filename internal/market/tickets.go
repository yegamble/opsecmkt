package market

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
)

const ticketMessageLimit = 20000

type TicketRecord struct {
	ID, OpenerID, AssignedTo, Status string
	AdminOnly                        bool
	Created, Updated                 time.Time
}

type TicketStaffChoice struct {
	ID, Handle, Role, PublicKey, Fingerprint string
}

type ticketQueryer interface {
	rowQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func init() {
	registerAction("/tickets", actionSpec{FormNonce: true, Run: ticketCreateAction})
	registerAction("/tickets/reply", actionSpec{FormNonce: true, Run: ticketReplyAction})
	registerAction("/tickets/status", actionSpec{FormNonce: true, Run: ticketStatusAction})
	registerAction("/tickets/assign", actionSpec{Roles: []string{"moderator", "admin"}, FormNonce: true, Run: ticketAssignAction})
	registerAction("/tickets/escalate", actionSpec{Roles: []string{"moderator", "admin"}, FormNonce: true, Run: ticketEscalateAction})
}

func ticketPage(id string) string { return "/ticket?id=" + url.QueryEscape(id) }

func ticketStaffUser(u *User) bool {
	return u != nil && (u.Role == "admin" || u.Role == "moderator")
}

// Expressions are supplied only by application code. Participation history never grants access: a demoted or
// inactive account loses staff access, and a moderator cannot read an escalated ticket unless they opened it.
func ticketReadableSQL(t, userID, role string) string {
	return `(` + t + `.opener_id=` + userID + ` OR ((` + role + `='admin' OR (` + role + `='moderator' AND NOT ` + t + `.admin_only))
		AND EXISTS(SELECT 1 FROM users ticket_reader WHERE ticket_reader.id=` + userID + ` AND ticket_reader.role=` + role + `
		AND ticket_reader.suspended_at IS NULL AND ticket_reader.closed_at IS NULL AND ticket_reader.retention_restricted_at IS NULL)))`
}

// readTicket reads the current database role rather than trusting an earlier request's user snapshot. Mutations
// lock the actor before this ticket; the optional ticket lock serializes messages, assignment and terminal state.
func readTicket(ctx context.Context, q rowQuerier, id string, user *User, lock bool) (*TicketRecord, error) {
	if user == nil || id == "" || len(id) > 128 {
		return nil, sql.ErrNoRows
	}
	query := `SELECT t.id,COALESCE(t.opener_id,''),COALESCE(t.assigned_to,''),t.status,t.admin_only,t.created,t.updated
		FROM tickets t JOIN users reader ON reader.id=$2 WHERE t.id=$1 AND ` + ticketReadableSQL("t", "reader.id", "reader.role")
	if lock {
		query += " FOR UPDATE OF t"
	}
	t := &TicketRecord{}
	err := q.QueryRowContext(ctx, query, id, user.ID).Scan(&t.ID, &t.OpenerID, &t.AssignedTo, &t.Status, &t.AdminOnly, &t.Created, &t.Updated)
	return t, err
}

const ticketVisibleStaffSQL = `((NOT s.staff_hidden AND EXISTS(SELECT 1 FROM settings WHERE key='staff_list_public' AND value='true'))
	OR EXISTS(SELECT 1 FROM settings WHERE key='staff_contact' AND value=s.handle))`

// ticketStaffChoices gives nonstaff callers only staff they could already discover through the public list or
// operator contact. Current staff may assign to hidden colleagues. All choices must have a usable encryption key.
func ticketStaffChoices(ctx context.Context, q ticketQueryer, user *User, adminOnly bool) ([]TicketStaffChoice, error) {
	if user == nil {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT s.id,s.handle,s.role,s.pgp FROM users s WHERE s.role IN ('admin','moderator')
		AND s.suspended_at IS NULL AND s.closed_at IS NULL AND s.retention_restricted_at IS NULL
		AND (NOT $2::boolean OR s.role='admin') AND (`+ticketVisibleStaffSQL+` OR EXISTS(SELECT 1 FROM users actor
		WHERE actor.id=$1 AND actor.role IN ('admin','moderator') AND actor.suspended_at IS NULL AND actor.closed_at IS NULL AND actor.retention_restricted_at IS NULL))
		ORDER BY CASE s.role WHEN 'admin' THEN 0 ELSE 1 END,s.handle,s.id LIMIT 200`, user.ID, adminOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var choices []TicketStaffChoice
	for rows.Next() {
		var choice TicketStaffChoice
		var public string
		if err = rows.Scan(&choice.ID, &choice.Handle, &choice.Role, &public); err != nil {
			return nil, err
		}
		key, fingerprint, canonical, parseErr := parseCanonicalPublicKey(public)
		if parseErr != nil || !canReceiveEncrypted(key, time.Now()) {
			continue
		}
		choice.PublicKey, choice.Fingerprint = canonical, fingerprint
		choices = append(choices, choice)
	}
	return choices, rows.Err()
}

func ticketActorRole(c *actionCtx) (string, error) {
	var role string
	err := c.Tx.QueryRowContext(c.Ctx(), `SELECT role FROM users WHERE id=$1 AND suspended_at IS NULL AND closed_at IS NULL
		AND retention_restricted_at IS NULL FOR SHARE NOWAIT`, c.User.ID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errForbidden
	}
	return role, err
}

func ticketManageRole(t *TicketRecord, role string) bool {
	return role == "admin" || role == "moderator" && !t.AdminOnly
}

func ticketForAction(c *actionCtx) (*TicketRecord, string, error) {
	role, err := ticketActorRole(c)
	if err != nil {
		return nil, "", err
	}
	t, err := readTicket(c.Ctx(), c.Tx, c.Form.Get("id"), c.User, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fail(404, "Ticket not found")
	}
	return t, role, err
}

func ticketStaffTarget(c *actionCtx, openerID, targetID, actorRole string, adminOnly, initial bool) (string, error) {
	if targetID == "" || len(targetID) > 128 || targetID == openerID {
		return "", fail(404, "The chosen staff account is unavailable for this ticket.")
	}
	var id string
	query := `SELECT s.id FROM users s WHERE s.id=$1 AND s.role IN ('admin','moderator')
		AND s.suspended_at IS NULL AND s.closed_at IS NULL AND s.retention_restricted_at IS NULL
		AND (NOT $2::boolean OR s.role='admin')`
	if initial && actorRole != "admin" && actorRole != "moderator" {
		query += ` AND ` + ticketVisibleStaffSQL
	}
	query += " FOR SHARE OF s NOWAIT"
	err := c.Tx.QueryRowContext(c.Ctx(), query, targetID, adminOnly).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fail(404, "The chosen staff account is unavailable for this ticket.")
	}
	if err != nil {
		return "", err
	}
	ok, err := userCanReceiveEncrypted(c.Ctx(), c.Tx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fail(409, "The chosen staff account needs a usable PGP encryption key before receiving ticket messages.")
	}
	return id, nil
}

func ticketCiphertext(c *actionCtx, openerID, assignedTo, body string) (string, error) {
	if len(body) < 60 || len(body) > ticketMessageLimit {
		return "", fail(400, "Paste a complete PGP-encrypted ticket message, up to 20 KB, encrypted to the opener and assigned staff account.")
	}
	canonical, err := canonicalMessage(body)
	if err != nil {
		return "", fail(400, "Ticket messages must contain only a complete PGP-encrypted message.")
	}
	ids, encrypted, err := inspectEncrypted(canonical)
	if err != nil || !encrypted || len(ids) == 0 || slices.Contains(ids, uint64(0)) {
		return "", fail(400, "Encrypt this ticket message to both displayed PGP keys, with visible recipient key IDs.")
	}
	users := []string{openerID, assignedTo, c.User.ID}
	slices.Sort(users)
	users = slices.Compact(users)
	for _, id := range users {
		if id == "" {
			return "", fail(409, "The opener, sender and assigned staff account need current usable PGP encryption keys before a ticket message can be saved.")
		}
		var one int
		if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT 1 FROM users WHERE id=$1 FOR SHARE NOWAIT", id).Scan(&one); err != nil {
			return "", err
		}
		key, err := loadPGPAccount(c.Ctx(), c.Tx, id, false)
		if err != nil {
			return "", err
		}
		if !canReceiveEncrypted(key.Key, time.Now()) {
			return "", fail(409, "The opener, sender and assigned staff account need current usable PGP encryption keys before a ticket message can be saved.")
		}
		if id == openerID || id == assignedTo {
			selected, ok := key.Key.EncryptionKey(time.Now())
			if !ok || !slices.Contains(ids, selected.PublicKey.KeyId) {
				return "", fail(400, "This ticket message must be encrypted to both the opener's and currently assigned staff account's displayed PGP keys.")
			}
		}
	}
	return canonical, nil
}

func ticketParticipant(ctx context.Context, tx *sql.Tx, ticketID, userID string, assigned, messaged bool) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ticket_participants(ticket_id,user_id,ever_assigned,ever_messaged) VALUES($1,$2,$3,$4)
		ON CONFLICT(ticket_id,user_id) DO UPDATE SET ever_assigned=ticket_participants.ever_assigned OR excluded.ever_assigned,
		ever_messaged=ticket_participants.ever_messaged OR excluded.ever_messaged`, ticketID, userID, assigned, messaged)
	return err
}

func ticketEvent(c *actionCtx, id, event, target string) error {
	_, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO ticket_events(ticket_id,actor_id,event,staff_id) VALUES($1,$2,$3,NULLIF($4,''))`, id, c.User.ID, event, target)
	return err
}

func insertTicketNotice(ctx context.Context, tx *sql.Tx, userID, body, audience, ticketID, messageID string) error {
	if userID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body,audience,ticket_id,ticket_message_id)
		VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''))`, randomToken(), userID, body, audience, ticketID, messageID)
	return err
}

// An absent ticket FK can also mean retention erased an inaccessible thread.
// Only this explicit metadata path grants historical staff an unlinked notice;
// ordinary ticket notices retain the default-false grant after FK unlinking.
func insertHistoricalTicketNotice(ctx context.Context, tx *sql.Tx, userID, body string) error {
	if userID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body,audience,ticket_history_only)
		VALUES($1,$2,$3,'ticket',true)`, randomToken(), userID, body)
	return err
}

func ticketNotifyStaff(c *actionCtx, t *TicketRecord, body string, except []string) error {
	staff, err := collectStrings(c.Ctx(), c.Tx, `SELECT id FROM users WHERE role IN ('admin','moderator') AND suspended_at IS NULL
		AND closed_at IS NULL AND retention_restricted_at IS NULL AND (NOT $1::boolean OR role='admin') AND id<>ALL($2)`, t.AdminOnly, except)
	if err != nil {
		return err
	}
	for _, id := range staff {
		if err = insertTicketNotice(c.Ctx(), c.Tx, id, body, "ticket", t.ID, ""); err != nil {
			return err
		}
	}
	return nil
}

func ticketInsertMessage(c *actionCtx, t *TicketRecord, body string) error {
	id := randomToken()
	if _, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO ticket_messages(id,ticket_id,sender_id,body,encrypted) VALUES($1,$2,$3,$4,true)`, id, t.ID, c.User.ID, body); err != nil {
		return err
	}
	if err := ticketParticipant(c.Ctx(), c.Tx, t.ID, c.User.ID, false, true); err != nil {
		return err
	}
	for _, target := range []string{t.OpenerID, t.AssignedTo} {
		if target != c.User.ID {
			audience := "ticket"
			if target == t.OpenerID {
				audience = "personal"
			}
			if err := insertTicketNotice(c.Ctx(), c.Tx, target, "You received a new message from "+c.User.Handle+" regarding Ticket #"+t.ID+".", audience, t.ID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func ticketCreateAction(c *actionCtx) (actionResult, error) {
	role, err := ticketActorRole(c)
	if err != nil {
		return actionResult{}, err
	}
	if !c.A.allowWriteBudget("ticket-open:"+c.User.ID, 5) {
		return actionResult{}, fail(429, "An account can open at most 5 tickets in 10 minutes. Try again later.")
	}
	target, err := ticketStaffTarget(c, c.User.ID, c.Form.Get("staff_id"), role, false, true)
	if err != nil {
		return actionResult{}, err
	}
	body, err := ticketCiphertext(c, c.User.ID, target, strings.TrimSpace(c.Form.Get("body")))
	if err != nil {
		return actionResult{}, err
	}
	t := &TicketRecord{ID: randomToken(), OpenerID: c.User.ID, AssignedTo: target, Status: "open"}
	if _, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO tickets(id,opener_id,assigned_to) VALUES($1,$2,$3)`, t.ID, t.OpenerID, t.AssignedTo); err != nil {
		return actionResult{}, err
	}
	if err = ticketParticipant(c.Ctx(), c.Tx, t.ID, target, true, false); err != nil {
		return actionResult{}, err
	}
	if err = ticketEvent(c, t.ID, "opened", target); err != nil {
		return actionResult{}, err
	}
	if err = ticketInsertMessage(c, t, body); err != nil {
		return actionResult{}, err
	}
	if err = ticketNotifyStaff(c, t, "Ticket #"+t.ID+" was opened.", []string{c.User.ID, target}); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: ticketPage(t.ID), Audit: "Opened encrypted support ticket " + t.ID}, nil
}

func ticketReplyAction(c *actionCtx) (actionResult, error) {
	t, role, err := ticketForAction(c)
	if err != nil {
		return actionResult{}, err
	}
	if t.Status != "open" {
		return actionResult{}, fail(409, "This ticket is closed or ignored. Staff must reopen it before another message is sent.")
	}
	if !c.A.allowWriteBudget("ticket-msg:"+c.User.ID, messageBudget) {
		return actionResult{}, fail(429, "An account can send at most 20 ticket messages in 10 minutes. Try again later.")
	}
	if _, err = ticketStaffTarget(c, t.OpenerID, t.AssignedTo, role, t.AdminOnly, false); err != nil {
		return actionResult{}, err
	}
	body, err := ticketCiphertext(c, t.OpenerID, t.AssignedTo, strings.TrimSpace(c.Form.Get("body")))
	if err != nil {
		return actionResult{}, err
	}
	if err = ticketInsertMessage(c, t, body); err != nil {
		return actionResult{}, err
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), "UPDATE tickets SET updated=now() WHERE id=$1", t.ID); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: ticketPage(t.ID), Audit: "Sent encrypted message on support ticket " + t.ID}, nil
}

func ticketAssignAction(c *actionCtx) (actionResult, error) {
	return ticketAssign(c, false)
}

func ticketEscalateAction(c *actionCtx) (actionResult, error) {
	return ticketAssign(c, true)
}

func ticketAssign(c *actionCtx, escalate bool) (actionResult, error) {
	t, role, err := ticketForAction(c)
	if err != nil {
		return actionResult{}, err
	}
	if !ticketManageRole(t, role) {
		return actionResult{}, errForbidden
	}
	if t.Status != "open" {
		return actionResult{}, fail(409, "Reopen this ticket before changing its assignment or escalating it.")
	}
	if escalate && t.AdminOnly {
		return actionResult{}, fail(409, "This ticket has already been escalated to administrators.")
	}
	target, err := ticketStaffTarget(c, t.OpenerID, c.Form.Get("staff_id"), role, t.AdminOnly || escalate, false)
	if err != nil {
		return actionResult{}, err
	}
	if !escalate && target == t.AssignedTo {
		return actionResult{}, fail(409, "This staff account is already assigned to the ticket.")
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), `UPDATE tickets SET assigned_to=$2,admin_only=admin_only OR $3,updated=now() WHERE id=$1`, t.ID, target, escalate); err != nil {
		return actionResult{}, err
	}
	t.AssignedTo, t.AdminOnly = target, t.AdminOnly || escalate
	if err = ticketParticipant(c.Ctx(), c.Tx, t.ID, target, true, false); err != nil {
		return actionResult{}, err
	}
	event := "assigned"
	if escalate {
		event = "escalated"
	}
	if err = ticketEvent(c, t.ID, event, target); err != nil {
		return actionResult{}, err
	}
	body := "Ticket #" + t.ID + " was assigned to a staff account. Encrypt future messages to the current keys shown on the ticket."
	if escalate {
		body = "Ticket #" + t.ID + " was escalated to administrators. Encrypt future messages to the current keys shown on the ticket."
	}
	if err = insertTicketNotice(c.Ctx(), c.Tx, t.OpenerID, body, "personal", t.ID, ""); err != nil {
		return actionResult{}, err
	}
	if escalate {
		if err = ticketNotifyStaff(c, t, "Ticket #"+t.ID+" was escalated to administrators.", []string{c.User.ID, t.OpenerID}); err != nil {
			return actionResult{}, err
		}
	} else if target != c.User.ID {
		if err = insertTicketNotice(c.Ctx(), c.Tx, target, "Ticket #"+t.ID+" was assigned to you. Earlier messages retain their original encryption recipients.", "ticket", t.ID, ""); err != nil {
			return actionResult{}, err
		}
	}
	redirect := ticketPage(t.ID)
	if !ticketManageRole(t, role) && c.User.ID != t.OpenerID {
		redirect = "/tickets"
	}
	return actionResult{Redirect: redirect, Audit: "Support ticket " + t.ID + " " + event}, nil
}

func ticketStatusAction(c *actionCtx) (actionResult, error) {
	t, role, err := ticketForAction(c)
	if err != nil {
		return actionResult{}, err
	}
	next := c.Form.Get("status")
	if next != "open" && next != "closed" && next != "ignored" {
		return actionResult{}, fail(400, "Choose Open, Closed or Ignored.")
	}
	if !ticketManageRole(t, role) && !(c.User.ID == t.OpenerID && t.Status == "open" && next == "closed") {
		return actionResult{}, errForbidden
	}
	if next == t.Status {
		return actionResult{}, fail(409, "This ticket already has that status. Nothing was changed.")
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), "UPDATE tickets SET status=$2,updated=now() WHERE id=$1", t.ID, next); err != nil {
		return actionResult{}, err
	}
	t.Status = next
	event := next
	if next == "open" {
		event = "reopened"
	}
	if err = ticketEvent(c, t.ID, event, ""); err != nil {
		return actionResult{}, err
	}
	body := "Ticket #" + t.ID + " is now " + strings.ToUpper(next[:1]) + next[1:] + "."
	// The opener is always alerted, including when they closed their own ticket.
	if err = insertTicketNotice(c.Ctx(), c.Tx, t.OpenerID, body, "personal", t.ID, ""); err != nil {
		return actionResult{}, err
	}
	if next == "open" {
		if err = ticketNotifyStaff(c, t, body, []string{c.User.ID, t.OpenerID}); err != nil {
			return actionResult{}, err
		}
	} else {
		if err = ticketNotifyHistoricalStaff(c, t, body); err != nil {
			return actionResult{}, err
		}
	}
	return actionResult{Redirect: ticketPage(t.ID), Audit: "Support ticket " + t.ID + " " + event}, nil
}

func ticketNotifyHistoricalStaff(c *actionCtx, t *TicketRecord, body string) error {
	staff, err := collectStrings(c.Ctx(), c.Tx, `SELECT u.id FROM ticket_participants p JOIN users u ON u.id=p.user_id
		WHERE p.ticket_id=$1 AND (p.ever_assigned OR p.ever_messaged) AND u.id<>$2
		AND u.role IN ('admin','moderator') AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.retention_restricted_at IS NULL`, t.ID, t.OpenerID)
	if err != nil {
		return err
	}
	for _, id := range staff {
		link := t.ID
		if _, err = readTicket(c.Ctx(), c.Tx, t.ID, &User{ID: id}, false); errors.Is(err, sql.ErrNoRows) {
			link = "" // historical moderators receive closure metadata without a dead link to an admin thread
		} else if err != nil {
			return err
		}
		if link == "" {
			err = insertHistoricalTicketNotice(c.Ctx(), c.Tx, id, body)
		} else {
			err = insertTicketNotice(c.Ctx(), c.Tx, id, body, "ticket", link, "")
		}
		if err != nil {
			return err
		}
	}
	return nil
}
