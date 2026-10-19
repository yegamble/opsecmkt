package market

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Monero escrow health (A-553, owner request 2026-10-18; STAFF66-3 as ruled in Round D). The Monero escrow watcher
// records, at the end of each pass it runs (under its advisory lock, so one process writes at a time), one settings
// row: when the pass ended and the last good pass ended, by the database's clock; why the latest pass was not good
// (watcherCause's classes, or the not-ready or not-synced state); whether wallet creation is stopped because
// ESCROW_XMR_SEED_KEY is not the pinned seed key; and what this process's calls last showed of the daemon and the
// wallet-rpc (xmrSeen: answered, last failure class, versions, height). Every app process reads the same row, and it
// survives a restart or redeploy: per-process memory was wrong across processes and lost on redeploy (QA66). Nothing in
// it is a URL, host, address or credential. Admin → Payments shows the watcher's health and Admin → System the nodes'
// in the dependency table's Health column. No migration: settings holds it, as A-552's scanner report.

const (
	settingXMRHealth = "escrow_xmr_health"
	// xmrHealthStaleAfter: no pass recorded for this long means the watcher has stopped. A pass may take up to
	// xmrEscrowPassTimeout (10 minutes) and runs every POLL_INTERVAL (30 seconds by default).
	xmrHealthStaleAfter = 15 * time.Minute
	xmrHealthCauseLimit = 600 // bytes of cause kept: a pass over many wallets can join many failures
)

// xmrHealthRecord is the stored row.
type xmrHealthRecord struct {
	Checked         time.Time `json:"checked"`         // the latest pass's end
	Good            time.Time `json:"good,omitzero"`   // the latest good pass's end (zero: none recorded)
	Cause           string    `json:"cause,omitempty"` // why the latest pass was not good; "" when it was
	CreationStopped bool      `json:"creation_stopped,omitempty"`
	Daemon          xmrSeen   `json:"daemon"`
	Wallet          xmrSeen   `json:"wallet"`
}

func init() { registerLoader("admin", xmrEscrowHealthLoader) }

// xmrPassCause is why a pass was not good, "" when it was: escrow ready, the daemon synced, every wallet read and no
// error (the provider episode's rule, provider_episodes.go). Fixed state words and watcherCause's classes only.
func (a *App) xmrPassCause(synced, unread bool, outcome error) string {
	x := a.escrowXMR
	var parts []string
	switch {
	case !x.ready.Load():
		state, _ := x.notReady()
		parts = append(parts, "Monero escrow not ready: "+state)
	case !synced && outcome == nil:
		why, _ := x.sync.Load().(string)
		parts = append(parts, "the escrow daemon is not synced ("+cmp.Or(why, "reason not recorded")+"); wallets were not read")
	}
	if unread && outcome == nil {
		parts = append(parts, "an escrow wallet was not read: another wallet program has it open, or it does not open with the market's password")
	}
	if outcome != nil {
		parts = append(parts, watcherCause(outcome))
	}
	cause := strings.Join(parts, "; ")
	if len(cause) > xmrHealthCauseLimit {
		cause = strings.ToValidUTF8(cause[:xmrHealthCauseLimit], "") + "…"
	}
	return cause
}

// noteXMRHealth stores the pass's health (cause "" for a good pass). It runs after the pass, even one cut short by
// its timeout, and never turns a pass into a failure: a write error is logged by class and the next pass retries.
func (a *App) noteXMRHealth(ctx context.Context, cause string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.saveXMRHealth(ctx, cause); err != nil {
		log.Printf("monero escrow health: state not saved (%s)", errorCause(err))
	}
}

func (a *App) saveXMRHealth(ctx context.Context, cause string) error {
	x := a.escrowXMR
	var raw sql.NullString
	var now time.Time
	if err := a.db.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=$1),clock_timestamp()`, settingXMRHealth).Scan(&raw, &now); err != nil {
		return err
	}
	var prev xmrHealthRecord
	if raw.Valid {
		_ = json.Unmarshal([]byte(raw.String), &prev) // an unreadable row is replaced
	}
	rec := xmrHealthRecord{Checked: now.UTC(), Good: prev.Good, Cause: cause, CreationStopped: x.seedKeyMismatch()}
	if cause == "" {
		rec.Good = rec.Checked
	}
	daemon, wallet := x.rpc.seen()
	if x.ready.Load() {
		daemon.Sync, _ = x.sync.Load().(string)
	}
	rec.Daemon, rec.Wallet = daemon.over(prev.Daemon), wallet.over(prev.Wallet)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, settingXMRHealth, string(b))
	return err
}

// over is s with what an earlier record knew and s does not: a process starts having seen nothing, so a restart keeps
// the last successful check, the last failure and the versions until this process sees newer ones.
func (s xmrSeen) over(prev xmrSeen) xmrSeen {
	if prev.OK.After(s.OK) {
		s.OK = prev.OK
	}
	if prev.ErrorAt.After(s.ErrorAt) {
		s.Error, s.ErrorAt = prev.Error, prev.ErrorAt
	}
	if s.InfoAt.IsZero() {
		s.Release, s.InfoAt, s.Height, s.Target, s.Synchronized = prev.Release, prev.InfoAt, prev.Height, prev.Target, prev.Synchronized
	}
	if s.RPC == "" {
		s.RPC = prev.RPC
	}
	return s
}

// xmrHealthStatus is the stored row as one page reads it.
type xmrHealthStatus struct {
	rec   xmrHealthRecord
	found bool      // a pass has been recorded
	now   time.Time // the database's clock
	stale bool      // no pass recorded for xmrHealthStaleAfter
}

func (a *App) xmrHealth(ctx context.Context) (xmrHealthStatus, error) {
	var raw sql.NullString
	var h xmrHealthStatus
	if err := a.db.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=$1),clock_timestamp()`, settingXMRHealth).Scan(&raw, &h.now); err != nil {
		return h, err
	}
	h.found = raw.Valid && json.Unmarshal([]byte(raw.String), &h.rec) == nil && !h.rec.Checked.IsZero()
	h.stale = h.found && h.now.Sub(h.rec.Checked) > xmrHealthStaleAfter
	return h, nil
}

