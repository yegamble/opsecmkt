package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func confirmedPayoutForm(id, nonce string) url.Values {
	return url.Values{
		"payout_id": {id}, "op": {"hold"},
		"note":     {"Check the wallet before paying this payout by hand"},
		"password": {testPassword}, formNonceField: {nonce},
	}
}

func payoutNonceCounts(t *testing.T, p *payEnv, id, order string) (receipt, audits, events int) {
	t.Helper()
	for _, q := range []struct {
		query string
		arg   any
		out   *int
	}{
		{"SELECT count(*) FROM form_submissions WHERE action_path='/admin/payout'", nil, &receipt},
		{"SELECT count(*) FROM audit_events WHERE action LIKE $1", "Held pending payout " + id + "%", &audits},
		{"SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%held by an administrator%'", order, &events},
	} {
		var err error
		if q.arg == nil {
			err = p.DB.QueryRow(q.query).Scan(q.out)
		} else {
			err = p.DB.QueryRow(q.query, q.arg).Scan(q.out)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestConfirmedPayoutEveryRenderedOperationHasDistinctNonce(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-new-destination")
	p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-old", "", false, "")
	p.insertPayout(stateCompleted, "release", p.vendor.ID, "held", "fake-testnet-old", heldReason, false, "")
	p.insertPayout(stateCompleted, "release", p.vendor.ID, "failed", "fake-testnet-old", "Wallet refused before broadcast", false, "")
	page := p.do("GET", "/admin", p.adminSess, nil)
	p.check(page, 200)
	forms := regexp.MustCompile(`(?s)<form class="stack" method="post" action="/admin/payout">(.*?)</form>`).FindAllStringSubmatch(page.Body.String(), -1)
	fields := regexp.MustCompile(`name="form_nonce" value="([0-9a-f]{32})"`)
	ops := regexp.MustCompile(`name="op" value="([a-z]+)"`)
	seenNonce, seenOp := map[string]bool{}, map[string]bool{}
	for _, form := range forms {
		nonce, op := fields.FindStringSubmatch(form[1]), ops.FindStringSubmatch(form[1])
		if nonce == nil || op == nil || seenNonce[nonce[1]] {
			t.Fatalf("missing or reused payout form nonce: op=%v nonce=%v", op, nonce)
		}
		seenNonce[nonce[1]], seenOp[op[1]] = true, true
	}
	for _, op := range []string{"hold", "release", "requeue", "repoint", "sent"} {
		if !seenOp[op] {
			t.Errorf("admin payout %s form was not rendered with a nonce", op)
		}
	}
	if got := submissionCount(t, p.testEnv); got != 0 {
		t.Fatalf("admin GET wrote %d receipts", got)
	}
}

func TestConfirmedPayoutNonceRenderReplayAndRefusals(t *testing.T) {
	p := newPayEnv(t)
	order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
	id := p.payoutID(order)
	admin := p.do("GET", "/admin", p.adminSess, nil)
	p.check(admin, 200)
	nonce := renderedFormNonce(t, admin.Body.String(), "/admin/payout")
	if got := submissionCount(t, p.testEnv); got != 0 {
		t.Fatalf("admin GET wrote %d receipts", got)
	}
	f := confirmedPayoutForm(id, nonce)
	first := p.do("POST", "/admin/payout", p.adminSess, f)
	p.check(first, 303)
	replay := p.do("POST", "/admin/payout", p.adminSess, f)
	p.check(replay, 303)
	if first.Header().Get("Location") != replay.Header().Get("Location") ||
		(first.Header().Get("Location") == "") {
		t.Fatalf("redirect differs: %q vs %q", first.Header().Get("Location"), replay.Header().Get("Location"))
	}
	if receipt, audits, events := payoutNonceCounts(t, p, id, order); receipt != 1 || audits != 1 || events != 1 {
		t.Fatalf("replay wrote receipt=%d audit=%d event=%d", receipt, audits, events)
	}
	var saved string
	if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, f.Get("note"), "fake-testnet-vendor", p.adminSess} {
		if strings.Contains(saved, secret) {
			t.Fatalf("receipt retained a raw submitted secret or destination")
		}
	}
	changed := url.Values{}
	for k, v := range f {
		changed[k] = v
	}
	changed.Set("note", "A different instruction must not use the saved result")
	p.check(p.do("POST", "/admin/payout", p.adminSess, changed), 409)
	wrong := url.Values{}
	for k, v := range f {
		wrong[k] = v
	}
	wrong.Set("password", "wrong password")
	p.check(p.do("POST", "/admin/payout", p.adminSess, wrong), 401)
	_, other := p.user("other_nonce_admin", "admin")
	p.check(p.do("POST", "/admin/payout", other, f), 409)
	p.check(p.do("POST", "/admin/payout", p.adminSess, url.Values{
		"payout_id": {id}, "op": {"hold"}, "note": {f.Get("note")}, "password": {testPassword}, formNonceField: {""},
	}), 400)
	// The fixture helper issues a nonce for ordinary direct POSTs. A raw request pins the actual missing-nonce rule.
	raw := confirmedPayoutForm(id, "")
	raw.Del(formNonceField)
	raw.Set("csrf", p.A.csrf(p.adminSess))
	r := httptest.NewRequest(http.MethodPost, "/admin/payout", strings.NewReader(raw.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: p.adminSess})
	w := httptest.NewRecorder()
	p.A.ServeHTTP(w, r)
	p.check(w, 400)
	raw.Set(formNonceField, nonce)
	raw.Set("csrf", "incorrect")
	r = httptest.NewRequest(http.MethodPost, "/admin/payout", strings.NewReader(raw.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: p.adminSess})
	w = httptest.NewRecorder()
	p.A.ServeHTTP(w, r)
	p.check(w, 403) // a saved response cannot bypass the CSRF gate
	if receipt, audits, events := payoutNonceCounts(t, p, id, order); receipt != 1 || audits != 1 || events != 1 {
		t.Fatalf("refusals changed receipt=%d audit=%d event=%d", receipt, audits, events)
	}
}

func TestConfirmedPayoutNonceRejectsUnsupportedConfirmationBeforeWork(t *testing.T) {
	e := newTestApp(t)
	id, session := e.user("unsupported_nonce_admin", "admin")
	_, recovery := agEnableTOTP(e, id)
	actor, err := e.A.sessionUser(t.Context(), session)
	if err != nil || actor == nil {
		t.Fatalf("admin session: %v", err)
	}
	for _, tc := range []struct {
		name string
		opts func(*int) *confirmation
	}{
		{"nil", func(*int) *confirmation { return nil }},
		{"recovery", func(*int) *confirmation { return &confirmation{Recovery: true} }},
		{"before hook", func(calls *int) *confirmation {
			return &confirmation{Before: func(*actionCtx) error { *calls++; return nil }}
		}},
		{"after password hook", func(calls *int) *confirmation {
			return &confirmation{AfterPassword: func() error { *calls++; return nil }}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f := url.Values{"password": {testPassword}, "code": {recovery}, formNonceField: {randomToken()[:32]}}
			c := &actionCtx{A: e.A, R: httptest.NewRequest(http.MethodPost, "/admin/payout", nil), User: actor, Token: session, Form: f, ConfirmedFormNonce: true}
			_, err := confirmedTxWith(c, tc.opts(&calls), func(*actionCtx) (actionResult, error) { calls++; return actionResult{}, nil })
			if err == nil || calls != 0 {
				t.Fatalf("unsupported confirmation ran work: err=%v calls=%d", err, calls)
			}
			if n := submissionCount(t, e); n != 0 {
				t.Fatalf("unsupported confirmation wrote %d receipts", n)
			}
			if n := agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NOT NULL", id); n != 0 {
				t.Fatalf("unsupported confirmation spent %d recovery codes", n)
			}
		})
	}
}

func TestConfirmedPayoutNonceRollsBackAndSerializes(t *testing.T) {
	p := newPayEnv(t)
	order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
	id := p.payoutID(order)
	f := confirmedPayoutForm(id, randomToken()[:32])
	f.Set("op", "release") // invalid state, after the nonce has been claimed
	p.check(p.do("POST", "/admin/payout", p.adminSess, f), 409)
	if got := submissionCount(t, p.testEnv); got != 0 {
		t.Fatalf("refused payout kept %d receipts", got)
	}
	f.Set("op", "hold")
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- p.do("POST", "/admin/payout", p.adminSess, f) }()
	}
	close(start)
	wg.Wait()
	close(results)
	for w := range results {
		p.check(w, 303)
	}
	if receipt, audits, events := payoutNonceCounts(t, p, id, order); receipt != 1 || audits != 1 || events != 1 {
		t.Fatalf("concurrent payout wrote receipt=%d audit=%d event=%d", receipt, audits, events)
	}
}

