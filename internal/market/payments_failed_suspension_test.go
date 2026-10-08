package market

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func suspendedRequeueForm(id, nonce string, extra url.Values) url.Values {
	f := url.Values{"payout_id": {id}, "op": {"requeue"}, "password": {testPassword}, formNonceField: {nonce}}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func TestFailedPayoutSuspensionRequiresCheckedAddressAfterRestore(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order, id := p.failPayout("tx-failed-suspend", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	original := p.str("SELECT error FROM payouts WHERE id=$1", id)
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	marked := p.str("SELECT error FROM payouts WHERE id=$1", id)
	if !strings.HasPrefix(marked, original) || !strings.Contains(marked, suspendedRequeue) ||
		p.str("SELECT state||'|'||send_ambiguous::text FROM payouts WHERE id=$1", id) != "failed|false" {
		t.Fatalf("suspension lost failed outcome: %q", marked)
	}
	ownerNotice := p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator suspended%' ORDER BY created DESC LIMIT 1", p.vendor.ID)
	if !strings.Contains(ownerNotice, "failed or sending payout(s)") || strings.Contains(ownerNotice, "are held") {
		t.Fatalf("suspension notice misstates failed payout: %q", ownerNotice)
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("restore", p.vendor.Handle)), 303)
	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	mustContain(t, admin, "payout address check required before requeue")
	section := resolveSection(t, admin, id)
	mustContain(t, section, `name="address_checked" value="confirmed" required`, "fake-testnet-vendor")
	mustNotContain(t, section, `name="not_broadcast" value="confirmed" required`)
	nonce := renderedFormNonce(t, section, "/admin/payout")
	beforeAudit := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout ' || $1 || ' %'", id)
	for _, check := range []string{"", "yes"} {
		f := suspendedRequeueForm(id, nonce, nil)
		if check != "" {
			f.Set("address_checked", check)
		}
		w := p.do("POST", "/admin/payout", p.adminSess, f)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "recorded payout address") {
			t.Fatalf("unchecked failed payout requeue: %d %s", w.Code, w.Body.String())
		}
	}
	if got := p.str("SELECT state||'|'||error FROM payouts WHERE id=$1", id); got != "failed|"+marked {
		t.Fatalf("refusal changed payout: %s", got)
	}
	if got := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout ' || $1 || ' %'", id); got != beforeAudit {
		t.Fatalf("refusal audited requeue: %d", got)
	}
	f := suspendedRequeueForm(id, nonce, url.Values{"address_checked": {"confirmed"}})
	first := p.do("POST", "/admin/payout", p.adminSess, f)
	p.check(first, 303)
	second := p.do("POST", "/admin/payout", p.adminSess, f)
	p.check(second, 303)
	if first.Header().Get("Location") != second.Header().Get("Location") {
		t.Fatal("saved requeue receipt did not return its original redirect")
	}
	if got := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout ' || $1 || ' %administrator checked the payout address fake-testnet-vendor%'", id); got != 1 {
		t.Fatalf("checked requeue audited %d times", got)
	}
	if got := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%checked the recorded payout address%'", order); got != 1 {
		t.Fatalf("checked requeue recorded %d order events", got)
	}
	if state, _, _, _ := p.payout(order); state != "pending" {
		t.Fatalf("checked requeue state %s", state)
	}
	p.poll()
	if state, _, _, _ := p.payout(order); state != "sent" || p.sendsTo("fake-testnet-vendor") != 1 {
		t.Fatalf("replayed requeue sent %d times, state %s", p.sendsTo("fake-testnet-vendor"), state)
	}
	p.check(p.do("POST", "/admin/payout", p.adminSess, f), 303)
	p.poll()
	if p.sendsTo("fake-testnet-vendor") != 1 {
		t.Fatalf("saved receipt queued another wallet send: %d", p.sendsTo("fake-testnet-vendor"))
	}
}

