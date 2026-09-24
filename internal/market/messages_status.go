package market

import (
	"context"
	"database/sql"
	"net/http"
	"slices"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// P2: message encryption status comes from OpenPGP packet inspection (pgp.go inspectEncrypted), never
// from the armor header alone. The server cannot decrypt, so "to recipient's key" means a public-key
// session packet names one of the recipient's saved key IDs.

const (
	matchYes     = "yes"     // a recipient key ID in the message belongs to the recipient's saved key
	matchNo      = "no"      // the message names only other keys
	matchUnknown = "unknown" // recipient has no usable key, hidden recipients, or passphrase-only encryption
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
	ids, encrypted, err := inspectEncrypted(body)
	if err != nil || !encrypted {
		return false, ""
	}
	key, _, err := parsePublicKey(recipientKey)
	if err != nil {
		key = nil
	}
	return true, recipientMatch(ids, key)
}

// inspectMessage is called by the /messages action before storing: it rejects bodies that are not
// OpenPGP-encrypted and returns the body to store - a canonical re-armor of the pasted packets, without
// armor headers or anything around the block - and the recipient-key match computed from that body.
func inspectMessage(c *actionCtx, body, recipientID string) (stored, match string, err error) {
	var key string
	if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT pgp FROM users WHERE id=$1", recipientID).Scan(&key); err != nil {
		return "", "", err
	}
	encrypted := false
	if stored, err = canonicalMessage(body); err == nil {
		encrypted, match = messageStatus(stored, key)
	}
	if !encrypted {
		return "", "", fail(400, "This is not an OpenPGP-encrypted message (plaintext, signed-only and malformed armor are rejected). Encrypt it locally to the recipient's public key, then paste the armored result.")
	}
	return stored, match, nil
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
	rows, err := a.db.QueryContext(ctx, "SELECT id,encrypted,recipient_match FROM messages WHERE id=ANY($1)", ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	type status struct {
		encrypted sql.NullBool
		match     string
	}
	found := map[string]status{}
	for rows.Next() {
		var id string
		var s status
		if err = rows.Scan(&id, &s.encrypted, &s.match); err != nil {
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
