package market

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A-479: the pacing of the arbiter wallet reads. While a payer holds N fee bumps of a payment to the market child's
// address unconfirmed, one listsinceblock carries about N² bytes (QA4W6-1, Core 31.1: 3000 bumps 605 MB, the node
// growing by about 1.5 GB per read), and the escrow watcher's retry read the wallet every poll while escrow was off. A
// read that takes escrowArbiterHeavy or longer, or times out, now makes the next one wait, whatever it found: 1 minute,
// doubled after each such read, up to 1 hour; a quicker read, pass or failure, clears the wait (QA4W7-2: a failure that
// costs the node nothing is read again every poll). While a slow pass's wait runs it counts as a pass and the other start
// checks go on (QA4W7-1).

// paceClock replaces arbiterPaceNow with a clock that stands still until the test moves it (the returned func, safe
// from any goroutine), so a read takes no time unless the fake Core moves the clock while it answers.
func paceClock(t *testing.T) func(time.Duration) {
	t.Helper()
	var off atomic.Int64
	base, saved := time.Now(), arbiterPaceNow
	arbiterPaceNow = func() time.Time { return base.Add(time.Duration(off.Load())) }
	t.Cleanup(func() { arbiterPaceNow = saved })
	return func(d time.Duration) { off.Add(int64(d)) }
}

// The schedule alone (no database): from when the read ended, 1, 2, 4 … 32 minutes, then 1 hour at most, after slow
// reads; a quick one (under escrowArbiterHeavy), pass or failure, clears it; a slow pass doubles it as a slow failure
// does (P12); a timeout is slow whatever the clock says.
func TestEscrowArbiterPaceSchedule(t *testing.T) {
	a := &App{}
	t0 := time.Now()
	failed := &escrowArbiterScan{err: &escrowRPCError{Method: "listsinceblock", Class: "rpc:-1"}, end: t0, took: escrowArbiterHeavy}
	next := func() time.Duration {
		if a.escrowPace.next.IsZero() {
			return 0
		}
		return a.escrowPace.next.Sub(t0)
	}
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60} {
		if a.paceArbiter(failed); next() != want*time.Minute {
			t.Fatalf("failed read %d: the next read waits %v, want %v", i+1, next(), want*time.Minute)
		}
	}
	if a.paceArbiter(&escrowArbiterScan{end: t0, took: escrowArbiterHeavy - time.Millisecond}); next() != 0 || a.escrowPace.wait != 0 {
		t.Fatalf("a quick answer after the waits: the next read waits %v (wait %v), want none", next(), a.escrowPace.wait)
	}
	slow := &escrowArbiterScan{end: t0, took: escrowArbiterHeavy}
	if a.paceArbiter(slow); next() != time.Minute || a.escrowPace.last != slow {
		t.Fatalf("a slow answer: the next read waits %v, want 1m", next())
	}
	if a.paceArbiter(failed); next() != 2*time.Minute || a.escrowPace.last != failed {
		t.Fatalf("a failed read after a slow one: the next read waits %v, want 2m", next())
	}
	if a.paceArbiter(slow); next() != 4*time.Minute || a.escrowPace.last != slow {
		t.Fatalf("a slow pass after two slow reads: the next read waits %v, want 4m (a slow pass doubles the wait too)", next())
	}
	quickFail := &escrowArbiterScan{err: &escrowRPCError{Method: "listsinceblock", Class: "forbidden"}, end: t0, took: time.Second}
	if a.paceArbiter(quickFail); next() != 0 || a.escrowPace.wait != 0 {
		t.Fatalf("a quick failure: the next read waits %v (wait %v), want none", next(), a.escrowPace.wait)
	}
	timedOut := &escrowArbiterScan{err: &escrowRPCError{Method: "listsinceblock", Class: "timeout"}, end: t0}
	if a.paceArbiter(timedOut); next() != time.Minute {
		t.Fatalf("a read that timed out (0 s on the pacing clock): the next read waits %v, want 1m", next())
	}
	for d, want := range map[time.Duration]string{time.Second: "1 minute", time.Minute: "1 minute", time.Minute + time.Second: "2 minutes", time.Hour: "60 minutes"} {
		if got := aboutMinutes(d); got != want {
			t.Errorf("aboutMinutes(%v) = %q, want %q", d, got, want)
		}
	}
}

