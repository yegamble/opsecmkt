package market

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// /escrow-key (WR-escrow Phase 1, ES-3 and ES-9): register or change the account's Bitcoin escrow key, and activate
// it by signing the practice transaction. The page and the /account status row appear only with ESCROW_BTC=on; with
// it off, /escrow-key and its actions answer exactly as unknown ones do, signed in or not (pageSpec.Off).
// Registering or changing a key needs the current password (and authenticator code when enrolled), notifies the
// owner and writes a fixed audit line with no key, fingerprint or path. A change replaces only this account's key
// row: an escrow keeps the child keys and origins it was built with (escrow_pubkeys), so no existing escrow changes.
//
// Which escrows use a key is read by the account and the key's origin (escrow_pubkeys.user_id, origin_fingerprint and
// origin_path), never by key_hmac alone: the HMAC derives from APP_SECRET_KEY, and rotating it must never hide an
// escrow or change which key an escrow uses (the chair's ruling on QA25). An origin names one key of an active account:
// the practice signature proved the owner's wallet holds the key at that fingerprint and path. Once a key has escrows
// its origin cannot change (keyAgainstEscrows, A-314), so a key with escrows has one origin. After a rotation the
// owner registers the same key again; Bitcoin Core confirms it derives the key an existing escrow holds
// (derivesChild), and it is active again at once, since practice is never offered for a key with escrows (ES-9).
// Cross-account reuse is refused by the HMAC, so across a rotation it is best effort: a key registered before is not
// recognised on another account until its owner registers it again (docs/operator-guide.md).

func init() {
	registerPage("escrow-key", pageSpec{Protected: true, Off: escrowOff})
	registerLoader("escrow-key", accountSecurityLoader) // TOTPEnabled: the key form asks for the authenticator code
	registerLoader("escrow-key", escrowKeyLoader)
	registerLoader("account", escrowAccountLoader)
	registerAction("/escrow-key", actionSpec{OwnTx: true, Off: escrowOff, Run: escrowKeyAction})
	registerAction("/escrow-key/practice", actionSpec{OwnTx: true, OneAtATime: true, Off: escrowOff, Run: escrowPracticeAction})
}

// escrowOff is ESCROW_BTC off: the escrow page and actions do not exist.
func escrowOff(a *App) bool { return !a.escrowConfigured }

var (
	errEscrowDown      = fail(409, "Bitcoin escrow is unavailable on this market right now: the operator must fix its Bitcoin node or arbiter wallet. Nothing was changed.")
	errKeyAck          = &keyRefusal{"This key is not on the standard multisig path m/48h/1h/<account>h/2h. Register it only if the wallet (or wallet account) it comes from is used only for this market: tick the box and send the form again. Nothing was saved."}
	errKeyUnusable     = &keyRefusal{"This key cannot be used here. Create a new wallet, or a new account in your wallet, used only for this market, and register its key. Nothing was saved."}
	errPracticeState   = fail(409, "The practice transaction is offered only for a registered key that is not active yet and has no escrows. Reload the page.")
	errPracticeChanged = fail(409, "Your escrow key was changed or activated while the practice transaction was checked; nothing was changed. Reload the page.")
	errKeyOtherOrigin  = &keyRefusal{"This key has the same origin as the key your escrows use, but it is a different key. Check the fingerprint and path your wallet shows for this key. Nothing was saved."}
	errKeyOriginLocked = fail(409, "This key has escrows, so its origin (fingerprint and path) cannot be changed. Its existing escrows keep the origin they were created with. If that origin is wrong, contact the market's staff. Nothing was saved.")
	errKeyUnchecked    = fail(503, "The key cannot be checked right now: the market's Bitcoin node did not answer. Nothing was saved; try again later.")
)

// errEscrowKeyUnreadable is shown when the stored key cannot be opened: APP_SECRET_KEY (or a legacy SETUP_TOKEN)
// was changed since it was registered (docs/operator-guide.md). errKeySealed is that failure inside practiceFor.
const errEscrowKeyUnreadable = "Your escrow key can no longer be read on this market; register it again."

var errKeySealed = errors.New("escrow: stored escrow key cannot be opened")

