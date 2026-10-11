package market

import (
	"database/sql"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func xmrDestroyedFormNonce(t *testing.T, page, walletID string) string {
	t.Helper()
	forms := regexp.MustCompile(`(?s)<form class="stack" method="post" action="/admin/escrow-xmr-destroyed">(.*?)</form>`).FindAllStringSubmatch(page, -1)
	walletField := `name="wallet_id" value="` + walletID + `"`
	nonceField := regexp.MustCompile(`name="form_nonce" value="([0-9a-f]{32})"`)
	for _, form := range forms {
		if !strings.Contains(form[1], walletField) {
			continue
		}
		match := nonceField.FindStringSubmatch(form[1])
		if match == nil {
			t.Fatalf("destroyed-wallet form for %s has no valid nonce", walletID)
		}
		return match[1]
	}
	t.Fatalf("admin page has no destroyed-wallet form for %s", walletID)
	return ""
}

func TestXMREscrowDestroyedConfirmedNonceReplayAfterRecoveryGate(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	const name = "escrow-202610-01"
	x.rotate()
	x.fake.edit(func(f *fakeXMREscrow) { delete(f.files, name) })
	x.mustPoll()
	if x.walletState(name) != "missing" {
		x.failWallets()
	}
	x.days(name, 91)
	walletID := strconv.Itoa(agInt(x.testEnv, "SELECT id FROM xmr_escrow_wallets WHERE name=$1", name))
	otherWalletID := strconv.Itoa(agInt(x.testEnv, "SELECT id FROM xmr_escrow_wallets WHERE name='escrow-202611-01'"))
	adminID, admin := x.user("xmr_destroy_nonce_"+randomToken()[:8], "admin")
	key, _ := agEnableTOTP(x.testEnv, adminID)
	path := "/admin/escrow-xmr-destroyed"

	callsBeforeGET := len(x.fake.callLog())
	page := html.UnescapeString(x.page("/admin", admin))
	nonce := xmrDestroyedFormNonce(t, page, walletID)
	if len(x.fake.callLog()) != callsBeforeGET || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path) != 0 {
		t.Fatal("admin GET wrote a receipt or made a wallet RPC")
	}
	form := url.Values{
		"wallet_id": {walletID}, "gone": {"confirmed"}, "password": {testPassword},
		"code": {agTOTPNow(key)}, formNonceField: {nonce},
	}
	wrongPassword := withField(form, "password", "not the current password")
	x.check(x.do("POST", path, admin, wrongPassword), 401)
	missingTOTP := withField(form, "code", "")
	x.check(x.do("POST", path, admin, missingTOTP), 401)
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path); n != 0 {
		t.Fatalf("confirmation refusals claimed %d receipts", n)
	}

	callsBeforeConfirm := len(x.fake.callLog())
	lost := loseNextActionCommit(t, x.testEnv)
	first := x.do("POST", path, admin, form)
	if lost.Load() || first.Code != 500 || first.Header().Get("Retry-After") != "" ||
		!strings.Contains(first.Body.String(), "outcome unknown") || !strings.Contains(first.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost confirmation acknowledgement: injected=%t status=%d retry=%q body=%q", !lost.Load(), first.Code, first.Header().Get("Retry-After"), first.Body.String())
	}
	if len(x.fake.callLog()) != callsBeforeConfirm {
		t.Fatal("admin confirmation made a Monero wallet RPC")
	}
	var state string
	var sealed sql.NullString
	var destroyedAt, updated time.Time
	readWallet := func() {
		t.Helper()
		if err := x.DB.QueryRow("SELECT state,seed_sealed,destroyed_at,updated FROM xmr_escrow_wallets WHERE id=$1", walletID).
			Scan(&state, &sealed, &destroyedAt, &updated); err != nil {
			t.Fatal(err)
		}
	}
	readWallet()
	if state != "destroyed" || sealed.Valid {
		t.Fatalf("lost acknowledgement did not commit destruction: state=%s seed-sealed=%t", state, sealed.Valid)
	}
	firstDestroyedAt, firstUpdated := destroyedAt, updated
	var lastStep int
	if err := x.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", adminID).Scan(&lastStep); err != nil {
		t.Fatal(err)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path); n != 1 {
		t.Fatalf("committed confirmation receipt count=%d, want one", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", adminID, "Confirmed the destruction of Monero escrow wallet "+name+"% (confirmed with password and authenticator code)"); n != 1 {
		t.Fatalf("confirmation audit count=%d, want one", n)
	}
	if first.Code != 500 || !strings.Contains(first.Body.String(), "Escrow wallet destruction outcome unknown") ||
		!strings.Contains(first.Body.String(), "Check the wallet record and sealed-seed status") ||
		!strings.Contains(first.Body.String(), "verify the wallet files and backups") ||
		!strings.Contains(first.Body.String(), "resubmit it unchanged") {
		t.Fatalf("destructive lost-ack response lacks the explicit recovery instruction: %q", first.Body.String())
	}
	if _, exists := x.fake.file(name); exists {
		t.Fatal("confirmation recreated the missing wallet files")
	}
	var receipt string
	if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE action_path=$1 AND nonce=$2", path, nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, testPassword) || strings.Contains(receipt, form.Get("code")) {
		t.Fatal("confirmed receipt retained a password or authenticator code")
	}
	otherWallet := withField(form, "wallet_id", otherWalletID)
	x.check(x.do("POST", path, admin, otherWallet), 409)
	otherSession := randomToken()
	agExec(x.testEnv, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), adminID)
	x.check(x.do("POST", path, otherSession, form), 409)
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path); n != 1 {
		t.Fatalf("mismatched replay changed receipt count to %d, want one", n)
	}
	expiredSession := randomToken()
	agExec(x.testEnv, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()-interval '1 second')", digest(expiredSession), adminID)
	x.check(x.do("POST", path, expiredSession, form), 401)

	// If an order's current buyer becomes this administrator, the old receipt cannot bypass the party prohibition.
	var originalBuyer string
	if err := x.DB.QueryRow("SELECT buyer_id FROM orders WHERE id=$1", order).Scan(&originalBuyer); err != nil {
		t.Fatal(err)
	}
	agExec(x.testEnv, "UPDATE orders SET buyer_id=$2 WHERE id=$1", order, adminID)
	x.check(x.do("POST", path, admin, form), 403)
	readWallet()
	if state != "destroyed" || sealed.Valid || !destroyedAt.Equal(firstDestroyedAt) || !updated.Equal(firstUpdated) ||
		agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path) != 1 {
		t.Fatal("new order-party status changed the wallet or replay receipt")
	}
	agExec(x.testEnv, "UPDATE orders SET buyer_id=$2 WHERE id=$1", order, originalBuyer)

	// Applying the restore recovery-gate SQL leaves already-destroyed rows intact. An exact retry must retrieve its
	// committed response without touching the wallet row, audit, TOTP step, or wallet-rpc.
	x.restoreBackup()
	agExec(x.testEnv, "UPDATE users SET role='moderator' WHERE id=$1", adminID)
	x.check(x.do("POST", path, admin, form), 403)
	agExec(x.testEnv, "UPDATE users SET role='admin' WHERE id=$1", adminID)
	replay := x.do("POST", path, admin, form)
	x.check(replay, 303)
	if replay.Header().Get("Location") != "/admin#xmr-escrow-recovery" || len(x.fake.callLog()) != callsBeforeConfirm {
		t.Fatalf("saved replay changed its recorded redirect or made wallet RPC: location=%q calls=%d/%d", replay.Header().Get("Location"), len(x.fake.callLog()), callsBeforeConfirm)
	}
	if _, exists := x.fake.file(name); exists {
		t.Fatal("exact replay recreated the destroyed wallet files")
	}
	readWallet()
	if state != "destroyed" || sealed.Valid || !destroyedAt.Equal(firstDestroyedAt) || !updated.Equal(firstUpdated) {
		t.Fatalf("replay changed the destroyed wallet: state=%s sealed=%t destroyedAt=%s/%s updated=%s/%s", state, sealed.Valid, destroyedAt, firstDestroyedAt, updated, firstUpdated)
	}
	if got := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", adminID); got != lastStep {
		t.Fatalf("exact replay changed the spent TOTP step from %d to %d", lastStep, got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path); n != 1 {
		t.Fatalf("replay left %d receipts, want one", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", adminID, "Confirmed the destruction of Monero escrow wallet "+name+"% (confirmed with password and authenticator code)"); n != 1 {
		t.Fatalf("replay wrote %d audits, want one", n)
	}

	// A new submission cannot reuse the already-spent authenticator step, and a refusal persists no receipt.
	fresh := withField(form, formNonceField, formNonceFor(path))
	x.check(x.do("POST", path, admin, fresh), 401)
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path); n != 1 {
		t.Fatalf("fresh spent-code refusal left %d receipts, want one", n)
	}
}