// healthStamp is t for the admin pages: "2026-10-19 09:00 UTC (3 minutes ago)".
func healthStamp(t, now time.Time) string {
	return t.UTC().Format(scannerStamp) + " (" + ageWords(now.Sub(t)) + ")"
}

// ageWords is how long ago d was, in whole minutes, hours or days.
func ageWords(d time.Duration) string {
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one + " ago"
		}
		return strconv.Itoa(n) + " " + one + "s ago"
	}
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return unit(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return unit(int(d/time.Hour), "hour")
	}
	return unit(int(d/(24*time.Hour)), "day")
}

// XMREscrowHealthView is Admin → Payments' Monero escrow watcher health (A-553, STAFF66-3), while ESCROW_XMR is on.
type XMREscrowHealthView struct {
	Recorded        bool   // a pass has been recorded on this database
	Checked, Good   string // healthStamp of the latest pass and of the latest good pass ("" for none)
	Stale           bool   // no pass recorded for xmrHealthStaleAfter
	Cause           string // why the latest pass was not good; "" when it was
	CreationStopped bool   // ESCROW_XMR_SEED_KEY is not the pinned seed key
}

// Attention: the overview flags the Payments section.
func (v *XMREscrowHealthView) Attention() bool {
	return v != nil && (v.CreationStopped || v.Stale || v.Cause != "")
}

func (h xmrHealthStatus) view() *XMREscrowHealthView {
	v := &XMREscrowHealthView{Recorded: h.found}
	if !h.found {
		return v
	}
	v.Checked, v.Stale, v.Cause, v.CreationStopped = healthStamp(h.rec.Checked, h.now), h.stale, h.rec.Cause, h.rec.CreationStopped
	if !h.rec.Good.IsZero() {
		v.Good = healthStamp(h.rec.Good, h.now)
	}
	return v
}

// xmrEscrowHealthLoader shows the watcher's health on /admin while ESCROW_XMR is on.
func xmrEscrowHealthLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	d.XMREscrowHealth = nil
	if a.escrowXMR == nil || a.db == nil || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	h, err := a.xmrHealth(ctx)
	if err != nil {
		return err
	}
	d.XMREscrowHealth = h.view()
	return nil
}

// nodeHealth is the dependency table's Health lines for the daemon (daemon true) or the wallet-rpc, and whether they
// need attention: reachable, synced (the daemon's height against its target), the last successful check and the last
// failure's class, from the watcher's record.
func (h xmrHealthStatus) nodeHealth(daemon bool) ([]string, bool) {
	if !h.found {
		return []string{"No Monero escrow watcher pass recorded yet"}, false
	}
	s := h.rec.Wallet
	if daemon {
		s = h.rec.Daemon
	}
	var lines []string
	failing := s.ErrorAt.After(s.OK)
	switch {
	case s.OK.IsZero() && s.ErrorAt.IsZero():
		lines = append(lines, "Not checked yet")
	case failing:
		lines = append(lines, "Not reachable or failing: "+s.Error+", "+healthStamp(s.ErrorAt, h.now))
	default:
		lines = append(lines, "Reachable")
	}
	bad := failing
	if daemon && !s.InfoAt.IsZero() {
		height := strconv.FormatInt(s.Height, 10)
		switch {
		case !s.Synchronized:
			lines, bad = append(lines, "Not synced: the daemon reports it is not synchronized (height "+height+")"), true
		case s.Target > s.Height:
			lines, bad = append(lines, "Not synced: height "+height+" of target "+strconv.FormatInt(s.Target, 10)), true
		case s.Sync != "":
			lines, bad = append(lines, "Not synced: "+s.Sync+" (height "+height+")"), true
		default:
			lines = append(lines, "Synced at height "+height)
		}
	}
	if s.OK.IsZero() {
		lines = append(lines, "No successful check recorded")
	} else {
		lines = append(lines, "Last successful check: "+healthStamp(s.OK, h.now))
	}
	if s.Error != "" && !failing {
		lines = append(lines, "Last error: "+s.Error+", "+healthStamp(s.ErrorAt, h.now))
	}
	if h.stale {
		lines, bad = append(lines, "Watcher record stale: no pass since "+healthStamp(h.rec.Checked, h.now)), true
	}
	return lines, bad
}
