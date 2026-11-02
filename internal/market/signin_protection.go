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
		v.Public.LastTripAt, v.Public.LastTrip = lastSignInTrip(d.Settings[settingSignInAttackLastPublic])
		v.Private.LastTripAt, v.Private.LastTrip = lastSignInTrip(d.Settings[settingSignInAttackLastPrivate])
		v.Private.HoldPosts = signInAttackBusy + cap(adminPasswordWork)
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
// sign-ins are failing). A sign-in refused while a registration holds a slot counts apart (SEC102-1): the image stops no
// registration, but a right password refunds its budget and fails nothing, so no other count would see these people kept
// out. signInAttackHeldBusy of them in the same window, more because a registration flood alone can refuse some, trip the
// same signal. A refused registration with no sign-in attempt never does; it is only counted, for /admin/security. A
// malformed handle, a wrong CAPTCHA, a paused handle's refusal and a wrong rescue code cost
// the server no bcrypt and do not count. Either threshold holds the image on for signInAttackHold; only another trip
// extends it, and while it is on each failure needs a solved image. So the image never stays on without new evidence,
// but about 100 wrong passwords every 30 minutes, or about 14 simultaneous sign-in posts (the 4 slots and 10 refusals),
// do hold it on, which costs an attacker little: /admin/security, the log and one notification per hold say so, and the
// operator can answer with the Off mode. The per-handle budgets apply in every mode.
//
// It is in memory, as those budgets are: a restart clears it, so it never locks anyone out without evidence from this
// process, and gives an attacker at most one threshold's worth of guesses more. Each ingress has its own (A-570), so
// the public listener cannot put the image on the private administrator sign-in. Only the record of the last trip is
// stored (PM109-3): which count turned it on and when, in one settings row per ingress, with no handle, account or client.
const (
	signInAttackFailures      = 100
	signInAttackFailureWindow = 10 * time.Minute
	signInAttackBusy          = 10
	signInAttackBusyWindow    = time.Minute
	signInAttackHeldBusy      = 30 // in signInAttackBusyWindow
	signInAttackHold          = 30 * time.Minute
	signInAttackTime          = "2006-01-02 15:04 UTC"

	settingSignInAttackLastPublic  = "sign_in_attack_last_public"
	settingSignInAttackLastPrivate = "sign_in_attack_last_private"

	signInRouteFailedPasswords = "failed_passwords"
	signInRouteRefused         = "refused_sign_ins"
	signInRouteHeldSlot        = "registration_held_slot"
)

// signInAttackRoutes words each count that can turn the signal on, for the notification and /admin/security. A stored
// route outside this table reads as no record.
var signInAttackRoutes = map[string]string{
	signInRouteFailedPasswords: "many failed passwords",
	signInRouteRefused:         "many sign-ins refused because every password slot was busy",
	signInRouteHeldSlot:        "many sign-ins refused while a registration held a password slot",
}

// cutTrip splits a stored trip, route|time.
func cutTrip(stored string) (route, at string, ok bool) { return strings.Cut(stored, "|") }

// lastSignInTrip is a stored trip for the panel: when, and the wording of its route. An unreadable row is no record.
func lastSignInTrip(stored string) (at, route string) {
	r, when, ok := cutTrip(stored)
	label, known := signInAttackRoutes[r]
	if !ok || !known {
		return "", ""
	}
	if _, err := time.Parse(signInAttackTime, when); err != nil {
		return "", ""
	}
	return when, label
}

