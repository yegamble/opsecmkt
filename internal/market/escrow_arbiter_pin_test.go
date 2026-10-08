package market

import (
	"strings"
	"testing"
)

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
}
