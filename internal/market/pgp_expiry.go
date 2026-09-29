package market

import (
	"context"
	"log"
	"strconv"
	"time"
)

// A-155: every running process checks the saved keys once a day (and when it starts) and tells each owner once when
// encryption to their key stops within keyExpiryWarning, and once when it has stopped because the key or its
// encryption subkey expired. users.pgp_expiry_notice records the last notice sent for a key and expiry, so a notice is
// never repeated, and a renewed or replaced key (a new expiry or fingerprint) is warned about again.

func init() { registerBackground(keyExpirySweeper) }

// keyExpirySweepInterval is how often the saved keys are checked. A variable only so tests can shorten it.
var keyExpirySweepInterval = 24 * time.Hour

func keyExpirySweeper(ctx context.Context, a *App) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sweepKeyExpiry(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("key expiry sweep: %s", errorCause(err)) // a class, never the driver's text (A-170)
		}
		t.Reset(keyExpirySweepInterval)
	}
}

// sweepKeyExpiry sends the expiry notices due at now. Each is recorded and sent in one transaction that applies only
// while the key and the recorded notice are still those read, so concurrent sweeps send it once.
func (a *App) sweepKeyExpiry(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	type saved struct{ id, armored, notice string }
	rows, err := a.db.QueryContext(ctx, "SELECT id,pgp,pgp_expiry_notice FROM users WHERE pgp<>''")
	if err != nil {
		return err
	}
	var keys []saved
	for rows.Next() {
		var k saved
		if err = rows.Scan(&k.id, &k.armored, &k.notice); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		key, fp, _, err := parseCanonicalPublicKey(k.armored)
		if err != nil {
			continue // an unreadable key is reported on the owner's pages
		}
		enc, ok := encryptionSubkey(key, now)
		if !ok || enc.Until.IsZero() || enc.Until.Sub(now) > keyExpiryWarning {
			continue
		}
		marker := fp + "/" + strconv.FormatInt(enc.Until.Unix(), 10)
		stage := "/soon"
		if !enc.Until.After(now) {
			stage = "/expired"
		}
		if k.notice == marker+stage || k.notice == marker+"/expired" {
			continue
		}
		if err = a.sendKeyExpiryNotice(ctx, k.id, k.armored, k.notice, marker+stage, keyExpiryNotice(enc.Until, stage == "/expired")); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) sendKeyExpiryNotice(ctx context.Context, userID, armored, was, notice, body string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE users SET pgp_expiry_notice=$1 WHERE id=$2 AND pgp=$3 AND pgp_expiry_notice=$4", notice, userID, armored, was)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), userID, body); err != nil {
		return err
	}
	return tx.Commit()
}

// keyExpiryNotice is the owner's notification. Like notifyOwner's it is link-free and names no fingerprint: the
// renewal commands say where to find it.
func keyExpiryNotice(until time.Time, expired bool) string {
	day := until.UTC().Format(time.DateOnly) + " UTC"
	head := "Encryption to your PGP key stops on " + day + ", when the key or its encryption subkey expires. After that nobody can encrypt messages or shipping addresses to it, and PGP sign-in, if on, cannot send you a code."
	if expired {
		head = "Encryption to your PGP key stopped on " + day + ": the key or its encryption subkey expired. Nobody can encrypt messages or shipping addresses to it, and PGP sign-in, if on, cannot send you a code."
	}
	return head + " Renew the same key in your own PGP software: gpg --quick-set-expire FINGERPRINT 2y renews the key and gpg --quick-set-expire FINGERPRINT 2y '*' its subkeys, where FINGERPRINT is the one on your account page." +
		" Then export it with gpg --armor --export FINGERPRINT and paste it on your account page with your password: the same key renewed keeps its ownership proof and PGP sign-in."
}