func TestSuspendedFailedRequeueKeepsWalletAndAddressChecks(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order, id := p.failPayout("tx-ambiguous-suspend", errors.New("wallet connection lost after request"))
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	// A restore prefixes the original failure but must keep both the broadcast uncertainty and suspension mark.
	if _, err := p.DB.Exec("UPDATE payouts SET error=$2 || ' Last error: ' || error WHERE id=$1", id, restoredHold); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("restore", p.vendor.Handle)), 303)
	section := resolveSection(t, html.UnescapeString(p.page("/admin", p.adminSess)), id)
	mustContain(t, section, `name="not_broadcast" value="confirmed" required`, `name="address_checked" value="confirmed" required`)
	nonce := renderedFormNonce(t, section, "/admin/payout")
	for _, extra := range []url.Values{
		{"address_checked": {"confirmed"}},
		{"not_broadcast": {"confirmed"}},
	} {
		w := p.do("POST", "/admin/payout", p.adminSess, suspendedRequeueForm(id, nonce, extra))
		if w.Code == 303 {
			t.Fatalf("one-confirmation requeue accepted: %v", extra)
		}
	}
	if state, _, _, _ := p.payout(order); state != "failed" || p.str("SELECT send_ambiguous::text FROM payouts WHERE id=$1", id) != "true" {
		t.Fatal("one-confirmation refusal changed uncertain failed outcome")
	}
	f := suspendedRequeueForm(id, nonce, url.Values{"not_broadcast": {"confirmed"}, "address_checked": {"confirmed"}})
	p.check(p.do("POST", "/admin/payout", p.adminSess, f), 303)
	if got := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout ' || $1 || ' %administrator confirmed the wallet shows no broadcast transaction%administrator checked the payout address%'", id); got != 1 {
		t.Fatalf("both confirmations not audited: %d", got)
	}
}

func TestSuspendedFailedPayoutWithoutAddressCannotRequeue(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	_, id := p.failPayout("tx-no-address-suspend", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	if _, err := p.DB.Exec("UPDATE payouts SET address='' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	section := resolveSection(t, html.UnescapeString(p.page("/admin", p.adminSess)), id)
	mustContain(t, section, "no recorded address to check")
	mustNotContain(t, section, `name="op" value="requeue"`)
	f := suspendedRequeueForm(id, randomToken()[:32], url.Values{"address_checked": {"confirmed"}})
	w := p.do("POST", "/admin/payout", p.adminSess, f)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "recorded payout address is empty") {
		t.Fatalf("empty-address requeue: %d %s", w.Code, w.Body.String())
	}
	if got := p.str("SELECT state||'|'||address FROM payouts WHERE id=$1", id); got != "failed|" {
		t.Fatalf("empty-address refusal changed payout: %s", got)
	}
}

func TestPayoutFailureAndSuspensionOverlapKeepsAddressReview(t *testing.T) {
	for _, suspendFirst := range []bool{true, false} {
		name := "record-first"
		if suspendFirst {
			name = "suspend-first"
		}
		t.Run(name, func(t *testing.T) {
			p := newPayEnv(t)
			p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
			order := p.completedWithPayout("tx-overlap-" + name)
			id := p.payoutID(order)
			if _, err := p.DB.Exec("UPDATE payouts SET state='sending',updated=now() WHERE id=$1", id); err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			record := func() error {
				return p.A.recordPayout(context.Background(), pid, order, p.vendor.ID, "failed", "", "The wallet reported a pre-broadcast error; nothing was broadcast. test refusal", false, "TESTNET payout failed", "TESTNET payout failed")
			}
			suspend := func() int {
				return p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)).Code
			}
			if suspendFirst {
				if code := suspend(); code != 303 {
					t.Fatalf("suspend: %d", code)
				}
				if err := record(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := record(); err != nil {
					t.Fatal(err)
				}
				if code := suspend(); code != 303 {
					t.Fatalf("suspend: %d", code)
				}
			}
			if got := p.str("SELECT state||'|'||error FROM payouts WHERE id=$1", id); !strings.HasPrefix(got, "failed|The wallet reported") || !strings.Contains(got, suspendedRequeue) {
				t.Fatalf("failure/suspension order lost address review: %q", got)
			}
		})
	}
}

