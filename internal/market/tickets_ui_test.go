package market

import (
	"database/sql"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTicketUIRecipientVisibilityAndPrivateThread(t *testing.T) {
	w := newTicketTestWorld(t)
	e := w.e
	agExec(e, "UPDATE settings SET value='false' WHERE key='staff_list_public'")
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact','ticket_mod') ON CONFLICT(key) DO UPDATE SET value=excluded.value")
	page := e.body("GET", "/tickets", w.sessions["opener"], nil, 200)
	mustContain(t, page, "Support recipient", "ticket_mod", "Load support keys")
	mustContain(t, page, "New tickets and their metadata are visible to moderators and administrators.", "Choosing an administrator as the recipient does not escalate the ticket or restrict moderator access.")
	mustNotContain(t, page, "ticket_othermod", "ticket_otheradmin", `name="subject"`, `name="title"`, "Open encrypted ticket")
	e.check(e.do("GET", "/tickets?staff="+w.ids["otheradmin"], w.sessions["opener"], nil), 404)
	page = e.body("GET", "/tickets?staff="+w.ids["mod"], w.sessions["opener"], nil, 200)
	mustContain(t, page, "PGP encrypted ticket", "Open encrypted ticket", "Encrypt to both ticket participants", `name="staff_id" value="`+w.ids["mod"]+`"`)
	renderedFormNonce(t, page, "/tickets")
	mustNotContain(t, page, w.public["otheradmin"])
	id, _ := w.open("mod")
	page = e.body("GET", "/ticket?id="+id, w.sessions["opener"], nil, 200)
	mustContain(t, page, id, "Ticket details", "Encrypted conversation", "Reply with encrypted text", "Close ticket", "Ticket history", "PGP encrypted reply", "does not make earlier messages readable")
	renderedFormNonce(t, page, "/tickets/reply")
	renderedFormNonce(t, page, "/tickets/status")
	mustNotContain(t, page, "Staff ticket controls", "Administrator assignee", "Private opening ticket evidence that must remain encrypted.")
	e.check(e.do("GET", "/ticket?id="+id, w.sessions["stranger"], nil), 404)
	mustNotContain(t, e.body("GET", "/tickets", w.sessions["stranger"], nil, 200), id)
	page = e.body("GET", "/ticket?id="+id, w.sessions["mod"], nil, 200)
	mustContain(t, page, "Staff ticket controls", "New assignee", "Administrator assignee", "Escalate to administrators")
	renderedFormNonce(t, page, "/tickets/assign")
	renderedFormNonce(t, page, "/tickets/escalate")
}

func TestTicketUIUnavailableAssigneeRequiresStaffReassignment(t *testing.T) {
	for _, state := range []struct {
		name, change string
		adminOnly    bool
	}{
		{"demoted", "UPDATE users SET role='buyer' WHERE id=$1", false},
		{"suspended", "UPDATE users SET suspended_at=now() WHERE id=$1", false},
		{"closed", "UPDATE users SET closed_at=now() WHERE id=$1", false},
		{"restricted", "UPDATE users SET retention_restricted_at=now() WHERE id=$1", false},
		{"escalated administrator demoted to moderator", "UPDATE users SET role='moderator' WHERE id=$1", true},
	} {
		t.Run(state.name, func(t *testing.T) {
			w := newTicketTestWorld(t)
			e := w.e
			id, _ := w.open("mod")
			assignee := "mod"
			if state.adminOnly {
				e.check(e.do("POST", "/tickets/escalate", w.sessions["mod"], url.Values{"id": {id}, "staff_id": {w.ids["admin"]}}), 303)
				assignee = "admin"
			}
			agExec(e, state.change, w.ids[assignee])
			if usable, err := userCanReceiveEncrypted(t.Context(), e.DB, w.ids[assignee]); err != nil || !usable {
				t.Fatalf("assignee fixture lost its usable key: %v", err)
			}
			for _, reader := range []string{"opener", "otheradmin"} {
				page := e.body("GET", "/ticket?id="+id, w.sessions[reader], nil, 200)
				mustContain(t, page, "the current assignee is no longer eligible to handle this ticket", `placeholder="-----BEGIN PGP MESSAGE-----" disabled>`, `type="submit" disabled>Send encrypted ticket reply`)
				mustNotContain(t, page, "Replies are unavailable until the opener, current assignee and sender all have usable PGP encryption keys.")
				if reader == "otheradmin" {
					mustContain(t, page, "Use Assign ticket in the staff controls below.")
				} else {
					mustContain(t, page, "Ask market staff to reassign this ticket.")
				}
				if state.adminOnly {
					mustContain(t, page, "An active administrator must be assigned")
				} else {
					mustContain(t, page, "An active moderator or administrator must be assigned")
				}
			}
			e.check(e.do("POST", "/tickets/assign", w.sessions["otheradmin"], url.Values{"id": {id}, "staff_id": {w.ids["otheradmin"]}}), 303)
			page := e.body("GET", "/ticket?id="+id, w.sessions["opener"], nil, 200)
			mustNotContain(t, page, "the current assignee is no longer eligible to handle this ticket", `type="submit" disabled>Send encrypted ticket reply`)
		})
	}
}

func TestTicketUIAdministratorRecipientDoesNotEscalate(t *testing.T) {
	w := newTicketTestWorld(t)
	e := w.e
	page := e.body("GET", "/tickets?staff="+w.ids["admin"], w.sessions["opener"], nil, 200)
	mustContain(t, page, "New tickets and their metadata are visible to moderators and administrators.", "Choosing an administrator as the recipient does not escalate the ticket or restrict moderator access.")
	id, _ := w.open("admin")
	page = e.body("GET", "/ticket?id="+id, w.sessions["mod"], nil, 200)
	mustContain(t, page, "Moderators and administrators", "ticket_admin")
	mustNotContain(t, page, "This ticket was escalated to administrators.")
}

func TestTicketUIEscalationAndCurrentReaderRole(t *testing.T) {
	w := newTicketTestWorld(t)
	e := w.e
	id, _ := w.open("mod")
	e.check(e.do("POST", "/tickets/escalate", w.sessions["mod"], url.Values{"id": {id}, "staff_id": {w.ids["admin"]}}), 303)
	e.check(e.do("GET", "/ticket?id="+id, w.sessions["mod"], nil), 404)
	mustNotContain(t, e.body("GET", "/tickets", w.sessions["mod"], nil, 200), id)
	page := e.body("GET", "/ticket?id="+id, w.sessions["opener"], nil, 200)
	mustContain(t, page, "Administrators only", "ticket_admin", "Close ticket")
	mustNotContain(t, page, "Staff ticket controls")
	// The loader must use the role stored now, even if its caller still holds a
	// moderator snapshot from before a role change.
	normal, _ := w.open("othermod")
	stale := &User{ID: w.ids["mod"], Handle: "ticket_mod", Role: "moderator"}
	agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", stale.ID)
	r := httptest.NewRequest("GET", "/ticket?id="+normal, nil)
	if err := ticketThreadLoader(r.Context(), e.A, r, &PageData{User: stale}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("demoted reader loaded a staff thread: %v", err)
	}
}

func TestTicketUIMissingKeysAndStatusControls(t *testing.T) {
	w := newTicketTestWorld(t)
	e := w.e
	id, _ := w.open("mod")
	e.setKey(w.ids["othermod"], "")
	staffPage := e.body("GET", "/ticket?id="+id, w.sessions["othermod"], nil, 200)
	mustContain(t, staffPage, "Before replying", "decrypt a PGP authentication challenge")
	mustNotContain(t, staffPage, `action="/tickets/reply"`)
	e.setKey(w.ids["opener"], "")
	page := e.body("GET", "/ticket?id="+id, w.sessions["opener"], nil, 200)
	mustContain(t, page, "Replies are unavailable until the opener, current assignee and sender all have usable PGP encryption keys.", "Before replying")
	mustNotContain(t, page, `action="/tickets/reply"`)
	e.check(e.do("POST", "/tickets/status", w.sessions["opener"], url.Values{"id": {id}, "status": {"closed"}}), 303)
	page = e.body("GET", "/ticket?id="+id, w.sessions["opener"], nil, 200)
	mustContain(t, page, "This ticket is Closed", "Replies are unavailable until authorized staff reopen it.")
	mustNotContain(t, page, `action="/tickets/reply"`, "Staff ticket controls")
	page = e.body("GET", "/ticket?id="+id, w.sessions["admin"], nil, 200)
	mustContain(t, page, "Open (reopen)", "Update ticket status")
	if strings.Contains(page, `action="/tickets/assign"`) || strings.Contains(page, `action="/tickets/escalate"`) {
		t.Fatal("closed ticket exposed assignment or escalation forms")
	}
}

func TestTicketUIDemotedAssigneeCannotExposeTheirNewBuyerKey(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	_, privateBuyerPublic := testPGPKey(t, "ticket-former-staff-private-buyer")
	_, fingerprint, canonical, err := parseCanonicalPublicKey(privateBuyerPublic)
	if err != nil {
		t.Fatal(err)
	}
	agExec(w.e, "UPDATE users SET role='buyer' WHERE id=$1", w.ids["mod"])
	w.e.setKey(w.ids["mod"], privateBuyerPublic)
	for _, reader := range []string{"opener", "otheradmin"} {
		page := w.e.body("GET", "/ticket?id="+id, w.sessions[reader], nil, 200)
		mustContain(t, page, "ticket_mod", "Encrypted conversation", "Ticket history", "the current assignee is no longer eligible")
		mustNotContain(t, page, canonical, formatFingerprint(fingerprint))
	}
	w.e.check(w.e.do("GET", "/tickets?staff="+w.ids["mod"], w.sessions["opener"], nil), 404)
}

func TestTicketUIHiddenActiveAssigneeStillSharesTheirTicketKey(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	agExec(w.e, "UPDATE users SET staff_hidden=true WHERE id=$1", w.ids["mod"])
	agExec(w.e, "UPDATE settings SET value='false' WHERE key='staff_list_public'")
	_, fingerprint, canonical, err := parseCanonicalPublicKey(w.public["mod"])
	if err != nil {
		t.Fatal(err)
	}
	page := w.e.body("GET", "/ticket?id="+id, w.sessions["opener"], nil, 200)
	mustContain(t, page, canonical, formatFingerprint(fingerprint), "Send encrypted ticket reply")
	mustNotContain(t, page, "the current assignee is no longer eligible")
}

func TestTicketUISelectedAndAssignedKeyReadsFailClosedDuringRoleChange(t *testing.T) {
	for _, kind := range []string{"selected recipient", "assigned staff"} {
		t.Run(kind, func(t *testing.T) {
			w := newTicketTestWorld(t)
			id, _ := w.open("mod")
			_, privateBuyerPublic := testPGPKey(t, "ticket-concurrent-private-buyer")
			change, err := w.e.DB.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer change.Rollback()
			if _, err = change.ExecContext(t.Context(), "UPDATE users SET role='buyer',pgp=$2 WHERE id=$1", w.ids["mod"], privateBuyerPublic); err != nil {
				t.Fatal(err)
			}
			d := &PageData{User: w.e.loadUser(w.ids["opener"])}
			path := "/ticket?id=" + id
			loader := ticketThreadLoader
			if kind == "selected recipient" {
				path = "/tickets?staff=" + w.ids["mod"]
				loader = ticketListLoader
			}
			r := httptest.NewRequest("GET", path, nil)
			err = loader(r.Context(), w.e.A, r, d)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("role/key change did not defer the %s read safely: %v", kind, err)
			}
			if d.Ticket != nil || d.TicketList != nil {
				t.Fatal("a deferred role/key read exposed ticket recipient keys")
			}
		})
	}
}