// Against the fake escrow Core: after a read that fails after 6 s, the watcher's retries within the wait do not call
// listsinceblock and keep the read-failure line, and /admin names the next check; the read after the wait happens; the
// wait doubles and stops at 1 hour; a quick pass starts escrow and clears the wait (the next slow failure waits 1 minute
// again); a slow pass starts escrow too and paces the next check: once escrow is off again within the wait, the retry
// does not read, takes that pass as a pass, and starts escrow (QA4W7-1).
func TestEscrowArbiterReadPacing(t *testing.T) {
	x := newEscrowEnv(t)
	logs := captureLog(t)
	ctx := context.Background()
	advance := paceClock(t)
	reads := func() int { return x.core.count("listsinceblock") }
	set := func(listErr int, before func()) {
		x.core.mu.Lock()
		x.core.arbiterListErr, x.core.beforeSince = listErr, before
		x.core.mu.Unlock()
	}
	x.core.mu.Lock()
	x.core.arbiterTxs, x.core.arbiterBirth = []string{"receive"}, 1700004000
	x.core.mu.Unlock()
	slow := func() { advance(6 * time.Second) } // a read that takes 6 s on the pacing clock
	set(-1, slow)
	x.restart() // startup's read is not paced: it reads, fails after 6 s, and the next read waits 1 minute
	if x.A.escrow.Load() != nil || x.A.escrowStartState != escrowLogArbiterRead {
		t.Fatalf("startup with a failing read: %q\n%s", x.A.escrowStartState, logs.String())
	}
	failedPage := func(minutes string) {
		t.Helper()
		page := x.adminPage()
		if !strings.Contains(page, "the arbiter wallet's transactions could not be read (listsinceblock rpc:-1)") ||
			!strings.Contains(page, "The next check is in about "+minutes+".") {
			t.Fatalf("the admin page does not name the next check in about %s:\n%s", minutes, escrowNodeAlert(page))
		}
	}
	failedPage("1 minute")
	if !strings.Contains(logs.String(), "escrow: the arbiter wallet's read took 6s and failed (listsinceblock rpc:-1); the next read waits about 1 minute") {
		t.Fatalf("no pacing line:\n%s", logs.String())
	}

	retry := &escrowRetry{last: x.A.escrowStartState}
	n := reads()
	for i, wait := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
		wait *= time.Minute
		advance(wait - 10*time.Second)
		x.A.retryEscrow(ctx, retry)
		if r := reads(); r != n {
			t.Fatalf("wait %d (%v): a retry %v after the failed read called listsinceblock %d times", i+1, wait, wait-10*time.Second, r-n)
		}
		if retry.last != escrowLogArbiterRead || x.A.escrow.Load() != nil {
			t.Fatalf("wait %d: a retry within it left %q", i+1, retry.last)
		}
		if i == 0 {
			failedPage("1 minute") // 10 s left
		}
		advance(10 * time.Second)
		if x.A.retryEscrow(ctx, retry); reads() != n+1 {
			t.Fatalf("wait %d (%v): the retry once it ended called listsinceblock %d times, want once", i+1, wait, reads()-n)
		}
		n++
		if i == 0 {
			failedPage("2 minutes")
		}
	}
	failedPage("60 minutes")

	// A quick pass: escrow on, the wait cleared.
	set(0, nil)
	advance(time.Hour)
	if x.A.retryEscrow(ctx, retry); x.A.escrow.Load() == nil || retry.last != escrowLogOn || reads() != n+1 {
		t.Fatalf("a quick pass after the wait: %q, %d reads\n%s", retry.last, reads()-n, logs.String())
	}
	n++
	if strings.Contains(x.adminPage(), `id="escrow-node"`) {
		t.Fatal("the admin line stayed after escrow started")
	}
	// Escrow goes off (as the daily pooled key self-check turns it off), and the next read fails: it is read at once (no
	// wait left), and after the failure the wait is 1 minute again, not the hour before the pass.
	e := x.A.escrow.Load()
	x.A.escrow.CompareAndSwap(e, nil)
	set(-1, slow)
	if x.A.retryEscrow(ctx, retry); reads() != n+1 || retry.last != escrowLogArbiterRead {
		t.Fatalf("escrow off after a quick pass: %q, %d reads", retry.last, reads()-n)
	}
	n++
	set(0, nil)
	advance(time.Minute)
	if x.A.retryEscrow(ctx, retry); reads() != n+1 || x.A.escrow.Load() == nil {
		t.Fatalf("a minute after the failure that followed a quick pass: %q, %d reads (the pass did not clear the wait)", retry.last, reads()-n)
	}
	n++

	// A slow pass (6 s on the pacing clock): escrow on, and the next check paced. While escrow is on nothing reads the
	// arbiter wallet; once it is off again within the wait, the retry does not read, and says why.
	e = x.A.escrow.Load()
	x.A.escrow.CompareAndSwap(e, nil)
	set(0, slow)
	if x.A.retryEscrow(ctx, retry); reads() != n+1 || x.A.escrow.Load() == nil || retry.last != escrowLogOn {
		t.Fatalf("a slow pass: %q, %d reads", retry.last, reads()-n)
	}
	n++
	if !strings.Contains(logs.String(), "escrow: the arbiter wallet's read took 6s and passed; the next read waits about 1 minute") {
		t.Fatalf("no pacing line after the slow pass:\n%s", logs.String())
	}
	set(0, nil)
	advance(20 * time.Second)
	if x.A.retryEscrow(ctx, retry); reads() != n {
		t.Fatal("a retry with escrow on read the arbiter wallet")
	}
	e = x.A.escrow.Load()
	x.A.escrow.CompareAndSwap(e, nil)
	retry.last = escrowLogPooledKey // as the pooled key self-check leaves it
	if x.A.retryEscrow(ctx, retry); reads() != n || x.A.escrow.Load() == nil || retry.last != escrowLogOn {
		t.Fatalf("escrow off within the slow pass's wait: %q, %d reads (want no read, the pass held, escrow on)\n%s", retry.last, reads()-n, logs.String())
	}
	if strings.Contains(x.adminPage(), `id="escrow-node"`) {
		t.Fatal("an admin line after escrow started on the held pass")
	}
}

