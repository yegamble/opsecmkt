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
// it by signing the practice transaction. The page and the /account status row appear only with ESCROW_BTC=on.
// Registering or changing a key needs the current password (and authenticator code when enrolled), notifies the
// owner and writes a fixed audit line with no key, fingerprint or path. A change replaces only this account's key
// row: an escrow keeps the child keys and origins it was built with (escrow_pubkeys), so no existing escrow changes.

func init() {
	registerPage("escrow-key", pageSpec{Protected: true})
	registerLoader("escrow-key", escrowKeyLoader)
	registerLoader("account", escrowAccountLoader)
	registerAction("/escrow-key", actionSpec{OwnTx: true, Run: escrowKeyAction})
	registerAction("/escrow-key/practice", actionSpec{OwnTx: true, OneAtATime: true, Run: escrowPracticeAction})
}

var (
	errEscrowOff       = fail(404, "Bitcoin escrow is not enabled on this market.")
	errEscrowDown      = fail(409, "Bitcoin escrow is unavailable on this market right now: the operator must fix its Bitcoin node or arbiter wallet. Nothing was changed.")
	errKeyAck          = &keyRefusal{"This key is not on the standard multisig path m/48h/1h/<account>h/2h. Register it only if the wallet (or wallet account) it comes from is used only for this market: tick the box and send the form again. Nothing was saved."}
	errKeyUnusable     = &keyRefusal{"This key cannot be used here. Create a new wallet, or a new account in your wallet, used only for this market, and register its key. Nothing was saved."}
	errPracticeState   = fail(409, "The practice transaction is offered only for a registered key that is not active yet and has no escrows. Reload the page.")
	errPracticeChanged = fail(409, "Your escrow key was changed or activated while the practice transaction was checked; nothing was changed. Reload the page.")
)

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
		active_at IS NOT NULL,EXISTS(SELECT 1 FROM escrow_pubkeys p WHERE p.key_hmac=k.xpub_hmac)
		FROM user_escrow_keys k WHERE user_id=$1`, userID).Scan(&k.sealed, &k.hmac, &k.fingerprint, &k.path, &k.activeAt, &k.active, &k.hasEscrows)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return k, err
}

// escrowKeyView fills the page view from the stored key (no RPC).
func (a *App) escrowKeyView(ctx context.Context, userID string) (*EscrowKeyView, *storedEscrowKey, error) {
	if !a.escrowConfigured {
		return nil, nil, nil
	}
	v := &EscrowKeyView{Available: a.escrow != nil}
	k, err := loadEscrowKey(ctx, a.db, userID)
	if err != nil || k == nil {
		return v, nil, err
	}
	v.Registered, v.Origin, v.ActiveAt, v.HasEscrows = true, "["+k.fingerprint+k.path+"]", k.activeAt, k.hasEscrows
	// Practice is offered only while the key is inactive and has no escrows (ES-9): once active it is never offered
	// again, so a server taken over later cannot pass off a real spend as practice.
	v.PracticeOffered = v.Available && !k.active && !k.hasEscrows
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
		return nil, err
	}
	key, err := parseEscrowKey("[" + k.fingerprint + k.path + "]" + xpub)
	if err != nil {
		return nil, err
	}
	return a.escrow.buildPractice(ctx, key, a.practiceSeed(userID, k.hmac))
}

// escrowKeyAction registers or changes the account's escrow key. The key is parsed (ES-3) before the password is
// checked or anything else is read, so a pasted private key is refused at once and goes nowhere.
func escrowKeyAction(c *actionCtx) (actionResult, error) {
	a := c.A
	if !a.escrowConfigured {
		return actionResult{}, errEscrowOff
	}
	if a.escrow == nil {
		return actionResult{}, errEscrowDown
	}
	k, err := parseEscrowKey(c.Form.Get("key"))
	if err != nil {
		return actionResult{}, fail(400, err.Error())
	}
	if !k.BIP48 && c.Form.Get("dedicated") != "1" {
		return actionResult{}, fail(400, errKeyAck.Error())
	}
	if k.Fingerprint == a.escrow.fingerprint { // the market's own arbiter key offered as a party key
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
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, uid := c.Ctx(), c.User.ID
		sealed, err := c.A.seal("escrow-xpub", k.XPub)
		if err != nil {
			return actionResult{}, err
		}
		var inserted bool
		err = c.Tx.QueryRowContext(ctx, `INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT (user_id) DO UPDATE SET xpub_sealed=EXCLUDED.xpub_sealed,xpub_hmac=EXCLUDED.xpub_hmac,origin_fingerprint=EXCLUDED.origin_fingerprint,
			origin_path=EXCLUDED.origin_path,active_at=NULL,updated=now() RETURNING xmax=0`, uid, sealed, h, k.Fingerprint, k.PathText).Scan(&inserted)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "user_escrow_keys_xpub_hmac_key" {
			return actionResult{}, fail(409, errKeyUnusable.Error()) // another account's key; which one is never said
		}
		if err != nil {
			return actionResult{}, err
		}
		audit, event := "Changed Bitcoin escrow key", "Your Bitcoin escrow key was changed; it is inactive until you sign its practice transaction."
		if inserted {
			audit, event = "Registered a Bitcoin escrow key", "A Bitcoin escrow key was registered on your account; it is inactive until you sign its practice transaction."
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
	if !a.escrowConfigured {
		return actionResult{}, errEscrowOff
	}
	if a.escrow == nil {
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
		err = a.escrow.verifyPractice(ctx, pt, pasted)
	}
	var kr *keyRefusal
	var pe *psbtError
	switch {
	case errors.As(err, &kr), errors.As(err, &pe):
		return actionResult{}, fail(400, err.Error())
	case errors.Is(err, errPracticeUnsafeOut):
		return actionResult{}, fail(409, "The practice transaction could not be confirmed as unbroadcastable, so it was not accepted. Nothing was changed; the operator should check the escrow node.")
	case err != nil:
		log.Printf("escrow: practice check failed: %s", errorClassOf(err))
		return actionResult{}, errPracticeNode
	}
	return confirmedTx(c, false, func(c *actionCtx) (actionResult, error) {
		res, err := c.Tx.ExecContext(ctx, `UPDATE user_escrow_keys SET active_at=now(),updated=now() WHERE user_id=$1 AND xpub_hmac=$2 AND active_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM escrow_pubkeys WHERE key_hmac=$2)`, uid, k.hmac)
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
