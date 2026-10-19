package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// P2: message encryption status comes from OpenPGP packet inspection (pgp.go inspectEncrypted), never
// from the armor header alone. The server cannot decrypt, so "to recipient's key" means a public-key
// session packet names one of the recipient's saved key IDs.

const (
	matchYes     = "yes"     // a recipient key ID in the message belongs to the recipient's saved key
	matchNo      = "no"      // the message names only other keys
	matchUnknown = "unknown" // hidden recipients the sender confirmed (A-569); on older rows also no usable key or passphrase-only
)

func init() {
	registerLoader("messages", messageStatusLoader)
	registerPreview("messages", func(d *PageData) {
		body := "-----BEGIN PGP MESSAGE-----\n(sample preview — not real ciphertext)\n-----END PGP MESSAGE-----"
		d.Messages = []Message{
			{ID: "m1", Sender: "ghost_circuit", Recipient: "preview_user", Body: body, Created: "Sample", Encrypted: true, RecipientMatch: matchYes},
			{ID: "m2", Sender: "preview_user", Recipient: "null_ptr", Body: body, Created: "Sample", Encrypted: true, RecipientMatch: matchUnknown},
			{ID: "m3", Sender: "preview_user", Recipient: "cipher_monk", Body: body, Created: "Sample", Encrypted: true, RecipientMatch: matchNo},
		}
	})
}

// recipientMatch compares the message's recipient key IDs with the recipient's key (nil = none saved).
func recipientMatch(ids []uint64, key *openpgp.Entity) string {
	if key == nil {
		return matchUnknown
	}
	own := entityKeyIDs(key)
	hidden := len(ids) == 0 // passphrase-only encryption names no recipient
	for _, id := range ids {
		if slices.Contains(own, id) {
			return matchYes
		}
		hidden = hidden || id == 0 // a hidden recipient could be the recipient's key
	}
	if hidden {
		return matchUnknown
	}
	return matchNo
}

// messageStatus inspects an armored body. It returns encrypted=false for anything that is not an
// OpenPGP-encrypted message (plaintext, signed-only, malformed or fake armor).
func messageStatus(body, recipientKey string) (encrypted bool, match string) {
	encrypted, match, _, _ = messageStatusDetails(body, recipientKey)
	return encrypted, match
}

// messageStatusDetails also counts distinct, visible public-key recipient IDs other than the recipient's saved
// primary/subkey IDs. Hidden-recipient packets are not counted, but do not hide visible additional recipients.
func messageStatusDetails(body, recipientKey string) (encrypted bool, match string, extraKeys int, extraKeysKnown bool) {
	ids, encrypted, err := inspectEncrypted(body)
	if err != nil || !encrypted {
		return false, "", 0, false
	}
	key, _, err := parsePublicKey(recipientKey)
	if err != nil {
		key = nil
	}
	match = recipientMatch(ids, key)
	extraKeys, extraKeysKnown = recipientExtraKeyCount(ids, key)
	return true, match, extraKeys, extraKeysKnown
}

func recipientExtraKeyCount(ids []uint64, key *openpgp.Entity) (int, bool) {
	if key == nil || len(ids) == 0 {
		return 0, false
	}
	own := make(map[uint64]struct{}, len(key.Subkeys)+1)
	own[key.PrimaryKey.KeyId] = struct{}{}
	for _, subkey := range key.Subkeys {
		own[subkey.PublicKey.KeyId] = struct{}{}
	}
	others := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 { // Do not count a hidden recipient or let it suppress visible additional recipients.
			continue
		}
		if _, recipient := own[id]; !recipient {
			others[id] = struct{}{}
		}
	}
	return len(others), true
}

