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
// read that fails, or answers in escrowArbiterHeavy or longer, now makes the next one wait: 1 minute, doubled after each
// such read, up to 1 hour; a quick answer clears the wait.

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

// The schedule alone (no database): from when the read ended, 1, 2, 4 … 32 minutes, then 1 hour at most, after reads
// that failed; a quick answer (under escrowArbiterHeavy) clears it, a slow one is paced as a failure is.
func TestEscrowArbiterPaceSchedule(t *testing.T) {
	a := &App{}
	t0 := time.Now()
	failed := &escrowArbiterScan{err: &escrowRPCError{Method: "listsinceblock", Class: "timeout"}, end: t0, took: escrowArbiterReadTimeout}
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
	for d, want := range map[time.Duration]string{time.Second: "1 minute", time.Minute: "1 minute", time.Minute + time.Second: "2 minutes", time.Hour: "60 minutes"} {
		if got := aboutMinutes(d); got != want {
			t.Errorf("aboutMinutes(%v) = %q, want %q", d, got, want)
		}
	}
}

// Against the fake escrow Core: after a read that fails, the watcher's retries within the wait do not call
// listsinceblock and keep the read-failure line, and /admin names the next check; the read after the wait happens; the
// wait doubles and stops at 1 hour; a quick pass starts escrow and clears the wait (the next failure waits 1 minute
// again); a slow pass starts escrow too and paces the next check, which, once escrow is off again, waits with its own
// line and admin alert.
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
	set(-1, nil)
	x.restart() // startup's read is not paced: it reads, fails, and the next read waits 1 minute
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
	if !strings.Contains(logs.String(), "escrow: the arbiter wallet's read failed (listsinceblock rpc:-1); the next read waits about 1 minute") {
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
	set(-1, nil)
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
	set(0, func() { advance(6 * time.Second) })
	if x.A.retryEscrow(ctx, retry); reads() != n+1 || x.A.escrow.Load() == nil || retry.last != escrowLogOn {
		t.Fatalf("a slow pass: %q, %d reads", retry.last, reads()-n)
	}
	n++
	if !strings.Contains(logs.String(), "escrow: the arbiter wallet's read took 6s (5 s or more); the next read waits about 1 minute") {
		t.Fatalf("no pacing line after the slow pass:\n%s", logs.String())
	}
	set(0, nil)
	advance(20 * time.Second)
	if x.A.retryEscrow(ctx, retry); reads() != n {
		t.Fatal("a retry with escrow on read the arbiter wallet")
	}
	e = x.A.escrow.Load()
	x.A.escrow.CompareAndSwap(e, nil)
	if x.A.retryEscrow(ctx, retry); reads() != n || x.A.escrow.Load() != nil || retry.last != escrowLogArbiterPaced ||
		!strings.Contains(logs.String(), escrowLogArbiterPaced) {
		t.Fatalf("escrow off within the slow pass's wait: %q, %d reads\n%s", retry.last, reads()-n, logs.String())
	}
	if page := x.adminPage(); !strings.Contains(page, "Bitcoin escrow off until the arbiter wallet's next check, in about 1 minute.") {
		t.Fatalf("the admin page does not say escrow waits for the next check:\n%s", escrowNodeAlert(page))
	}
	advance(40 * time.Second)
	if x.A.retryEscrow(ctx, retry); reads() != n+1 || x.A.escrow.Load() == nil || retry.last != escrowLogOn {
		t.Fatalf("the check after the slow pass's wait: %q, %d reads", retry.last, reads()-n)
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
