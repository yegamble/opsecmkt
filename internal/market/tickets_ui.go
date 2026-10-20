package market

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const ticketPageSize = 50

type TicketListEntry struct {
	ID, Opener, Assignee, Status, Created, Updated string
	AdminOnly                                      bool
}

func (t TicketListEntry) StatusLabel() string { return ticketStatusLabel(t.Status) }

type TicketListView struct {
	Staff, Mine, KeyUsable, CanOpen bool
	Status                          string
	Entries                         []TicketListEntry
	Choices                         []TicketStaffChoice
	Selected                        *TicketStaffChoice
	SelectedKey                     ContactKey
	OpenerKey                       ContactKey
	Paging                          *ActivityPaging
}

type TicketMessageView struct{ ID, Sender, Body, Created string }
type TicketHistoryView struct{ Actor, Event, Staff, Created string }

func (h TicketHistoryView) Description() string {
	switch h.Event {
	case "opened":
		return "Opened ticket"
	case "assigned":
		return "Assigned ticket"
	case "escalated":
		return "Escalated to administrators"
	case "closed":
		return "Closed ticket"
	case "ignored":
		return "Marked ticket ignored"
	case "reopened":
		return "Reopened ticket"
	}
	return "Ticket updated"
}

type TicketView struct {
	ID, Opener, Assignee, Status, Created, Updated string
	AdminOnly, CanReply, CanClose, CanManage       bool
	AssigneeEligible, ReplyKeysUsable              bool
	Keys                                           []ContactKey
	Choices, AdminChoices                          []TicketStaffChoice
	Messages                                       []TicketMessageView
	History                                        []TicketHistoryView
	Paging                                         *ActivityPaging
}

func (t TicketView) StatusLabel() string { return ticketStatusLabel(t.Status) }

func ticketStatusLabel(status string) string {
	switch status {
	case "open":
		return "Open"
	case "closed":
		return "Closed"
	case "ignored":
		return "Ignored"
	}
	return "Unavailable"
}

func init() {
	registerPage("tickets", pageSpec{Protected: true})
	registerPage("ticket", pageSpec{Protected: true})
	registerLoader("tickets", ticketListLoader)
	registerLoader("ticket", ticketThreadLoader)
	registerLoader("tickets", staffContactLoader)
	registerLoader("ticket", staffContactLoader)
	registerPreview("tickets", func(d *PageData) {
		d.TicketList = &TicketListView{KeyUsable: false, Entries: []TicketListEntry{{ID: "sample-ticket", Opener: "preview_buyer", Assignee: "preview_moderator", Status: "open", Created: "Sample", Updated: "Sample"}}}
	})
	registerPreview("ticket", func(d *PageData) {
		d.Ticket = &TicketView{ID: "sample-ticket", Opener: "preview_buyer", Assignee: "preview_moderator", Status: "open", Created: "Sample", Updated: "Sample", History: []TicketHistoryView{{Actor: "preview_buyer", Event: "opened", Staff: "preview_moderator", Created: "Sample"}}}
	})
}

// Ticket loaders lock the reader before checking the ticket. Role changes and
// escalation cannot turn an authorized page read into a protected thread read
// halfway through rendering its messages and recipient keys.
func ticketUIReader(ctx context.Context, tx *sql.Tx, u *User) (*User, error) {
	reader := *u
	if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id=$1 AND suspended_at IS NULL AND closed_at IS NULL
		AND retention_restricted_at IS NULL FOR SHARE`, u.ID).Scan(&reader.Role); err != nil {
		return nil, err
	}
	return &reader, nil
}

// Lock eligible staff before separately loading their current key. A stale ticket
// assignment or recipient choice must never expose a demoted buyer's replacement key.
// Peer locks are nonblocking because another reader may already hold a ticket lock.
func ticketUIStaffEligible(ctx context.Context, tx *sql.Tx, id, openerID string, adminOnly, publicOnly bool) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM users s WHERE s.id=$1 AND s.id<>$3
		AND s.role IN ('moderator','admin') AND (NOT $2::boolean OR s.role='admin')
		AND s.suspended_at IS NULL AND s.closed_at IS NULL AND s.retention_restricted_at IS NULL`
	if publicOnly {
		query += " AND " + ticketVisibleStaffSQL
	}
	query += " FOR SHARE OF s NOWAIT)"
	var eligible bool
	err := tx.QueryRowContext(ctx, query, id, adminOnly, openerID).Scan(&eligible)
	return eligible, err
}