func TestPayoutFailureRacingSuspensionKeepsAddressReview(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-record-suspend-race")
	id := p.payoutID(order)
	pid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var blocker int
	if err = lock.QueryRow("UPDATE payouts SET state='sending' WHERE id=$1 RETURNING pg_backend_pid()", id).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	suspended := make(chan int, 1)
	recorded := make(chan error, 1)
	go func() {
		suspended <- p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)).Code
	}()
	waitFor(t, func() bool {
		return p.count("SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", blocker) > 0
	})
	go func() {
		recorded <- p.A.recordPayout(context.Background(), pid, order, p.vendor.ID, "failed", "",
			"The wallet reported a pre-broadcast error; nothing was broadcast. test refusal", false,
			"TESTNET payout failed", "TESTNET payout failed")
	}()
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-suspended; code != 303 {
		t.Fatalf("suspend: %d", code)
	}
	if err = <-recorded; err != nil {
		t.Fatalf("record after overlapping suspension: %v", err)
	}
	if got := p.str("SELECT state||'|'||error FROM payouts WHERE id=$1", id); !strings.HasPrefix(got, "failed|The wallet reported") || !strings.Contains(got, suspendedRequeue) {
		t.Fatalf("racing failure/suspension lost address review: %q", got)
	}
}

func TestSuspendedStuckSendingNeedsBothChecks(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-stuck-suspend")
	id := p.payoutID(order)
	if _, err := p.DB.Exec("UPDATE payouts SET state='sending',updated=now()-interval '6 minutes' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	oldUpdated := p.str("SELECT updated::text FROM payouts WHERE id=$1", id)
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	if got := p.str("SELECT updated::text FROM payouts WHERE id=$1", id); got != oldUpdated {
		t.Fatalf("suspension reset stuck-send age: %q -> %q", oldUpdated, got)
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("restore", p.vendor.Handle)), 303)
	// Suspension's marker persists after restore even though the send never recorded an outcome.
	if got := p.str("SELECT error FROM payouts WHERE id=$1", id); !strings.Contains(got, suspendedRequeue) {
		t.Fatalf("stuck send lost suspension history: %q", got)
	}
	section := resolveSection(t, html.UnescapeString(p.page("/admin", p.adminSess)), id)
	mustContain(t, section, `name="not_broadcast" value="confirmed" required`, `name="address_checked" value="confirmed" required`)
	nonce := renderedFormNonce(t, section, "/admin/payout")
	w := p.do("POST", "/admin/payout", p.adminSess, suspendedRequeueForm(id, nonce, url.Values{"not_broadcast": {"confirmed"}}))
	if w.Code != 409 {
		t.Fatalf("stuck send without address check: %d %s", w.Code, w.Body.String())
	}
	if got := p.str("SELECT state FROM payouts WHERE id=$1", id); got != "sending" {
		t.Fatalf("refused stuck requeue changed state: %s", got)
	}
	p.check(p.do("POST", "/admin/payout", p.adminSess, suspendedRequeueForm(id, nonce, url.Values{"not_broadcast": {"confirmed"}, "address_checked": {"confirmed"}})), 303)
	if got := p.str("SELECT state FROM payouts WHERE id=$1", id); got != "pending" {
		t.Fatalf("checked stuck requeue state: %s", got)
	}
	// A later watcher pass sends the requeued payout once; no automatic send happened before the checks.
	p.poll()
	if p.sendsTo("fake-testnet-vendor") != 1 {
		t.Fatalf("stuck payout sent %d times", p.sendsTo("fake-testnet-vendor"))
	}
}

func TestSendingPayoutRecordedSentAfterSuspensionIsNotHeld(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-sent-after-suspend")
	id := p.payoutID(order)
	if _, err := p.DB.Exec("UPDATE payouts SET state='sending' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	if got := p.str("SELECT state||'|'||error FROM payouts WHERE id=$1", id); got != "sending|"+suspendedRequeue {
		t.Fatalf("suspension changed in-progress send: %q", got)
	}
	pid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.A.recordPayout(context.Background(), pid, order, p.vendor.ID, "sent", strings.Repeat("ab", 32), "", false,
		"TESTNET payout sent", "TESTNET payout sent"); err != nil {
		t.Fatal(err)
	}
	if got := p.str("SELECT state||'|'||error FROM payouts WHERE id=$1", id); got != "sent|" {
		t.Fatalf("recorded successful send was held or kept a failure marker: %q", got)
	}
	p.poll()
	if p.sendsTo("fake-testnet-vendor") != 0 {
		t.Fatal("already-recorded successful send repeated from the wallet")
	}
}

