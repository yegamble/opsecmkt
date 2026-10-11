package market

import (
	"os"
	"strings"
	"testing"
)

// A-452: a changed key root cannot authenticate the immutable arbiter pin. Keep the
// existing escrow and pin intact, refuse key changes, and recover with the original root.
func TestEscrowArbiterPinRequiresOriginalKeyRoot(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	original := os.Getenv("APP_SECRET_KEY")
	pin := agStr(x.testEnv, "SELECT value FROM settings WHERE key='escrow_btc_arbiter_xpub_hmac'")
	rows := func() string {
		return strings.Join(agStrings(t, x.testEnv, "SELECT e::text FROM escrows e ORDER BY id"), "\n") +
			strings.Join(agStrings(t, x.testEnv, "SELECT p::text FROM escrow_pubkeys p ORDER BY escrow_id,party"), "\n")
	}
	before := rows()
	t.Setenv("APP_SECRET_KEY", rotatedSecretKey())
	x.restart()
	if x.A.escrow.Load() != nil || agStr(x.testEnv, "SELECT disabled_class FROM bitcoin_escrow_health WHERE singleton") != "arbiter_key_mismatch" {
		t.Fatal("changed root did not disable escrow at its arbiter pin")
	}
	calls := len(x.core.methods())
	if code, _ := registerKey(x.testEnv, x.buyerSess, x.buyerKey); code < 400 {
		t.Fatalf("key registration accepted while the arbiter pin could not be verified: %d", code)
	}
	if len(x.core.methods()) != calls || rows() != before || agStr(x.testEnv, "SELECT value FROM settings WHERE key='escrow_btc_arbiter_xpub_hmac'") != pin {
		t.Fatal("failed pin verification changed escrow rows/pin or made key-registration RPC calls")
	}
	t.Setenv("APP_SECRET_KEY", original)
	x.restart()
	if x.A.escrow.Load() == nil || rows() != before || agStr(x.testEnv, "SELECT value FROM settings WHERE key='escrow_btc_arbiter_xpub_hmac'") != pin {
		t.Fatal("restoring the original root failed to recover the unchanged escrow and pin")
	}
	order2 := x.draft()
	x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	if suffix := agStrings(t, x.testEnv, "SELECT party||derivation_suffix FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 AND party<>'market' ORDER BY party", order2); strings.Join(suffix, ",") != "buyer/0/2,vendor/0/2" {
		t.Fatalf("recovery reused or skipped a party child: %v", suffix)
	}
}