// inspectMessage is called by the /messages action before storing: it rejects bodies that are not
// OpenPGP-encrypted and returns the body to store - a canonical re-armor of the pasted packets, without
// armor headers or anything around the block - and the recipient-key match computed from that body.
//
// A-569 (policy: docs/data-retention-privacy-policy.md, private channels): only ciphertext the recipient can be
// expected to read is stored. A recipient whose saved key cannot receive encrypted messages now (canReceiveEncrypted)
// is refused with 409; a passphrase-only message (no public-key session packet) and one whose visible recipient key
// IDs are all other keys with 400. A message whose recipient IDs are all hidden (gpg --throw-keyids) cannot be checked:
// it is stored as matchUnknown only when the sender ticked hidden_recipients, and otherwise the compose form comes
// back with the ciphertext kept (hiddenRecipientsForm). The match recorded is therefore yes or unknown, never no.
func inspectMessage(c *actionCtx, body, recipientID, handle string) (stored, match string, extraKeys int, extraKeysKnown bool, err error) {
	var ids []uint64
	encrypted := false
	if stored, err = canonicalMessage(body); err == nil {
		ids, encrypted, err = inspectEncrypted(stored)
	}
	if err != nil || !encrypted {
		return "", "", 0, false, fail(400, "This is not an OpenPGP-encrypted message (plaintext, signed-only and malformed armor are rejected). Encrypt it locally to the recipient's public key, then paste the armored result.")
	}
	p, err := loadPGPAccount(c.Ctx(), c.Tx, recipientID, false)
	if err != nil {
		return "", "", 0, false, err
	}
	const ask = " Nothing was stored. Ask %[1]s to add a public key that can receive encrypted messages, then encrypt to it."
	switch {
	case canReceiveEncrypted(p.Key, time.Now()):
	case p.Key != nil:
		return "", "", 0, false, fail(409, fmt.Sprintf("Message not sent: %[1]s’s saved PGP key cannot receive encrypted messages (it has expired or been revoked, or has no current encryption subkey)."+ask, handle))
	case p.Armored != "":
		return "", "", 0, false, fail(409, fmt.Sprintf("Message not sent: %[1]s’s saved PGP key can no longer be read."+ask, handle))
	default:
		return "", "", 0, false, fail(409, fmt.Sprintf("Message not sent: %[1]s has not saved a PGP public key, so nothing can be encrypted to them."+ask, handle))
	}
	fingerprint := formatFingerprint(p.Fingerprint)
	redo := fmt.Sprintf(" Nothing was stored. Encrypt it to %s’s key %s and paste the result.", handle, fingerprint)
	match = recipientMatch(ids, p.Key)
	switch {
	case len(ids) == 0:
		return "", "", 0, false, fail(400, "Message not sent: it is encrypted with a passphrase only and names no recipient key, so "+handle+" could not read it without that passphrase."+redo)
	case match == matchYes:
	case slices.ContainsFunc(ids, func(id uint64) bool { return id != 0 }):
		// A visible key ID that is not the recipient's: a hidden one beside it does not make the message checkable.
		return "", "", 0, false, fail(400, "Message not sent: its visible recipient keys are not "+handle+"’s; it names only other keys, so "+handle+" could not read it."+redo+
			" To hide recipients, hide every recipient key ID (gpg --throw-keyids).")
	case c.Form.Get("hidden_recipients") != "confirmed":
		return "", "", 0, false, hiddenRecipientsForm(c, handle, fingerprint, stored)
	}
	extraKeys, extraKeysKnown = recipientExtraKeyCount(ids, p.Key)
	return stored, match, extraKeys, extraKeysKnown, nil
}

// hiddenRecipientsForm answers a message whose recipient key IDs are all hidden, sent without the confirmation, with
// 400 and the compose form again (A-569): the explanation in its error alert, the recipient chosen and the ciphertext
// kept (canonical, as it would be stored), and a confirmation box to tick, so a browser without JavaScript sends it
// with one more submission. When the page cannot be loaded, the shared refusal page says to send it again from a
// reloaded page, and echoes the text.
func hiddenRecipientsForm(c *actionCtx, handle, fingerprint, body string) error {
	msg := "Message not sent yet. The recipient key IDs are hidden (as gpg --throw-keyids writes them), so the server cannot check that it is encrypted to " +
		handle + "’s key with fingerprint " + fingerprint + ". Nothing was stored. If you encrypted it to that key, tick the confirmation below and send again; it will be marked “recipient unknown”."
	return &httpError{Code: 400, Msg: msg, Render: func(w http.ResponseWriter) {
		a := c.A
		r := c.R.Clone(c.R.Context())
		r.URL.RawQuery = url.Values{"to": {handle}}.Encode()
		d := a.pageData(r, "messages", c.Token, c.User)
		if err := a.load(r, &d); err != nil {
			a.refuse(w, c.R, c.User, 400, "Message not sent: its recipient key IDs are hidden, so the server cannot check its recipient. Nothing was stored. Go back, reload the page and send it again to be asked for confirmation.")
			return
		}
		d.Error, d.MessageBody, d.MessageHiddenRecipients = msg, body, true
		a.renderStatus(w, c.R, d, 400)
	}}
}

// messageStatusLoader adds the stored inspection result to each listed message. Messages stored before
// inspection existed (encrypted IS NULL) are inspected now; their recipient match is reported as unknown.
// An encrypted message is displayed as its canonical re-armor, never with the stored armor headers.
func messageStatusLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if len(d.Messages) == 0 {
		return nil
	}
	ids := make([]string, len(d.Messages))
	for i, m := range d.Messages {
		ids[i] = m.ID
	}
	rows, err := a.db.QueryContext(ctx, "SELECT id,encrypted,recipient_match,recipient_extra_keys FROM messages WHERE id=ANY($1)", ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	type status struct {
		encrypted sql.NullBool
		match     string
		extraKeys sql.NullInt64
	}
	found := map[string]status{}
	for rows.Next() {
		var id string
		var s status
		if err = rows.Scan(&id, &s.encrypted, &s.match, &s.extraKeys); err != nil {
			return err
		}
		found[id] = s
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range d.Messages {
		m := &d.Messages[i]
		s, ok := found[m.ID]
		switch {
		case ok && s.encrypted.Valid:
			m.Encrypted, m.RecipientMatch = s.encrypted.Bool, s.match
			if s.extraKeys.Valid && s.extraKeys.Int64 >= 0 {
				m.RecipientExtraKeys, m.RecipientExtraKeysSet = int(s.extraKeys.Int64), true
			}
			// Rows stored before the strict armor check (A-67) may hold text around or inside the armor.
			if m.Encrypted {
				_, strict, err := inspectEncrypted(m.Body)
				m.Encrypted = err == nil && strict
			}
		default:
			m.Encrypted, m.RecipientMatch = messageStatus(m.Body, "")
		}
		if m.Encrypted {
			// Show only the packets: a pre-A-67 row may carry plaintext in an armor header (A-74).
			m.Body, _ = canonicalMessage(m.Body)
		} else {
			m.RecipientMatch = ""
		}
	}
	return nil
}
