package market

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"opsecmkt/internal/btc"
)

// The escrow signer's client (A-454, ruling P4-R17). Per-order (shape (b)) escrows are co-signed by a dedicated signer,
// cmd/escrow-signer, which holds the market's key for them and listens on a Unix socket (ESCROW_SIGNER_SOCKET); the app
// never holds that key. No order uses shape (b) yet: the settlement flow calls sign from A-456. Until then the app only
// checks the signer at startup and on every escrow watcher pass (checkEscrowSigner): it asks for the signer's account
// key, pins its fingerprint and an HMAC of it at first contact, and reports one of four states. A key that differs from
// the pinned one makes shape (b) unavailable (one fixed log line and an admin line; nothing else changes: shape (a)
// escrow, the pins and every order stay as they are). A signer that does not answer is "waiting", not an error: a
// settlement that needs it will wait for it and nothing is rebuilt.

const (
	escrowSignerProtocol       = 1                           // the socket protocol's version field; the signer is upgraded first
	escrowSignerFingerprintPin = "escrow_signer_fingerprint" // settings key: the signer account key's own fingerprint (8 hex)
	escrowSignerXPubPin        = "escrow_signer_xpub_hmac"   // settings key: escrowXPubHMAC of the signer's account key
	escrowSignerInfoTimeout    = 10 * time.Second
	escrowSignerSignTimeout    = 2 * time.Minute

	signerReady    = "ready"        // the signer answered with the pinned key (pinned now if it was the first contact)
	signerWaiting  = "waiting"      // the signer did not answer
	signerMismatch = "pin-mismatch" // the signer's account key differs from the pinned one: shape (b) unavailable
	signerBadReply = "bad-reply"    // the signer's reply was not understood (another protocol version?): shape (b) unavailable

	escrowLogSignerReady    = "escrow: escrow signer ready (shape (b); its account key matches the pins in this database)"
	escrowLogSignerWaiting  = "escrow: escrow signer not reachable at ESCROW_SIGNER_SOCKET; anything that needs it waits (nothing is rebuilt)"
	escrowLogSignerMismatch = "escrow: shape (b) unavailable: the escrow signer's account key differs from the one pinned in this database (escrow_signer_fingerprint, escrow_signer_xpub_hmac); see docs/operator-guide.md, \"Escrow signer\""
	escrowLogSignerBadReply = "escrow: shape (b) unavailable: the escrow signer's reply was not understood (another protocol version? upgrade the signer first); see docs/operator-guide.md, \"Escrow signer\""
)

// errSignerUnreachable is a signer that did not answer: nothing listens on the socket, or it timed out or hung up.
var errSignerUnreachable = errors.New("escrow signer: not reachable")

// signerRefusal is a signer's {"error":"<class>"} reply, or "bad-reply" for a reply that is not the protocol's.
type signerRefusal struct{ Class string }

func (r *signerRefusal) Error() string { return "escrow signer refused: " + r.Class }

// escrowSignerClient speaks the signer's socket protocol: one JSON request line, one JSON reply line, per connection.
type escrowSignerClient struct{ socket string }

// escrowSignerInfo is the signer's info reply, checked: a rooted account tpub and its own fingerprint.
type escrowSignerInfo struct {
	Key         btc.ExtKey
	Fingerprint string
}

func (c *escrowSignerClient) roundTrip(ctx context.Context, timeout time.Duration, req, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return errSignerUnreachable
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err = conn.Write(append(line, '\n')); err != nil {
		return errSignerUnreachable
	}
	reply, err := bufio.NewReaderSize(conn, 64<<10).ReadBytes('\n')
	if err != nil {
		return errSignerUnreachable
	}
	var refused struct {
		Error *string `json:"error"`
	}
	if json.Unmarshal(reply, &refused) != nil {
		return &signerRefusal{signerBadReply}
	}
	if refused.Error != nil {
		return &signerRefusal{*refused.Error}
	}
	if json.Unmarshal(reply, out) != nil {
		return &signerRefusal{signerBadReply}
	}
	return nil
}

