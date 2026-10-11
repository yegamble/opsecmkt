package market

import (
	"strings"
	"testing"
)

func adminListingReceiptCount(t *testing.T, e *testEnv) int {
	t.Helper()
	return agInt(e, "SELECT count(*) FROM form_submissions WHERE action_path='/admin/listing'")
}

func TestAdminListingNonceBindsActorSessionAndCredentials(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("listing_nonce_admin", "admin")
	vendorID, _ := e.user("listing_nonce_vendor", "vendor")
	product := e.product(vendorID, "physical")
	form := takedownForm(t, e.body("GET", "/listing-edit?id="+product, admin, nil, 200), "takedown", product)
	form.Set("reason", takedownReason)
	if got := adminListingReceiptCount(t, e); got != 0 {
		t.Fatalf("listing edit GET wrote %d receipts", got)
	}
	wrong := withField(form, "password", "incorrect-password")
	e.check(e.do("POST", "/admin/listing", admin, wrong), 401)
	if got := adminListingReceiptCount(t, e); got != 0 {
		t.Fatalf("wrong password persisted %d receipts", got)
	}

	first := e.do("POST", "/admin/listing", admin, form)
	e.check(first, 303)
	replay := e.do("POST", "/admin/listing", admin, form)
	e.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("replay location %q, original %q", replay.Header().Get("Location"), first.Header().Get("Location"))
	}
	changedReason := withField(form, "reason", takedownReason+" Revised")
	e.check(e.do("POST", "/admin/listing", admin, changedReason), 409)
	otherSession := randomToken()
	if _, err := e.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), adminID); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/admin/listing", otherSession, form), 409)
	hash, err := hashPassword("a-different-long-password")
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE users SET password_hash=$2 WHERE id=$1", adminID, hash)
	e.check(e.do("POST", "/admin/listing", admin, form), 401)
	agExec(e, "UPDATE users SET role='moderator' WHERE id=$1", adminID)
	e.check(e.do("POST", "/admin/listing", admin, form), 403)
	if got := adminListingReceiptCount(t, e); got != 1 {
		t.Fatalf("mismatched requests changed receipt count to %d", got)
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", vendorID); got != 1 {
		t.Fatalf("replay/refusals wrote %d vendor notifications", got)
	}
}

func TestAdminListingFreshStateRefusalLeavesNonceReusable(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("listing_retry_admin", "admin")
	vendorID, _ := e.user("listing_retry_vendor", "vendor")
	product := e.product(vendorID, "physical")
	edit := "/listing-edit?id=" + product
	take := takedownForm(t, e.body("GET", edit, admin, nil, 200), "takedown", product)
	take.Set("reason", takedownReason)
	e.check(e.do("POST", "/admin/listing", admin, take), 303)

	restoreOne := takedownForm(t, e.body("GET", edit, admin, nil, 200), "restore", product)
	restoreTwo := takedownForm(t, e.body("GET", edit, admin, nil, 200), "restore", product)
	if restoreOne.Get(formNonceField) == restoreTwo.Get(formNonceField) {
		t.Fatal("separately rendered restore forms shared a receipt nonce")
	}
	if got := adminListingReceiptCount(t, e); got != 1 {
		t.Fatalf("restore GETs changed receipt count to %d", got)
	}
	e.check(e.do("POST", "/admin/listing", admin, restoreOne), 303)
	stale := e.do("POST", "/admin/listing", admin, restoreTwo)
	if stale.Code != 409 || !strings.Contains(stale.Body.String(), "not taken down") {
		t.Fatalf("stale restore: %d %q", stale.Code, stale.Body.String())
	}
	if got := adminListingReceiptCount(t, e); got != 2 {
		t.Fatalf("refused stale restore persisted a receipt: got %d", got)
	}

	newTake := takedownForm(t, e.body("GET", edit, admin, nil, 200), "takedown", product)
	newTake.Set("reason", takedownReason)
	e.check(e.do("POST", "/admin/listing", admin, newTake), 303)
	e.check(e.do("POST", "/admin/listing", admin, restoreTwo), 303)
	if archived, reason := takenDown(e, product); archived || reason != "" {
		t.Fatalf("corrected retry did not restore listing: archived=%v reason=%q", archived, reason)
	}
	if got := adminListingReceiptCount(t, e); got != 4 {
		t.Fatalf("successful transitions persisted %d receipts, want 4", got)
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", vendorID); got != 4 {
		t.Fatalf("four transitions wrote %d notices", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id IN ($1,$2)", adminID, vendorID); got != 8 {
		t.Fatalf("four transitions wrote %d audit rows, want 8", got)
	}
}

func TestAdminListingNonceTOTPReplayAndFreshCode(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("listing_totp_admin", "admin")
	vendorID, _ := e.user("listing_totp_vendor", "vendor")
	product := e.product(vendorID, "physical")
	key, _ := agEnableTOTP(e, adminID)
	edit := "/listing-edit?id=" + product
	take := takedownForm(t, e.body("GET", edit, admin, nil, 200), "takedown", product)
	take.Set("reason", takedownReason)
	e.check(e.do("POST", "/admin/listing", admin, take), 401)
	if got := adminListingReceiptCount(t, e); got != 0 {
		t.Fatalf("missing TOTP code persisted %d receipts", got)
	}
	spentCode := agTOTPNow(key)
	take.Set("code", spentCode)
	e.check(e.do("POST", "/admin/listing", admin, take), 303)
	step := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", adminID)
	e.check(e.do("POST", "/admin/listing", admin, take), 303)
	if got := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", adminID); got != step {
		t.Fatalf("exact replay consumed TOTP twice: %d then %d", step, got)
	}
	newRestore := takedownForm(t, e.body("GET", edit, admin, nil, 200), "restore", product)
	newRestore.Set("code", spentCode)
	e.check(e.do("POST", "/admin/listing", admin, newRestore), 401)
	if got := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", adminID); got != step {
		t.Fatalf("fresh-nonce request accepted spent TOTP: %d then %d", step, got)
	}
	if got := adminListingReceiptCount(t, e); got != 1 {
		t.Fatalf("TOTP replay/refusal left %d receipts", got)
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", vendorID); got != 1 {
		t.Fatalf("TOTP replay/refusal wrote %d vendor notices", got)
	}
}

func TestAdminListingNonceLostCommitAcknowledgementReplays(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("listing_lost_admin", "admin")
	vendorID, _ := e.user("listing_lost_vendor", "vendor")
	product := e.product(vendorID, "physical")
	form := takedownForm(t, e.body("GET", "/listing-edit?id="+product, admin, nil, 200), "takedown", product)
	form.Set("reason", takedownReason)
	lost := loseNextActionCommit(t, e)
	w := e.do("POST", "/admin/listing", admin, form)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "Listing change outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement: injected=%t status=%d retry=%q body=%q", !lost.Load(), w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	e.check(e.do("POST", "/admin/listing", admin, form), 303)
	if got := adminListingReceiptCount(t, e); got != 1 {
		t.Fatalf("lost-ack replay left %d receipts", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id IN ($1,$2)", adminID, vendorID); got != 2 {
		t.Fatalf("lost-ack replay wrote %d audit rows", got)
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", vendorID); got != 1 {
		t.Fatalf("lost-ack replay wrote %d vendor notices", got)
	}
}