// QA4W7-1: a slow read that passes while a later start check fails (the watch-only wallet not loaded). The operator
// fixes that check, and the next retry, within the slow pass's wait, starts escrow without reading the arbiter wallet
// again: a held pass counts as a pass, and nothing blames fee bumps for the watch-only wallet.
func TestEscrowArbiterSlowPassThenLaterCheckFails(t *testing.T) {
	x := newEscrowEnv(t)
	logs := captureLog(t)
	ctx := context.Background()
	advance := paceClock(t)
	x.core.mu.Lock()
	x.core.arbiterTxs, x.core.arbiterBirth = []string{"receive"}, 1700009000
	watch := x.core.watchInfo
	x.core.watchInfo = nil // the watch-only wallet is not loaded
	x.core.beforeSince = func() { advance(6 * time.Second) }
	x.core.mu.Unlock()
	x.restart()
	if x.A.escrow.Load() != nil || x.A.escrowStartState != escrowLogWatch {
		t.Fatalf("startup with a slow pass and no watch-only wallet: %q\n%s", x.A.escrowStartState, logs.String())
	}
	x.core.mu.Lock()
	x.core.watchInfo = watch // the operator loads the watch-only wallet
	x.core.beforeSince = nil
	x.core.mu.Unlock()
	advance(30 * time.Second)
	retry := &escrowRetry{last: x.A.escrowStartState}
	before := x.core.count("listsinceblock")
	if x.A.retryEscrow(ctx, retry); x.A.escrow.Load() == nil || retry.last != escrowLogOn {
		t.Fatalf("the retry 30 s later, the watch-only wallet loaded: %q, admin %q\n%s", retry.last, escrowNodeAlert(x.adminPage()), logs.String())
	}
	if n := x.core.count("listsinceblock") - before; n != 0 {
		t.Fatalf("the retry within the slow pass's wait read the arbiter wallet %d times", n)
	}
}

