package market

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

func init() {
	registerLoader("admin", signInProtectionLoader)
	registerPreview("admin", func(d *PageData) {
		var off signInAttack
		d.CaptchaAdmin = &CaptchaAdminView{UnderAttack: true, Public: off.view(time.Now())}
		d.CaptchaAdmin.Public.Where, d.CaptchaAdmin.Public.Effective = "Attack signal", true
	})
}

func signInProtectionLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User != nil && d.User.Role == "admin" {
		now := time.Now()
		d.SignInRefusals = a.signInRefusals(now)
		d.AdminIngressRefusals = a.adminIngressRefusals(now)
		mode := captchaModeOf(d.Settings["captcha_required"])
		v := &CaptchaAdminView{
			Off: mode == captchaModeOff, UnderAttack: mode == captchaModeUnderAttack, Always: mode == captchaModeAlways,
			HasPrivate: a.adminSocket != "",
		}
		a.mu.Lock()
		v.Public, v.Private = a.signInAttackPublic.view(now), a.signInAttackPrivate.view(now)
		a.mu.Unlock()
		v.Public.Where, v.Private.Where = "Attack signal", "Attack signal on the private administrator onion"
		v.Public.Effective, v.Private.Effective = v.UnderAttack, v.UnderAttack
		v.ImageNow = v.Always || v.UnderAttack && v.Public.Active
		d.CaptchaAdmin = v
	}
	return nil
}

// The attack signal behind the sign-in image in the default mode (A-725). It is global on purpose: over Tor there is
// no per-client signal, and A-132's per-handle trigger was refused (D-24), since every unknown handle is a free bcrypt.
// It reads the two things the A-151 limiter work measures: passwords that failed bcrypt (signInAttackFailures in a
// fixed window) and sign-ins refused because every password slot was taken (signInAttackBusy in a fixed window: real
// sign-ins are failing). A malformed handle, a wrong CAPTCHA, a paused handle's refusal and a wrong rescue code cost
// the server no bcrypt and do not count. Either threshold holds the image on for signInAttackHold; only another trip
// extends it, and while it is on each failure needs a solved image. So the image never stays on without new evidence,
// but about 100 wrong passwords every 30 minutes do hold it on, which costs an attacker little: /admin/security and the
// log say so, and the operator can answer with the Off mode. The per-handle budgets apply in every mode.
//
// It is in memory, as those budgets are: a restart clears it, so it never locks anyone out without evidence from this
// process, and gives an attacker at most one threshold's worth of guesses more. Each ingress has its own (A-570), so
// the public listener cannot put the image on the private administrator sign-in.
const (
	signInAttackFailures      = 100
	signInAttackFailureWindow = 10 * time.Minute
	signInAttackBusy          = 10
	signInAttackBusyWindow    = time.Minute
	signInAttackHold          = 30 * time.Minute
	signInAttackTime          = "2006-01-02 15:04 UTC"
)

type signInAttack struct {
	failures, busy         int
	failuresFrom, busyFrom time.Time
	since, until           time.Time
	trips                  int
}

func (s *signInAttack) active(now time.Time) bool { return now.Before(s.until) }

// trip holds the signal on from now and reports whether it was off.
func (s *signInAttack) trip(now time.Time) bool {
	on := !s.active(now)
	if on {
		s.since = now
		s.trips++
	}
	s.until = now.Add(signInAttackHold)
	return on
}

// noteFailure counts one failed password and reports whether that turned the signal on.
func (s *signInAttack) noteFailure(now time.Time) bool {
	if now.Sub(s.failuresFrom) >= signInAttackFailureWindow {
		s.failures, s.failuresFrom = 0, now
	}
	s.failures++
	if s.failures < signInAttackFailures {
		return false
	}
	s.failures = 0
	return s.trip(now)
}

// noteBusy counts one sign-in refused for want of a password slot and reports whether that turned the signal on.
func (s *signInAttack) noteBusy(now time.Time) bool {
	if now.Sub(s.busyFrom) >= signInAttackBusyWindow {
		s.busy, s.busyFrom = 0, now
	}
	s.busy++
	if s.busy < signInAttackBusy {
		return false
	}
	s.busy = 0
	return s.trip(now)
}

func (s *signInAttack) view(now time.Time) SignInAttackView {
	v := SignInAttackView{Active: s.active(now), Trips: s.trips,
		FailureLimit: signInAttackFailures, BusyLimit: signInAttackBusy, HoldMinutes: int(signInAttackHold / time.Minute)}
	if v.Active {
		v.Since, v.Until = s.since.UTC().Format(signInAttackTime), s.until.UTC().Format(signInAttackTime)
	}
	if now.Sub(s.failuresFrom) < signInAttackFailureWindow {
		v.Failures = s.failures
	}
	if now.Sub(s.busyFrom) < signInAttackBusyWindow {
		v.Busy = s.busy
	}
	return v
}

// signInAttackFor is ctx's ingress's signal. Call it with a.mu held.
func (a *App) signInAttackFor(ctx context.Context) *signInAttack {
	if privateAdminIngress(ctx) {
		return &a.signInAttackPrivate
	}
	return &a.signInAttackPublic
}

func (a *App) signInAttackOn(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.signInAttackFor(ctx).active(time.Now())
}

func (a *App) noteSignInFailure(ctx context.Context) {
	a.noteSignInAttack(ctx, (*signInAttack).noteFailure)
}

func (a *App) noteSignInBusy(ctx context.Context) {
	a.noteSignInAttack(ctx, (*signInAttack).noteBusy)
}

// noteSignInAttack counts through note and logs the signal turning on, naming no handle, account or client.
func (a *App) noteSignInAttack(ctx context.Context, note func(*signInAttack, time.Time) bool) {
	a.mu.Lock()
	on := note(a.signInAttackFor(ctx), time.Now())
	a.mu.Unlock()
	if on {
		log.Printf("sign-in attack signal on private_ingress=%t", privateAdminIngress(ctx))
	}
}

// Copy while holding the limiter mutex, then sort outside it. Reading the panel
// neither spends nor clears a budget and never persists attempted handles.
func (a *App) signInRefusals(now time.Time) []SignInRefusal {
	a.mu.Lock()
	refused := pausedHandles(a.limits, "auth:", now)
	a.mu.Unlock()
	return sortRefusals(refused)
}

// adminIngressRefusals lists the handles paused on the private administrator ingress, whose budgets are its own
// (A-570): someone who can reach that onion is guessing there.
func (a *App) adminIngressRefusals(now time.Time) []SignInRefusal {
	a.mu.Lock()
	refused := pausedHandles(a.adminLimits, adminIngressPrefix+"auth:", now)
	a.mu.Unlock()
	return sortRefusals(refused)
}

// pausedHandles reads limits; call it with a.mu held.
func pausedHandles(limits map[string]bucket, prefix string, now time.Time) []SignInRefusal {
	var refused []SignInRefusal
	for key, b := range limits {
		if !strings.HasPrefix(key, prefix) || b.Count < 10 || now.After(b.Until) {
			continue
		}
		handle := strings.TrimPrefix(key, prefix)
		if handlePattern.MatchString(handle) {
			refused = append(refused, SignInRefusal{Handle: handle, Until: b.Until.UTC().Format("2006-01-02 15:04 UTC")})
		}
	}
	return refused
}

func sortRefusals(refused []SignInRefusal) []SignInRefusal {
	sort.Slice(refused, func(i, j int) bool { return refused[i].Handle < refused[j].Handle })
	return refused
}
