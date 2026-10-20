package market

import (
	"net/url"
	"testing"
)

func TestTicketMessagesRequireSessionProofForBuyersAndStaff(t *testing.T) {
	for _, actor := range []string{"opener", "mod", "othermod", "admin"} {
		t.Run(actor, func(t *testing.T) {
			w := newTicketTestWorld(t)
			id, _ := w.open("mod")
			// An account-level verification flag, PGP sign-in enrollment and TOTP
			// enrollment do not authorize this unverified browser to send a message.
			agExec(w.e, "UPDATE users SET pgp_verified_at=now(),pgp_2fa=true WHERE id=$1", w.ids[actor])
			agEnableTOTP(w.e, w.ids[actor])
			agExec(w.e, "UPDATE sessions SET pgp_access_key='' WHERE token_hash=$1", digest(w.sessions[actor]))
			form := url.Values{"id": {id}, "body": {w.ciphertext("mod", "Private reply from a browser that must authenticate its current key.")}}
			w.refused("/tickets/reply", actor, form)
			proveAccess(w.e, w.ids[actor], w.sessions[actor], w.keys[actor])
			w.e.check(w.e.do("POST", "/tickets/reply", w.sessions[actor], form), 303)
		})
	}
}

func TestTicketProofIsBrowserBoundAndKeyReplacementRevokesOldBrowsers(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	first := w.sessions["opener"]
	second := agSession(w.e, w.ids["opener"])
	w.sessions["opener"] = second
	form := url.Values{"id": {id}, "body": {w.ciphertext("mod", "Private reply from the second browser.")}}
	w.refused("/tickets/reply", "opener", form)
	proveAccess(w.e, w.ids["opener"], second, w.keys["opener"])
	w.e.check(w.e.do("POST", "/tickets/reply", second, form), 303)

	key, public := testPGPKey(t, "ticket-replaced-opener")
	w.e.check(w.e.do("POST", "/account", second, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	w.keys["opener"], w.public["opener"] = key, public
	form.Set("body", w.ciphertext("mod", "Private reply using the replacement public key."))
	for _, token := range []string{first, second} {
		w.sessions["opener"] = token
		w.refused("/tickets/reply", "opener", form)
	}
	w.sessions["opener"] = second
	proveAccess(w.e, w.ids["opener"], second, key)
	w.e.check(w.e.do("POST", "/tickets/reply", second, form), 303)
	if agInt(w.e, "SELECT count(*) FROM ticket_messages WHERE ticket_id=$1", id) != 3 {
		t.Fatal("unverified browsers changed the encrypted ticket conversation")
	}
}

func TestTicketBrowseOnlySessionsCanReadAndCloseButCannotCompose(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	second := agSession(w.e, w.ids["opener"])
	list := w.e.body("GET", "/tickets?staff="+w.ids["mod"], second, nil, 200)
	mustContain(t, list, id, "Before opening a ticket", "decrypt a PGP authentication challenge")
	mustNotContain(t, list, `method="post" action="/tickets"`, `name="body"`)
	thread := w.e.body("GET", "/ticket?id="+id, second, nil, 200)
	mustContain(t, thread, "Encrypted conversation", "Close ticket", "Before replying")
	mustNotContain(t, thread, `action="/tickets/reply"`, `name="body"`)
	w.e.check(w.e.do("POST", "/tickets/status", second, url.Values{"id": {id}, "status": {"closed"}}), 303)
	if agStr(w.e, "SELECT status FROM tickets WHERE id=$1", id) != "closed" {
		t.Fatal("a browse-only opener could not close their existing ticket")
	}
}