// escrowKeyHMAC is user_escrow_keys.xpub_hmac: HMAC-SHA256 of the key's chain code and public key under a key derived
// from keyRoot, so the same key registered with other version bytes or origin metadata still collides.
func (a *App) escrowKeyHMAC(k escrowKey) string {
	key := sha256.Sum256([]byte("escrow-xpub-hmac:" + a.keyRoot))
	m := hmac.New(sha256.New, key[:])
	m.Write(k.ChainCode[:])
	m.Write(k.PubKey[:])
	return hex.EncodeToString(m.Sum(nil))
}

// storedEscrowKey is the viewer's user_escrow_keys row.
type storedEscrowKey struct {
	sealed, hmac, fingerprint, path, activeAt string
	active, hasEscrows                        bool
}

func loadEscrowKey(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID string) (*storedEscrowKey, error) {
	k := &storedEscrowKey{}
	err := q.QueryRowContext(ctx, `SELECT xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,coalesce(to_char(active_at,'YYYY-MM-DD HH24:MI "UTC"'),''),
		active_at IS NOT NULL,EXISTS(`+escrowsOfKey+`)
		FROM user_escrow_keys k WHERE user_id=$1`, userID).Scan(&k.sealed, &k.hmac, &k.fingerprint, &k.path, &k.activeAt, &k.active, &k.hasEscrows)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return k, err
}

// escrowsOfKey selects the escrow keys of the user_escrow_keys row k: by account and origin, or by the key's HMAC. The
// HMAC only adds matches (a rotation can hide none): a key has one origin once it has escrows (keyAgainstEscrows).
const escrowsOfKey = "SELECT 1 FROM escrow_pubkeys p WHERE p.user_id=k.user_id AND (p.origin_fingerprint=k.origin_fingerprint AND p.origin_path=k.origin_path OR p.key_hmac=k.xpub_hmac)"