func TestConcurrentSuspensionAndRequeueNeverLeavesUnreviewedPending(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order, id := p.failPayout("tx-race-suspend", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	_, otherSession := p.user("other_admin_"+randomToken()[:6], "admin")
	// Pause suspension after it changed the recipient row, before it can visit payouts. The separate admin
	// requeue sees the previously committed unsuspended account and may queue this failed payout. Releasing
	// the barrier must make suspension recheck that now-pending row and hold it in its one payout UPDATE.
	if _, err := p.DB.Exec(`CREATE FUNCTION a112_pause_suspension() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(130112); RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec(`CREATE TRIGGER a112_pause_suspension AFTER UPDATE OF suspended_at ON users
		FOR EACH ROW WHEN (NEW.suspended_at IS NOT NULL) EXECUTE FUNCTION a112_pause_suspension()`); err != nil {
		t.Fatal(err)
	}
	lock, err := p.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var blocker int
	if _, err = lock.Exec("SELECT pg_advisory_xact_lock(130112)"); err != nil {
		t.Fatal(err)
	}
	if err = lock.QueryRow("SELECT pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	suspended := make(chan int, 1)
	go func() {
		suspended <- p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)).Code
	}()
	waitFor(t, func() bool {
		return p.count("SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", blocker) > 0
	})
	w := p.do("POST", "/admin/payout", otherSession, suspendedRequeueForm(id, randomToken()[:32], nil))
	if w.Code != 303 {
		t.Fatalf("requeue while suspension is paused: %d %s", w.Code, w.Body.String())
	}
	if state := p.str("SELECT state FROM payouts WHERE id=$1", id); state != "pending" {
		t.Fatalf("barrier failed to put requeue before suspension's payout update: %s", state)
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-suspended; code != 303 {
		t.Fatalf("suspend: %d", code)
	}
	if state := p.str("SELECT state FROM payouts WHERE id=$1", id); state != "held" {
		t.Fatalf("concurrent suspension left payout %s", state)
	}
	p.poll()
	if p.sendsTo("fake-testnet-vendor") != 0 {
		t.Fatalf("concurrent unreviewed requeue sent for %s", order)
	}
}

// A requeue that has locked the failed payout but not yet written pending must not slip between two separate
// suspension UPDATEs. The old first UPDATE skipped the still-failed row; its second UPDATE waited for requeue,
// then skipped the newly-pending row. One combined UPDATE waits and rechecks, then holds the pending payout.
func TestRequeueLockedFailedRowBeforeSuspensionStillEndsHeld(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order, id := p.failPayout("tx-locked-failed-suspend", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	_, otherSession := p.user("race_admin_"+randomToken()[:6], "admin")
	if _, err := p.DB.Exec(`CREATE FUNCTION a112_pause_pending_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF OLD.state='failed' AND NEW.state='pending' THEN
			PERFORM pg_advisory_xact_lock(130113);
		END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec(`CREATE TRIGGER a112_pause_pending_write BEFORE UPDATE ON payouts
		FOR EACH ROW EXECUTE FUNCTION a112_pause_pending_write()`); err != nil {
		t.Fatal(err)
	}
	barrier, err := p.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback()
	if _, err = barrier.Exec("SELECT pg_advisory_xact_lock(130113)"); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err = barrier.QueryRow("SELECT pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	requeued := make(chan int, 1)
	go func() {
		requeued <- p.do("POST", "/admin/payout", otherSession,
			suspendedRequeueForm(id, randomToken()[:32], nil)).Code
	}()
	waitFor(t, func() bool {
		return p.count("SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", blocker) > 0
	})
	suspended := make(chan int, 1)
	go func() {
		suspended <- p.do("POST", "/admin/suspend", p.adminSess,
			suspendForm("suspend", p.vendor.Handle)).Code
	}()
	waitFor(t, func() bool {
		return p.count(`SELECT count(*) FROM pg_stat_activity rq JOIN pg_stat_activity su
			ON rq.pid=ANY(pg_blocking_pids(su.pid)) WHERE $1=ANY(pg_blocking_pids(rq.pid))`, blocker) > 0
	})
	if err = barrier.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-requeued; code != 303 {
		t.Fatalf("requeue holding the old failed row: %d", code)
	}
	if code := <-suspended; code != 303 {
		t.Fatalf("suspend after locked requeue: %d", code)
	}
	if state := p.str("SELECT state FROM payouts WHERE id=$1", id); state != "held" {
		t.Fatalf("suspension missed the newly-pending payout: %s", state)
	}
	p.poll()
	if p.sendsTo("fake-testnet-vendor") != 0 {
		t.Fatalf("suspension let unreviewed requeue send order %s", order)
	}
}
