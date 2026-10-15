package market

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

// escrowNodeAlert is the escrow node alert of an admin page, for a failure message ("" when there is none).
func escrowNodeAlert(page string) string {
	_, alert, ok := strings.Cut(page, `id="escrow-node">`)
	if !ok {
		return ""
	}
	alert, _, _ = strings.Cut(alert, "</div>")
	return alert
}