// P6: a slow read that found a spend keeps "used for the escrow key only" while its wait runs, without reading again.
func TestEscrowArbiterSlowSpendHeld(t *testing.T) {
	x := newEscrowEnv(t)
	ctx := context.Background()
	advance := paceClock(t)
	x.core.mu.Lock()
	x.core.arbiterTxs, x.core.arbiterBirth = []string{"receive", "send"}, 1700009100
	x.core.beforeSince = func() { advance(6 * time.Second) }
	x.core.mu.Unlock()
	x.restart()
	if x.A.escrow.Load() != nil || x.A.escrowStartState != escrowLogArbiterUse {
		t.Fatalf("startup with a slow read that found a spend: %q", x.A.escrowStartState)
	}
	retry := &escrowRetry{last: x.A.escrowStartState}
	before := x.core.count("listsinceblock")
	advance(20 * time.Second)
	if x.A.retryEscrow(ctx, retry); retry.last != escrowLogArbiterUse || x.A.escrow.Load() != nil {
		t.Fatalf("a retry within the wait after a slow read that found a spend: %q, want %q", retry.last, escrowLogArbiterUse)
	}
	if n := x.core.count("listsinceblock") - before; n != 0 {
		t.Fatalf("the retry within the wait read the arbiter wallet %d times", n)
	}
}

// QA4W7-2: only a slow read is paced. A failure that answers at once (an RPC error, as for listsinceblock missing from
// the whitelist) costs the node nothing and is read again every poll, with no wait on /admin; a failure after 6 s, or a
// read that times out, is paced.
func TestEscrowArbiterQuickFailureNotPaced(t *testing.T) {
	x := newEscrowEnv(t)
	logs := captureLog(t)
	ctx := context.Background()
	advance := paceClock(t)
	reads := func() int { return x.core.count("listsinceblock") }
	x.core.mu.Lock()
	x.core.arbiterTxs, x.core.arbiterBirth, x.core.arbiterListErr = []string{"receive"}, 1700009300, -1
	x.core.mu.Unlock()
	x.restart()
	if x.A.escrowStartState != escrowLogArbiterRead {
		t.Fatalf("startup with an instant failure: %q", x.A.escrowStartState)
	}
	retry := &escrowRetry{last: x.A.escrowStartState}
	n := reads()
	for i := range 5 {
		advance(30 * time.Second)
		if x.A.retryEscrow(ctx, retry); reads() != n+1 || retry.last != escrowLogArbiterRead {
			t.Fatalf("poll %d after instant failures: %d reads, %q; want one read each poll", i+1, reads()-n, retry.last)
		}
		n++
	}
	if page := x.adminPage(); !strings.Contains(page, "could not be read (listsinceblock rpc:-1)") || !strings.Contains(page, "The next check is at the next poll.") {
		t.Fatalf("the admin page after instant failures:\n%s", escrowNodeAlert(page))
	}
	if strings.Contains(logs.String(), "the next read waits") {
		t.Fatalf("an instant failure was paced:\n%s", logs.String())
	}

	// A failure after 6 s: the next poll does not read.
	x.core.mu.Lock()
	x.core.beforeSince = func() { advance(6 * time.Second) }
	x.core.mu.Unlock()
	advance(30 * time.Second)
	if x.A.retryEscrow(ctx, retry); reads() != n+1 {
		t.Fatalf("the slow failure's read: %d reads", reads()-n)
	}
	n++
	x.core.mu.Lock()
	x.core.beforeSince = nil
	x.core.mu.Unlock()
	advance(30 * time.Second)
	if x.A.retryEscrow(ctx, retry); reads() != n {
		t.Fatalf("a poll 30 s after a failure that took 6 s read again (%d reads)", reads()-n)
	}

	// A read that times out is slow, whatever the pacing clock says (here it stands still): paced.
	savedRead := escrowArbiterReadTimeout
	escrowArbiterReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { escrowArbiterReadTimeout = savedRead })
	x.core.mu.Lock()
	x.core.arbiterListErr, x.core.beforeSince = 0, func() { time.Sleep(time.Second) }
	x.core.mu.Unlock()
	advance(time.Hour)
	if x.A.retryEscrow(ctx, retry); reads() != n+1 || !strings.Contains(logs.String(), "failed (listsinceblock timeout); the next read waits about") {
		t.Fatalf("a read that timed out: %d reads, %q\n%s", reads()-n, retry.last, logs.String())
	}
	n++
	advance(30 * time.Second)
	if x.A.retryEscrow(ctx, retry); reads() != n {
		t.Fatalf("a poll 30 s after a read that timed out read again (%d reads)", reads()-n)
	}
}