func TestConfirmedPayoutNonceRechecksFactorAndActor(t *testing.T) {
	t.Run("TOTP is not spent on exact replay", func(t *testing.T) {
		p := newPayEnv(t)
		order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
		id := p.payoutID(order)
		admin, err := p.A.sessionUser(t.Context(), p.adminSess)
		if err != nil || admin == nil {
			t.Fatalf("admin session: %v", err)
		}
		key, _ := agEnableTOTP(p.testEnv, admin.ID)
		f := confirmedPayoutForm(id, randomToken()[:32])
		f.Set("code", agTOTPNow(key))
		p.check(p.do("POST", "/admin/payout", p.adminSess, f), 303)
		var saved string
		if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", f.Get(formNonceField)).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(saved, `"`+f.Get("code")+`"`) {
			t.Fatal("receipt retained the raw authenticator code")
		}
		var step int64
		if err := p.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", admin.ID).Scan(&step); err != nil {
			t.Fatal(err)
		}
		p.check(p.do("POST", "/admin/payout", p.adminSess, f), 303)
		var after int64
		if err := p.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", admin.ID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != step {
			t.Fatalf("replay spent TOTP again: %d to %d", step, after)
		}
		f.Set(formNonceField, randomToken()[:32])
		p.check(p.do("POST", "/admin/payout", p.adminSess, f), 401) // a new change cannot reuse that code
	})
	for _, tc := range []struct{ name, update, want string }{
		{"demoted", "UPDATE users SET role='buyer' WHERE id=$1", "403"},
		{"suspended", "UPDATE users SET suspended_at=now() WHERE id=$1", "403"},
		{"password changed", "UPDATE users SET password_hash='__REPLACE__' WHERE id=$1", "401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPayEnv(t)
			order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
			id := p.payoutID(order)
			admin, err := p.A.sessionUser(t.Context(), p.adminSess)
			if err != nil || admin == nil {
				t.Fatalf("admin session: %v", err)
			}
			f := confirmedPayoutForm(id, randomToken()[:32])
			p.check(p.do("POST", "/admin/payout", p.adminSess, f), 303)
			update := tc.update
			if tc.name == "password changed" {
				hash, err := hashPassword("a-different-long-password")
				if err != nil {
					t.Fatal(err)
				}
				update = strings.Replace(update, "__REPLACE__", hash, 1)
			}
			w := besideAccountChange(t, p.testEnv, admin.ID, []string{update}, func() *httptest.ResponseRecorder {
				return p.do("POST", "/admin/payout", p.adminSess, f)
			})
			want := 403
			if tc.want == "401" {
				want = 401
			}
			p.check(w, want)
			if receipt, audits, events := payoutNonceCounts(t, p, id, order); receipt != 1 || audits != 1 || events != 1 {
				t.Fatalf("stale actor changed receipt=%d audit=%d event=%d", receipt, audits, events)
			}
		})
	}
}

