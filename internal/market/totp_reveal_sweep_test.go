package market

import (
	"context"
	"testing"
	"time"
)

// A-10: sealed plaintext recovery codes never outlive their display window, whether or not /totp is viewed.

func (e *testEnv) setReveal(userID, sealed, until string) {
	e.t.Helper()
	if _, err := e.DB.Exec("UPDATE users SET recovery_reveal=$2,recovery_reveal_until=now()+$3::interval WHERE id=$1", userID, sealed, until); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) reveal(userID string) (sealed string, untilNull bool) {
	e.t.Helper()
	if err := e.DB.QueryRow("SELECT recovery_reveal,recovery_reveal_until IS NULL FROM users WHERE id=$1", userID).Scan(&sealed, &untilNull); err != nil {
		e.t.Fatal(err)
	}
	return sealed, untilNull
}

func TestRecoveryRevealSweepClearsOnlyExpired(t *testing.T) {
	e := newTestApp(t)
	expired, _ := e.user("reveal-expired", "buyer")
	fresh, _ := e.user("reveal-fresh", "buyer")
	e.setReveal(expired, "sealed-expired", "-1 second")
	e.setReveal(fresh, "sealed-fresh", "9 minutes")
	if err := e.A.sweepRecoveryReveals(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, null := e.reveal(expired); s != "" || !null {
		t.Fatalf("expired reveal kept: %q until-null=%v", s, null)
	}
	if s, null := e.reveal(fresh); s != "sealed-fresh" || null {
		t.Fatalf("in-window reveal changed: %q until-null=%v", s, null)
	}
}

func TestRecoveryRevealExpiredClearedOnAccountView(t *testing.T) {
	e := newTestApp(t)
	uid, sess := e.user("reveal-view", "buyer")
	e.setReveal(uid, "sealed-view", "-1 second")
	e.check(e.do("GET", "/account", sess, nil), 200)
	if s, null := e.reveal(uid); s != "" || !null {
		t.Fatalf("expired reveal kept after /account: %q until-null=%v", s, null)
	}
	// An in-window reveal is left for /totp to display.
	e.setReveal(uid, "sealed-view", "9 minutes")
	e.check(e.do("GET", "/account", sess, nil), 200)
	if s, _ := e.reveal(uid); s != "sealed-view" {
		t.Fatalf("in-window reveal cleared by /account: %q", s)
	}
}

func TestRecoveryRevealSweepRunsInBackgroundAndStops(t *testing.T) {
	old := recoveryRevealSweepInterval
	recoveryRevealSweepInterval = 50 * time.Millisecond
	t.Cleanup(func() { recoveryRevealSweepInterval = old })
	e := newTestApp(t)
	expired, _ := e.user("reveal-bg", "buyer")
	fresh, _ := e.user("reveal-bg-fresh", "buyer")
	e.setReveal(fresh, "sealed-fresh", "9 minutes")
	e.A.Start(context.Background())
	// Set after Start so the periodic pass, not only the first one, must clear it.
	time.Sleep(100 * time.Millisecond)
	e.setReveal(expired, "sealed-bg", "-1 second")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s, _ := e.reveal(expired); s == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background sweep did not clear the expired reveal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s, _ := e.reveal(fresh); s != "sealed-fresh" {
		t.Fatalf("background sweep cleared an in-window reveal: %q", s)
	}
	done := make(chan struct{})
	go func() { e.A.stop(); e.A.background.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not stop on Close")
	}
	// Stopped: a newly expired reveal is no longer touched.
	e.setReveal(expired, "sealed-after-stop", "-1 second")
	time.Sleep(200 * time.Millisecond)
	if s, _ := e.reveal(expired); s != "sealed-after-stop" {
		t.Fatalf("sweep still running after stop: %q", s)
	}
}