// P10: the wait lives in the App, not the process: restarting the app reads the arbiter wallet at once, whatever wait the
// last App had, and the new App starts with none.
func TestEscrowArbiterRestartReadsAtOnce(t *testing.T) {
	x := newEscrowEnv(t)
	ctx := context.Background()
	advance := paceClock(t)
	x.core.mu.Lock()
	x.core.arbiterTxs, x.core.arbiterBirth, x.core.arbiterListErr = []string{"receive"}, 1700009400, -1
	x.core.beforeSince = func() { advance(6 * time.Second) }
	x.core.mu.Unlock()
	x.restart()
	retry := &escrowRetry{last: x.A.escrowStartState}
	for _, w := range []time.Duration{1, 2, 4, 8, 16, 32} {
		advance(w * time.Minute)
		x.A.retryEscrow(ctx, retry)
	}
	if x.A.escrowPace.wait != time.Hour {
		t.Fatalf("wait %v, want 1h", x.A.escrowPace.wait)
	}
	x.core.mu.Lock()
	x.core.arbiterListErr, x.core.beforeSince = 0, nil
	x.core.mu.Unlock()
	advance(time.Minute)
	n := x.core.count("listsinceblock")
	x.restart()
	if x.core.count("listsinceblock") != n+1 || x.A.escrow.Load() == nil {
		t.Fatalf("a restart within the hour's wait: %d reads, state %q; want one read and escrow on", x.core.count("listsinceblock")-n, x.A.escrowStartState)
	}
	if p := x.A.escrowPace; !p.next.IsZero() || p.wait != 0 {
		t.Fatalf("the new App's pace after a quick read: next %v, wait %v; want none", p.next, p.wait)
	}
}

// P9: the read's duration covers the whole read: a first call from the checkpoint that takes 6 s and answers -5 (a block
// the node does not know), then a quick re-read from the start, is a slow read and paces the next.
func TestEscrowArbiterSlowFirstCallPaced(t *testing.T) {
	x := newEscrowEnv(t)
	logs := captureLog(t)
	advance := paceClock(t)
	var calls atomic.Int32
	x.core.mu.Lock()
	x.core.arbiterTxs, x.core.arbiterBirth = []string{"receive"}, 1700009500
	x.core.beforeSince = func() {
		if calls.Add(1) == 1 {
			advance(6 * time.Second)
		}
	}
	x.core.mu.Unlock()
	unknown := strings.Repeat("ab", 32)
	if _, err := x.DB.Exec("INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=excluded.value", escrowArbiterChecked,
		`{"wallet":"opsecmkt-arbiter","birthtime":1700009500,"block":"`+unknown+`"}`); err != nil {
		t.Fatal(err)
	}
	x.restart()
	if x.A.escrow.Load() == nil || calls.Load() != 2 {
		t.Fatalf("startup with an unknown checkpoint block: %q, %d calls", x.A.escrowStartState, calls.Load())
	}
	if p := x.A.escrowPace; p.wait != time.Minute || !strings.Contains(logs.String(), "the arbiter wallet's read took 6s and passed") {
		t.Fatalf("a slow first call and a quick re-read: wait %v, want 1m\n%s", p.wait, logs.String())
	}
}

// escrowNodeAlert is the escrow node alert of an admin page, for a failure message ("" when there is none).
func escrowNodeAlert(page string) string {
	_, alert, ok := strings.Cut(page, `id="escrow-node">`)
	if !ok {
		return ""
	}
	alert, _, _ = strings.Cut(alert, "</div>")
	return alert
}
