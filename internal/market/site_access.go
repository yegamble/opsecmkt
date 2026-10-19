package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

const (
	settingSiteAccess         = "site_access"
	settingRegistrations      = "registrations"
	siteAccessLock            = 782501
	registrationClosedMessage = "New registrations are currently closed. Existing accounts can still sign in."
)

// SiteAccessView is the current database policy, shared by route guards and forms.
type SiteAccessView struct {
	MembersOnly      bool
	RegistrationOpen bool
}

type initialSiteAccess struct{ mode, registrations string }
type siteAccessContextKey struct{}

func init() {
	registerAction("/admin/access", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: saveSiteAccess})
}

func initialSiteAccessFromEnv(getenv func(string) string) (initialSiteAccess, error) {
	p := initialSiteAccess{getenv("INITIAL_SITE_ACCESS"), getenv("INITIAL_REGISTRATIONS")}
	if p.mode == "" {
		p.mode = "public"
	}
	if p.registrations == "" {
		p.registrations = "open"
	}
	if p.mode != "public" && p.mode != "members" {
		return p, errors.New("INITIAL_SITE_ACCESS must be public or members")
	}
	if p.registrations != "open" && p.registrations != "closed" {
		return p, errors.New("INITIAL_REGISTRATIONS must be open or closed")
	}
	return p, nil
}

// Initial values fill missing rows only. An administrator's saved choices survive restarts.
func (a *App) initializeSiteAccess(ctx context.Context, p initialSiteAccess) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, siteAccessLock); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES ('site_access',$1),('registrations',$2) ON CONFLICT(key) DO NOTHING`, p.mode, p.registrations); err != nil {
		return err
	}
	return tx.Commit()
}

// Missing or malformed rows fail closed after initialization rather than reopening access.
func siteAccessFromSettings(settings map[string]string) *SiteAccessView {
	return &SiteAccessView{MembersOnly: settings[settingSiteAccess] != "public", RegistrationOpen: settings[settingRegistrations] == "open"}
}

func (a *App) readSiteAccess(ctx context.Context) (*SiteAccessView, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN ('site_access','registrations')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	settings := make(map[string]string, 2)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		settings[key] = value
	}
	return siteAccessFromSettings(settings), rows.Err()
}

func authenticationPage(page string) bool {
	switch page {
	case "login", "register", "setup", "challenge":
		return true
	}
	return false
}

func accessRequest(r *http.Request, p *SiteAccessView) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), siteAccessContextKey{}, p))
}

func requestSiteAccess(r *http.Request) *SiteAccessView {
	p, _ := r.Context().Value(siteAccessContextKey{}).(*SiteAccessView)
	return p
}

func (a *App) redirectForSiteSignIn(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if next := a.signInNext(r.URL.RequestURI()); next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// The shared lock remains held through account creation's commit. Closing registrations waits for
// already-authorized creations; later creations see the committed closed policy and are refused.
func registrationAllowedTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, siteAccessLock); err != nil {
		return err
	}
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='registrations'`).Scan(&value)
	if err == sql.ErrNoRows || err == nil && value != "open" {
		return fail(http.StatusForbidden, registrationClosedMessage)
	}
	return err
}

func saveSiteAccess(c *actionCtx) (actionResult, error) {
	mode, registrations := c.Form.Get(settingSiteAccess), c.Form.Get(settingRegistrations)
	if mode != "public" && mode != "members" || registrations != "open" && registrations != "closed" {
		return actionResult{}, fail(400, "Choose public or members-only access and open or closed registrations.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		// Do not take the global account-creation lock after confirmedTx's actor lock.
		if _, err := c.Tx.ExecContext(c.Ctx(), `SELECT pg_advisory_xact_lock($1)`, siteAccessLock); err != nil {
			return actionResult{}, err
		}
		previous, err := readSiteAccessTx(c)
		if err != nil {
			return actionResult{}, err
		}
		if _, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO settings(key,value) VALUES ('site_access',$1),('registrations',$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, mode, registrations); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/security", Notice: "access", Audit: fmt.Sprintf("Changed site access from %s to %s; registrations from %s to %s", previous[settingSiteAccess], mode, previous[settingRegistrations], registrations)}, nil
	})
}

func readSiteAccessTx(c *actionCtx) (map[string]string, error) {
	previous := map[string]string{settingSiteAccess: "members", settingRegistrations: "closed"}
	rows, err := c.Tx.QueryContext(c.Ctx(), `SELECT key,value FROM settings WHERE key IN ('site_access','registrations') ORDER BY key FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		if key == settingSiteAccess && value != "public" && value != "members" || key == settingRegistrations && value != "open" && value != "closed" {
			value = "invalid"
		}
		previous[key] = value
	}
	return previous, rows.Err()
}