// keyAgainstEscrows checks key k (HMAC h), offered for account uid, against each key the account's escrows hold (the
// latest child key recorded under each origin), and reports whether k is the key of existing escrows at their
// origin: restored, active at once. Whether k is a recorded key is decided by the HMAC, or, where it differs (an
// APP_SECRET_KEY rotation changes every HMAC), by Core deriving the recorded child key from k; never by the origin
// (A-314). A recorded key under another origin is refused (errKeyOriginLocked): its child indices continue per
// account, but practice is never offered for a key with escrows (ES-9), and the escrows keep the origin recorded
// with them. Another key under the origin of escrows is refused too (errKeyOtherOrigin). Before any escrow exists
// there is nothing to check, so an origin can still be corrected (A-309).
func (a *App) keyAgainstEscrows(ctx context.Context, e *escrowBTC, uid string, k escrowKey, h string) (bool, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT DISTINCT ON (origin_fingerprint,origin_path) origin_fingerprint,origin_path,derivation_suffix,pubkey,key_hmac
		FROM escrow_pubkeys WHERE user_id=$1 ORDER BY origin_fingerprint,origin_path,escrow_id DESC`, uid)
	if err != nil {
		return false, err
	}
	type recorded struct{ fp, path, suffix, pub, hmac string }
	var keys []recorded
	for rows.Next() {
		var r recorded
		if err = rows.Scan(&r.fp, &r.path, &r.suffix, &r.pub, &r.hmac); err != nil {
			rows.Close()
			return false, err
		}
		keys = append(keys, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	// The HMAC first: a key recognised by it is refused or restored without asking the node.
	restored := false
	for _, r := range keys {
		switch sameOrigin := r.fp == k.Fingerprint && r.path == k.PathText; {
		case r.hmac == h && !sameOrigin:
			return false, errKeyOriginLocked
		case r.hmac == h:
			restored = true
		}
	}
	for _, r := range keys {
		if r.hmac == h {
			continue
		}
		same, err := e.derivesChild(ctx, k, r.suffix, r.pub)
		if err != nil {
			log.Printf("escrow: re-registered key not checked: %s", errorClassOf(err))
			return false, errKeyUnchecked
		}
		switch sameOrigin := r.fp == k.Fingerprint && r.path == k.PathText; {
		case same && !sameOrigin:
			return false, errKeyOriginLocked
		case same:
			restored = true
		case sameOrigin:
			return false, fail(400, errKeyOtherOrigin.Error())
		}
	}
	return restored, nil
}

// escrowKeyView fills the page view from the stored key (no RPC).
func (a *App) escrowKeyView(ctx context.Context, userID string) (*EscrowKeyView, *storedEscrowKey, error) {
	if !a.escrowConfigured {
		return nil, nil, nil
	}
	v := &EscrowKeyView{Available: a.escrow.Load() != nil}
	k, err := loadEscrowKey(ctx, a.db, userID)
	if err != nil || k == nil {
		return v, nil, err
	}
	v.Registered, v.Origin, v.ActiveAt, v.HasEscrows = true, "["+k.fingerprint+k.path+"]", k.activeAt, k.hasEscrows
	_, openErr := a.open("escrow-xpub", k.sealed)
	v.Unreadable = openErr != nil
	// Practice is offered only while the key is inactive and has no escrows (ES-9): once active it is never offered
	// again, so a server taken over later cannot pass off a real spend as practice.
	v.PracticeOffered = v.Available && !k.active && !k.hasEscrows && !v.Unreadable
	return v, k, nil
}

func escrowAccountLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	v, _, err := a.escrowKeyView(ctx, d.User.ID)
	d.EscrowKey = v
	return err
}

// escrowKeyLoader also builds the practice PSBT when ?practice=1 asks for it (a few node RPCs, none to a wallet).
func escrowKeyLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	v, k, err := a.escrowKeyView(ctx, d.User.ID)
	d.EscrowKey = v
	if err == nil && v != nil && v.Unreadable {
		log.Print(errKeySealed)
	}
	if err != nil || v == nil || !v.PracticeOffered || r.URL.Query().Get("practice") != "1" {
		return err
	}
	pt, err := a.practiceFor(ctx, d.User.ID, k)
	if err != nil {
		log.Printf("escrow: practice transaction not built: %s", errorClassOf(err))
		v.PracticeError = "The practice transaction cannot be built right now: the market's Bitcoin node did not answer. Try again later."
		return nil
	}
	v.Practice = pt.PSBT
	return nil
}

// practiceFor rebuilds the practice transaction of the stored key k.
func (a *App) practiceFor(ctx context.Context, userID string, k *storedEscrowKey) (*practiceTx, error) {
	xpub, err := a.open("escrow-xpub", k.sealed)
	if err != nil {
		return nil, errKeySealed
	}
	key, err := parseEscrowKey("[" + k.fingerprint + k.path + "]" + xpub)
	if err != nil {
		return nil, err
	}
	return a.escrow.Load().buildPractice(ctx, key, a.practiceSeed(userID, k.hmac))
}

// escrowKeyAction registers or changes the account's escrow key. The key is parsed (ES-3) before the password is
// checked or anything else is read, so a pasted private key is refused at once and goes nowhere.
func escrowKeyAction(c *actionCtx) (actionResult, error) {
	a := c.A
	e := a.escrow.Load()
	if e == nil {
		return actionResult{}, errEscrowDown
	}
	k, err := parseEscrowKey(c.Form.Get("key"))
	if err != nil {
		return actionResult{}, fail(400, err.Error())
	}
	if !k.BIP48 && c.Form.Get("dedicated") != "1" {
		return actionResult{}, fail(400, errKeyAck.Error())
	}
	if k.Fingerprint == e.fingerprint { // the market's own arbiter key offered as a party key
		return actionResult{}, fail(400, errKeyUnusable.Error())
	}
	h := a.escrowKeyHMAC(k)
	cur, err := loadEscrowKey(c.Ctx(), a.db, c.User.ID)
	if err != nil {
		return actionResult{}, err
	}
	if cur != nil && cur.hmac == h && cur.fingerprint == k.Fingerprint && cur.path == k.PathText {
		return actionResult{Redirect: "/escrow-key", Notice: "escrow-key-unchanged"}, nil
	}
	// Checked against the keys this account's escrows hold, before the password is.
	restored, err := a.keyAgainstEscrows(c.Ctx(), e, c.User.ID, k, h)
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, uid := c.Ctx(), c.User.ID
		sealed, err := c.A.seal("escrow-xpub", k.XPub)
		if err != nil {
			return actionResult{}, err
		}
		var inserted bool
		var activeAt any
		if restored {
			activeAt = "now"
		}
		err = c.Tx.QueryRowContext(ctx, `INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at) VALUES($1,$2,$3,$4,$5,$6::timestamptz)
			ON CONFLICT (user_id) DO UPDATE SET xpub_sealed=EXCLUDED.xpub_sealed,xpub_hmac=EXCLUDED.xpub_hmac,origin_fingerprint=EXCLUDED.origin_fingerprint,
			origin_path=EXCLUDED.origin_path,active_at=EXCLUDED.active_at,updated=now() RETURNING xmax=0`, uid, sealed, h, k.Fingerprint, k.PathText, activeAt).Scan(&inserted)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "user_escrow_keys_xpub_hmac_key" {
			return actionResult{}, fail(409, errKeyUnusable.Error()) // another account's key; which one is never said
		}
		if err != nil {
			return actionResult{}, err
		}
		// An escrow a checkout committed for this key since keyAgainstEscrows ran (the checkout held this key row FOR
		// UPDATE, so the upsert above waited for it) is seen here: the origin of a key with escrows still cannot change.
		var moved bool
		if err = c.Tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM escrow_pubkeys WHERE user_id=$1 AND key_hmac=$2
			AND NOT (origin_fingerprint=$3 AND origin_path=$4))`, uid, h, k.Fingerprint, k.PathText).Scan(&moved); err != nil {
			return actionResult{}, err
		}
		if moved {
			return actionResult{}, errKeyOriginLocked
		}
		audit, event := "Changed Bitcoin escrow key", "Your Bitcoin escrow key was changed; it is inactive until you sign its practice transaction."
		if inserted {
			audit, event = "Registered a Bitcoin escrow key", "A Bitcoin escrow key was registered on your account; it is inactive until you sign its practice transaction."
		}
		if restored {
			audit += " (the key of your existing escrows; active)"
			event = "Your Bitcoin escrow key was registered again: it is the key your existing escrows use, so it is active."
		}
		if !k.BIP48 {
			audit += " (path outside BIP48; wallet used only for this market acknowledged)"
		}
		if err = notifyOwner(ctx, c.Tx, uid, event); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/escrow-key", Notice: "escrow-key", Audit: audit}, nil
	})
}