// info asks for the signer's account key and checks the reply: the protocol version, a rooted (depth 0) tpub, and a
// fingerprint that is the key's own.
func (c *escrowSignerClient) info(ctx context.Context) (escrowSignerInfo, error) {
	var r struct {
		Version     int    `json:"version"`
		Tpub        string `json:"tpub"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := c.roundTrip(ctx, escrowSignerInfoTimeout, map[string]any{"version": escrowSignerProtocol, "op": "info"}, &r); err != nil {
		return escrowSignerInfo{}, err
	}
	k, err := btc.ParseExtPub(r.Tpub, btc.VersionTpub)
	fp := k.Fingerprint()
	if r.Version != escrowSignerProtocol || err != nil || k.Depth != 0 || r.Fingerprint != hex.EncodeToString(fp[:]) {
		return escrowSignerInfo{}, &signerRefusal{signerBadReply}
	}
	return escrowSignerInfo{Key: k, Fingerprint: r.Fingerprint}, nil
}

// sign asks the signer to co-sign one escrow's PSBT (base64) as the market key of per-order index index, and returns
// the signed PSBT. The signer refuses anything but exactly one 2-of-3 escrow holding its key for that index
// (cmd/escrow-signer/sign.go). No caller yet (A-456).
func (c *escrowSignerClient) sign(ctx context.Context, psbt string, index uint32) (string, error) {
	var r struct {
		PSBT string `json:"psbt"`
	}
	req := map[string]any{"version": escrowSignerProtocol, "op": "sign", "psbt": psbt, "index": index}
	if err := c.roundTrip(ctx, escrowSignerSignTimeout, req, &r); err != nil {
		return "", err
	}
	if r.PSBT == "" {
		return "", &signerRefusal{signerBadReply}
	}
	return r.PSBT, nil
}

// escrowXPubHMAC is HMAC-SHA256 of a key's chain code and public key under the key escrowKeyHMAC uses (derived from
// keyRoot), so a pin reveals nothing about the key without APP_SECRET_KEY.
func (a *App) escrowXPubHMAC(chainCode [32]byte, pubKey [33]byte) string {
	key := sha256.Sum256([]byte("escrow-xpub-hmac:" + a.keyRoot))
	m := hmac.New(sha256.New, key[:])
	m.Write(chainCode[:])
	m.Write(pubKey[:])
	return hex.EncodeToString(m.Sum(nil))
}

// pinEscrowSigner records fp and mac as the signer's pins when none are recorded (one statement, so concurrent first
// contacts agree on one signer) and reports whether both recorded pins equal them. A recorded pin is never changed.
func pinEscrowSigner(ctx context.Context, db *sql.DB, fp, mac string) (bool, error) {
	if _, err := db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2),($3,$4) ON CONFLICT (key) DO NOTHING",
		escrowSignerFingerprintPin, fp, escrowSignerXPubPin, mac); err != nil {
		return false, err
	}
	var pinnedFP, pinnedMAC string
	if err := db.QueryRowContext(ctx, "SELECT (SELECT value FROM settings WHERE key=$1),(SELECT value FROM settings WHERE key=$2)",
		escrowSignerFingerprintPin, escrowSignerXPubPin).Scan(&pinnedFP, &pinnedMAC); err != nil {
		return false, err
	}
	return pinnedFP == fp && pinnedMAC == mac, nil
}

// checkEscrowSigner asks the signer for its account key, checks it against the pins (written at first contact) and
// records the state, logging one fixed line when it changes. No signer configured: nothing. A database failure leaves
// the state as it was and is logged; the next pass checks again.
func (a *App) checkEscrowSigner(ctx context.Context) {
	if a.escrowSigner == nil || a.db == nil {
		return
	}
	state := signerReady
	info, err := a.escrowSigner.info(ctx)
	var refused *signerRefusal
	switch {
	case errors.As(err, &refused):
		state = signerBadReply
	case err != nil:
		state = signerWaiting
	default:
		ok, err := pinEscrowSigner(ctx, a.db, info.Fingerprint, a.escrowXPubHMAC(info.Key.ChainCode, info.Key.PubKey))
		if err != nil {
			log.Printf("escrow: escrow signer pins could not be checked: %s", errorCause(err))
			return
		}
		if !ok {
			state = signerMismatch
		}
	}
	if prev, _ := a.escrowSignerStatus.Swap(state).(string); prev != state {
		log.Print(map[string]string{signerReady: escrowLogSignerReady, signerWaiting: escrowLogSignerWaiting,
			signerMismatch: escrowLogSignerMismatch, signerBadReply: escrowLogSignerBadReply}[state])
	}
}

// escrowSignerState is the signer's state as the latest check found it ("" = no signer configured, or not checked).
func (a *App) escrowSignerState() string {
	s, _ := a.escrowSignerStatus.Load().(string)
	return s
}

func init() { registerLoader("admin", escrowSignerLoader) }

// escrowSignerLoader shows on /admin, under Payment providers, a signer that is not ready.
func escrowSignerLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	if a.escrowSigner == nil || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	if s := a.escrowSignerState(); s != "" && s != signerReady {
		d.EscrowSigner = &EscrowSignerView{State: s}
	}
	return nil
}