type signInAttack struct {
	failures, busy         int
	failuresFrom, busyFrom time.Time
	since, until           time.Time
	trips                  int
	// Per signInAttackBusyWindow: sign-ins refused while a registration held a slot, and registrations refused for want of one.
	heldBusy, registerBusy int
	heldFrom, registerFrom time.Time
	// Registrations refused for want of a slot since this process started, and when the last was (PM109-6).
	registerBusyTotal int
	registerBusyLast  time.Time
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

// noteHeldBusy counts one sign-in refused while a registration held a password slot and reports whether that turned the
// signal on.
func (s *signInAttack) noteHeldBusy(now time.Time) bool {
	if now.Sub(s.heldFrom) >= signInAttackBusyWindow {
		s.heldBusy, s.heldFrom = 0, now
	}
	s.heldBusy++
	if s.heldBusy < signInAttackHeldBusy {
		return false
	}
	s.heldBusy = 0
	return s.trip(now)
}

// noteRegisterBusy counts one registration refused for want of a slot, for /admin/security alone: it never trips.
func (s *signInAttack) noteRegisterBusy(now time.Time) {
	if now.Sub(s.registerFrom) >= signInAttackBusyWindow {
		s.registerBusy, s.registerFrom = 0, now
	}
	s.registerBusy++
	s.registerBusyTotal++
	s.registerBusyLast = now
}

func (s *signInAttack) view(now time.Time) SignInAttackView {
	v := SignInAttackView{Active: s.active(now), Trips: s.trips, HoldPosts: signInAttackBusy + cap(passwordWork),
		FailureLimit: signInAttackFailures, BusyLimit: signInAttackBusy, HeldBusyLimit: signInAttackHeldBusy, HoldMinutes: int(signInAttackHold / time.Minute)}
	if v.Active {
		v.Since, v.Until = s.since.UTC().Format(signInAttackTime), s.until.UTC().Format(signInAttackTime)
	}
	if now.Sub(s.failuresFrom) < signInAttackFailureWindow {
		v.Failures = s.failures
	}
	if now.Sub(s.busyFrom) < signInAttackBusyWindow {
		v.Busy = s.busy
	}
	if now.Sub(s.heldFrom) < signInAttackBusyWindow {
		v.HeldBusy = s.heldBusy
	}
	if now.Sub(s.registerFrom) < signInAttackBusyWindow {
		v.RegisterBusy = s.registerBusy
	}
	v.RegisterBusyTotal = s.registerBusyTotal
	if !s.registerBusyLast.IsZero() {
		v.RegisterBusyLast = s.registerBusyLast.UTC().Format(signInAttackTime)
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
	a.noteSignInAttack(ctx, signInRouteFailedPasswords, (*signInAttack).noteFailure)
}

func (a *App) noteSignInBusy(ctx context.Context) {
	a.noteSignInAttack(ctx, signInRouteRefused, (*signInAttack).noteBusy)
}

// noteSignInHeldBusy counts a sign-in refused while a registration held a password slot.
func (a *App) noteSignInHeldBusy(ctx context.Context) {
	a.noteSignInAttack(ctx, signInRouteHeldSlot, (*signInAttack).noteHeldBusy)
}

// noteRegisterBusy counts a registration refused for want of a slot.
func (a *App) noteRegisterBusy(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.signInAttackFor(ctx).noteRegisterBusy(time.Now())
}

// noteSignInAttack counts through note and, when that turns the signal on, logs it with which count did and notifies the
// administrators, naming no handle, account or client. An extension does neither, so that is at most once per hold.
func (a *App) noteSignInAttack(ctx context.Context, route string, note func(*signInAttack, time.Time) bool) {
	a.mu.Lock()
	on := note(a.signInAttackFor(ctx), time.Now())
	a.mu.Unlock()
	if on {
		log.Printf("sign-in attack signal on private_ingress=%t route=%s", privateAdminIngress(ctx), route)
		a.notifySignInAttack(ctx, route)
	}
}

// notifySignInAttack tells every active administrator the signal turned on and which count did (SEC725-6, PM109-3), and
// stores that as the last trip. It is best effort with a bound of its own, whatever became of the request that tripped
// it: a failure is logged by class and refuses no one, and the log line above still names the route.
func (a *App) notifySignInAttack(ctx context.Context, route string) {
	if a.db == nil {
		return
	}
	where, key := "Sign-in attack signal on: ", settingSignInAttackLastPublic
	if privateAdminIngress(ctx) {
		where, key = "Sign-in attack signal on the private administrator onion: ", settingSignInAttackLastPrivate
	}
	stored := route + "|" + time.Now().UTC().Format(signInAttackTime)
	body := where + signInAttackRoutes[route] + ". If the human check is Only under attack, everyone signing in there must read the image for at least 30 minutes, and people using a screen reader cannot sign in. Security & access says what to do."
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, stored)
		if err == nil {
			err = notifyAdminsExcept(ctx, tx, body, "")
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		log.Printf("sign-in attack notification: %s", errorCause(err)) // a class, never the driver's text (A-170)
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