func TestConfirmedPayoutNonceRevokedOrExpiredSessionCannotReplay(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"revoked", "DELETE FROM sessions WHERE user_id=$1"},
		{"expired", "UPDATE sessions SET expires=clock_timestamp()-interval '1 second' WHERE user_id=$1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPayEnv(t)
			order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
			id := p.payoutID(order)
			admin, err := p.A.sessionUser(t.Context(), p.adminSess)
			if err != nil || admin == nil {
				t.Fatalf("admin session: %v", err)
			}
			f := confirmedPayoutForm(id, randomToken()[:32])
			p.check(p.do("POST", "/admin/payout", p.adminSess, f), 303)
			w := besideAccountChange(t, p.testEnv, admin.ID, []string{tc.change}, func() *httptest.ResponseRecorder {
				return p.do("POST", "/admin/payout", p.adminSess, f)
			})
			p.check(w, 401)
			if receipt, audits, events := payoutNonceCounts(t, p, id, order); receipt != 1 || audits != 1 || events != 1 {
				t.Fatalf("stale session changed receipt=%d audit=%d event=%d", receipt, audits, events)
			}
		})
	}
}

func TestConfirmedPayoutNonceLostCommitAcknowledgementCanReplay(t *testing.T) {
	p := newPayEnv(t)
	order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
	id := p.payoutID(order)
	f := confirmedPayoutForm(id, randomToken()[:32])
	lost := loseNextActionCommit(t, p.testEnv)
	w := p.do("POST", "/admin/payout", p.adminSess, f)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement: status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	p.check(p.do("POST", "/admin/payout", p.adminSess, f), 303)
	if receipt, audits, events := payoutNonceCounts(t, p, id, order); receipt != 1 || audits != 1 || events != 1 {
		t.Fatalf("lost-ack replay wrote receipt=%d audit=%d event=%d", receipt, audits, events)
	}
	var state string
	if err := p.DB.QueryRow("SELECT state FROM payouts WHERE id=$1", id).Scan(&state); err != nil || state != "held" {
		t.Fatalf("held payout after lost acknowledgement: state=%s err=%v", state, err)
	}
}