// escrowPracticeAction checks a pasted practice PSBT (ES-9) and, when it passes, records active_at: the only thing
// stored. The paste itself is never stored, logged or shown again. The node RPCs run before the transaction; the
// update then requires the same key to be still inactive and without escrows.
func escrowPracticeAction(c *actionCtx) (actionResult, error) {
	a, ctx, uid := c.A, c.Ctx(), c.User.ID
	e := a.escrow.Load()
	if e == nil {
		return actionResult{}, errEscrowDown
	}
	pasted := c.Form.Get("psbt")
	if strings.TrimSpace(pasted) == "" {
		return actionResult{}, fail(400, "Paste the signed practice PSBT.")
	}
	k, err := loadEscrowKey(ctx, a.db, uid)
	if err != nil {
		return actionResult{}, err
	}
	if k == nil || k.active || k.hasEscrows {
		return actionResult{}, errPracticeState
	}
	pt, err := a.practiceFor(ctx, uid, k)
	if err == nil {
		err = e.verifyPractice(ctx, pt, pasted)
	}
	var kr *keyRefusal
	var pe *psbtError
	switch {
	case errors.As(err, &kr), errors.As(err, &pe):
		return actionResult{}, fail(400, err.Error())
	case errors.Is(err, errKeySealed):
		log.Print(errKeySealed)
		return actionResult{}, fail(409, errEscrowKeyUnreadable)
	case errors.Is(err, errPracticeUnsafeOut):
		return actionResult{}, fail(409, "The practice transaction could not be confirmed as unbroadcastable, so it was not accepted. Nothing was changed; the operator should check the escrow node.")
	case err != nil:
		log.Printf("escrow: practice check failed: %s", errorClassOf(err))
		return actionResult{}, errPracticeNode
	}
	return confirmedTx(c, false, func(c *actionCtx) (actionResult, error) {
		// The key checked (the same key and origin) must still be the account's, inactive and without escrows.
		res, err := c.Tx.ExecContext(ctx, `UPDATE user_escrow_keys k SET active_at=now(),updated=now() WHERE user_id=$1 AND xpub_hmac=$2
			AND origin_fingerprint=$3 AND origin_path=$4 AND active_at IS NULL AND NOT EXISTS (`+escrowsOfKey+`)`,
			uid, k.hmac, k.fingerprint, k.path)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, errPracticeChanged
		}
		if err = notifyOwner(ctx, c.Tx, uid, "Your Bitcoin escrow key is now active: its practice transaction was signed."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/escrow-key", Notice: "escrow-key-active", Audit: "Activated Bitcoin escrow key (practice transaction signed)"}, nil
	})
}