func ticketUIKey(ctx context.Context, q rowQuerier, id, handle, relation string) (ContactKey, error) {
	k := ContactKey{Handle: handle, Relation: relation}
	if id == "" {
		return k, nil
	}
	p, err := loadPGPAccount(ctx, q, id, false)
	if err == sql.ErrNoRows {
		return k, nil
	}
	if err != nil {
		return k, err
	}
	switch {
	case canReceiveEncrypted(p.Key, time.Now()):
		verifiedOn, _, _ := strings.Cut(p.VerifiedAt, " ")
		k.Armored, k.Fingerprint, k.Verified, k.VerifiedAt = p.Canonical, formatFingerprint(p.Fingerprint), p.Verified, verifiedOn
	case p.Key != nil:
		k.NoEncryption = true
	case p.Armored != "":
		k.Unreadable = true
	}
	return k, nil
}

func ticketListLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil {
		return sql.ErrNoRows
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	reader, err := ticketUIReader(ctx, tx, d.User)
	if err != nil {
		return err
	}
	v := &TicketListView{Staff: ticketStaffUser(reader), Status: r.URL.Query().Get("status")}
	v.Mine = !v.Staff || r.URL.Query().Get("view") == "mine"
	if v.Status != "" && v.Status != "open" && v.Status != "closed" && v.Status != "ignored" {
		return fail(400, "Choose Open, Closed, Ignored or all ticket statuses.")
	}
	base := ` FROM tickets t JOIN users reader ON reader.id=$1 WHERE ` + ticketReadableSQL("t", "reader.id", "reader.role") + ` AND (NOT $2 OR t.opener_id=reader.id) AND ($3='' OR t.status=$3)`
	var total int
	if err = tx.QueryRowContext(ctx, "SELECT count(*)"+base, reader.ID, v.Mine, v.Status).Scan(&total); err != nil {
		return err
	}
	extra := url.Values{}
	if v.Status != "" {
		extra.Set("status", v.Status)
	}
	if v.Staff && v.Mine {
		extra.Set("view", "mine")
	}
	v.Paging = activityPaging("/tickets", total, r.URL.Query().Get("page"), extra)
	rows, err := tx.QueryContext(ctx, `SELECT t.id,COALESCE((SELECT handle FROM users WHERE id=t.opener_id),'Former account'),
		COALESCE((SELECT handle FROM users WHERE id=t.assigned_to),'Former account'),t.status,t.admin_only,
		to_char(t.created,'YYYY-MM-DD HH24:MI "UTC"'),to_char(t.updated,'YYYY-MM-DD HH24:MI "UTC"')`+base+
		` ORDER BY (t.status='open') DESC,t.updated DESC,t.id DESC LIMIT $4 OFFSET $5`, reader.ID, v.Mine, v.Status, ticketPageSize, v.Paging.Offset())
	if err != nil {
		return err
	}
	for rows.Next() {
		var entry TicketListEntry
		if err = rows.Scan(&entry.ID, &entry.Opener, &entry.Assignee, &entry.Status, &entry.AdminOnly, &entry.Created, &entry.Updated); err != nil {
			rows.Close()
			return err
		}
		v.Entries = append(v.Entries, entry)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	v.Choices, err = ticketStaffChoices(ctx, tx, reader, false)
	if err != nil {
		return err
	}
	v.Choices = slices.DeleteFunc(v.Choices, func(choice TicketStaffChoice) bool { return choice.ID == reader.ID })
	selected := r.URL.Query().Get("staff")
	if selected != "" {
		for i := range v.Choices {
			if v.Choices[i].ID == selected {
				v.Selected = &v.Choices[i]
				break
			}
		}
		if v.Selected == nil {
			return fail(404, "This support recipient is unavailable.")
		}
		eligible, err := ticketUIStaffEligible(ctx, tx, v.Selected.ID, reader.ID, false, !ticketStaffUser(reader))
		if err != nil {
			return err
		}
		if !eligible {
			return fail(404, "This support recipient is unavailable.")
		}
		v.SelectedKey, err = ticketUIKey(ctx, tx, v.Selected.ID, v.Selected.Handle, v.Selected.Role)
		if err != nil {
			return err
		}
	}
	v.OpenerKey, err = ticketUIKey(ctx, tx, reader.ID, reader.Handle, "your account")
	if err != nil {
		return err
	}
	v.KeyUsable = v.OpenerKey.Usable()
	accessVerified, err := pgpAccessVerified(ctx, tx, reader.ID, sessionToken(r))
	if err != nil {
		return err
	}
	v.CanOpen = accessVerified && v.KeyUsable && v.Selected != nil && v.SelectedKey.Usable()
	d.TicketList = v
	return tx.Commit()
}

func ticketThreadLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || r.URL.Query().Get("id") == "" || len(r.URL.Query().Get("id")) > 128 {
		return sql.ErrNoRows
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	reader, err := ticketUIReader(ctx, tx, d.User)
	if err != nil {
		return err
	}
	record, err := readTicket(ctx, tx, r.URL.Query().Get("id"), reader, true)
	if err != nil {
		return err
	}
	v := &TicketView{ID: record.ID, Status: record.Status, AdminOnly: record.AdminOnly,
		Created: record.Created.UTC().Format("2006-01-02 15:04 UTC"), Updated: record.Updated.UTC().Format("2006-01-02 15:04 UTC")}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT handle FROM users WHERE id=$1),'Former account'),
		COALESCE((SELECT handle FROM users WHERE id=$2),'Former account')`, record.OpenerID, record.AssignedTo).Scan(&v.Opener, &v.Assignee); err != nil {
		return err
	}
	v.AssigneeEligible, err = ticketUIStaffEligible(ctx, tx, record.AssignedTo, record.OpenerID, record.AdminOnly, false)
	if err != nil {
		return err
	}
	for i, keyOwner := range []struct{ id, handle, relation string }{{record.OpenerID, v.Opener, "ticket opener"}, {record.AssignedTo, v.Assignee, "current assignee"}} {
		if i == 1 && !v.AssigneeEligible {
			continue
		}
		key, err := ticketUIKey(ctx, tx, keyOwner.id, keyOwner.handle, keyOwner.relation)
		if err != nil {
			return err
		}
		v.Keys = append(v.Keys, key)
	}
	v.CanManage = ticketStaffUser(reader) && (!record.AdminOnly || reader.Role == "admin")
	viewerKeyUsable, err := userCanReceiveEncrypted(ctx, tx, reader.ID)
	if err != nil {
		return err
	}
	v.ReplyKeysUsable = len(v.Keys) == 2 && v.Keys[0].Usable() && v.Keys[1].Usable() && viewerKeyUsable
	accessVerified, err := pgpAccessVerified(ctx, tx, reader.ID, sessionToken(r))
	if err != nil {
		return err
	}
	v.CanReply = accessVerified && record.Status == "open" && v.AssigneeEligible && v.ReplyKeysUsable
	v.CanClose = record.Status == "open" && (v.CanManage || reader.ID == record.OpenerID)
	if v.CanManage {
		v.Choices, err = ticketStaffChoices(ctx, tx, reader, record.AdminOnly)
		if err != nil {
			return err
		}
		v.Choices = slices.DeleteFunc(v.Choices, func(choice TicketStaffChoice) bool { return choice.ID == record.OpenerID })
		allAdmins, err := ticketStaffChoices(ctx, tx, reader, true)
		if err != nil {
			return err
		}
		v.AdminChoices = slices.DeleteFunc(allAdmins, func(choice TicketStaffChoice) bool { return choice.ID == record.OpenerID })
	}
	var total int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM ticket_messages WHERE ticket_id=$1", record.ID).Scan(&total); err != nil {
		return err
	}
	v.Paging = activityPaging("/ticket", total, r.URL.Query().Get("page"), url.Values{"id": {record.ID}})
	rows, err := tx.QueryContext(ctx, `SELECT m.id,COALESCE(u.handle,'Former account'),m.body,to_char(m.created,'YYYY-MM-DD HH24:MI "UTC"')
		FROM ticket_messages m LEFT JOIN users u ON u.id=m.sender_id WHERE m.ticket_id=$1
		ORDER BY m.created DESC,m.id DESC LIMIT $2 OFFSET $3`, record.ID, ticketPageSize, v.Paging.Offset())
	if err != nil {
		return err
	}
	for rows.Next() {
		var message TicketMessageView
		if err = rows.Scan(&message.ID, &message.Sender, &message.Body, &message.Created); err != nil {
			rows.Close()
			return err
		}
		v.Messages = append(v.Messages, message)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	slices.Reverse(v.Messages)
	rows, err = tx.QueryContext(ctx, `SELECT COALESCE(actor.handle,'Former account'),e.event,COALESCE(staff.handle,''),to_char(e.created,'YYYY-MM-DD HH24:MI "UTC"')
		FROM ticket_events e LEFT JOIN users actor ON actor.id=e.actor_id LEFT JOIN users staff ON staff.id=e.staff_id
		WHERE e.ticket_id=$1 ORDER BY e.created DESC,e.id DESC LIMIT 50`, record.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var event TicketHistoryView
		if err = rows.Scan(&event.Actor, &event.Event, &event.Staff, &event.Created); err != nil {
			rows.Close()
			return err
		}
		v.History = append(v.History, event)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	d.Ticket = v
	return tx.Commit()
}