// A-452 SEC-9: the arbiter's 4-byte fingerprint alone cannot distinguish two account keys from
// the same seed. Pin the account-key material with the existing keyRoot-derived HMAC.
func TestEscrowArbiterAccountKeyPinUpgradeAndSubstitution(t *testing.T) {
	const arbiterKeyPin = "escrow_btc_arbiter_xpub_hmac"
	const keyMismatchLog = "escrow: Bitcoin escrow disabled: the arbiter account key differs from the one pinned in this database"
	logs := captureLog(t)
	core, srv := newFakeEscrowCore(t)
	setEscrowEnv(t, srv.url(""))
	a, dsn, err := startApp(t, "")
	if err != nil || a.escrow.Load() == nil {
		t.Fatalf("initial escrow start: %v", err)
	}
	key, err := parseEscrowKey("[" + fakeArbiterFP + "/84h/1h/0h]" + testBIP84Key[20:])
	if err != nil {
		t.Fatal(err)
	}
	want := a.escrowXPubHMAC(key.ChainCode, key.PubKey)
	var fp, pin string
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key=$1", escrowFingerprintPin).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key=$1", arbiterKeyPin).Scan(&pin); err != nil || fp != fakeArbiterFP || pin != want {
		t.Fatalf("first startup pins: fingerprint=%q key-pin=%q err=%v", fp, pin, err)
	}
	// A legacy database has only the fingerprint row. The first upgrade startup backfills a key pin
	// only after the live fingerprint matches that existing row.
	if _, err = a.db.Exec("DELETE FROM settings WHERE key=$1", arbiterKeyPin); err != nil {
		t.Fatal(err)
	}
	a.Close()
	core.arbiterFP = "0badf00d"
	core.arbiterDesc = "wpkh([0badf00d/84h/1h/0h]" + testBIP84Key[20:] + "/1/*)#different-fingerprint"
	a, _, err = startApp(t, dsn)
	if err != nil || a.escrow.Load() != nil {
		t.Fatalf("changed fingerprint on legacy database was not refused: %v", err)
	}
	var got int
	if err = a.db.QueryRow("SELECT count(*) FROM settings WHERE key=$1", arbiterKeyPin).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("a mismatched fingerprint was given a new key pin: %d", got)
	}
	a.Close()
	core.arbiterFP = fakeArbiterFP
	core.arbiterDesc = "wpkh([" + fakeArbiterFP + "/84h/1h/0h]" + testBIP84Key[20:] + "/1/*)#fakechk0"
	a, _, err = startApp(t, dsn)
	if err != nil || a.escrow.Load() == nil {
		t.Fatalf("upgrade start with unchanged key: %v", err)
	}
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key=$1", arbiterKeyPin).Scan(&pin); err != nil || pin != want {
		t.Fatalf("upgrade key pin %q, want the unchanged account-key HMAC %q (err=%v)", pin, want, err)
	}
	a.Close()

	// A different account key with the same fingerprint must not replace the pin or enable escrow.
	other, err := parseEscrowKey(altEscrowKey(t, testBIP84Key, fakeArbiterFP, 1))
	if err != nil {
		t.Fatal(err)
	}
	core.arbiterDesc = "wpkh([" + fakeArbiterFP + "/84h/1h/0h]" + other.XPub + "/1/*)#substituted"
	a, _, err = startApp(t, dsn)
	if err != nil || a.escrow.Load() != nil || !strings.Contains(logs.String(), keyMismatchLog) {
		t.Fatalf("same-fingerprint key substitution was not refused: err=%v logs=%s", err, logs.String())
	}
	var healthClass string
	if err = a.db.QueryRow("SELECT disabled_class FROM bitcoin_escrow_health WHERE singleton").Scan(&healthClass); err != nil || healthClass != "arbiter_key_mismatch" {
		t.Fatalf("same-fingerprint key substitution health class %q (err=%v)", healthClass, err)
	}
	e := &testEnv{t: t, A: a, DB: a.db}
	_, adminSession := e.user("escrow_key_pin_admin", "admin")
	if page := e.do("GET", "/admin", adminSession, nil).Body.String(); !strings.Contains(page, "arbiter_key_mismatch") {
		t.Fatal("admin Payment providers did not show the safe arbiter-key mismatch health class")
	}
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key=$1", arbiterKeyPin).Scan(&pin); err != nil || pin != want {
		t.Fatalf("substitution changed key pin to %q (err=%v)", pin, err)
	}
	if strings.Contains(logs.String(), fakeArbiterFP) || strings.Contains(logs.String(), key.XPub) || strings.Contains(logs.String(), other.XPub) || strings.Contains(logs.String(), want) {
		t.Fatalf("operator logs exposed fingerprint or key material: %s", logs.String())
	}
	a.Close()

	// The pin covers the public key as well as the chain code: altEscrowKey changes only the chain code (raw[13]),
	// so a pin that dropped the public key would still pass the leg above. Here only the public key's parity byte
	// (raw[45], 02 <-> 03: the same x, the negated y) differs.
	samePub, err := parseEscrowKey(altEscrowPubKey(t, testBIP84Key, fakeArbiterFP))
	if err != nil {
		t.Fatal(err)
	}
	if samePub.ChainCode != key.ChainCode || samePub.PubKey == key.PubKey {
		t.Fatalf("pubkey-only substitution fixture is wrong: chain code equal=%v pubkey equal=%v", samePub.ChainCode == key.ChainCode, samePub.PubKey == key.PubKey)
	}
	core.arbiterDesc = "wpkh([" + fakeArbiterFP + "/84h/1h/0h]" + samePub.XPub + "/1/*)#pubkey-substituted"
	a, _, err = startApp(t, dsn)
	if err != nil || a.escrow.Load() != nil || strings.Count(logs.String(), keyMismatchLog) != 2 {
		t.Fatalf("same-fingerprint, same-chain-code public-key substitution was not refused: err=%v logs=%s", err, logs.String())
	}
	if err = a.db.QueryRow("SELECT disabled_class FROM bitcoin_escrow_health WHERE singleton").Scan(&healthClass); err != nil || healthClass != "arbiter_key_mismatch" {
		t.Fatalf("public-key substitution health class %q (err=%v)", healthClass, err)
	}
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key=$1", arbiterKeyPin).Scan(&pin); err != nil || pin != want {
		t.Fatalf("public-key substitution changed key pin to %q (err=%v)", pin, err)
	}
	if strings.Contains(logs.String(), samePub.XPub) {
		t.Fatalf("operator logs exposed the substituted key: %s", logs.String())
	}
	a.Close()
}

// altEscrowPubKey is key with its public key's parity byte flipped (02 <-> 03) and the same chain code, under origin fp.
func altEscrowPubKey(t *testing.T, key, fp string) string {
	t.Helper()
	k, err := parseEscrowKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base58CheckDecode(k.XPub)
	if err != nil {
		t.Fatal(err)
	}
	raw[45] ^= 1
	return "[" + fp + k.PathText + "]" + base58CheckEncode(raw)
}
