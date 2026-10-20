package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const settingXMPPConfig = "xmpp_config"

// The outgoing credential is sealed separately from ordinary settings. It must
// never be loaded into PageData.Settings or a public User value.
type storedXMPPConfig struct {
	Enabled       bool `json:"enabled"`
	AllowContents bool `json:"allow_contents"`
	// A permission toggle invalidates queued content, including when permission
	// is later granted again before a previously queued message is processed.
	ContentPolicyVersion int64  `json:"content_policy_version"`
	Sender               string `json:"sender"`
	Host                 string `json:"host"`
	Port                 int    `json:"port"`
	TLSMode              string `json:"tls_mode"`
	PasswordSealed       string `json:"password_sealed"`
}

type XMPPAccountView struct {
	Destination         string
	Enabled             bool
	IncludeContents     bool
	AllowContents       bool
	KeyUsable           bool
	ContentsUnavailable string
}

type XMPPAdminView struct {
	Enabled               bool
	AllowContents         bool
	Sender, Host, TLSMode string
	Port                  int
	PasswordConfigured    bool
	Error                 string
}

func init() {
	registerAction("/account/xmpp", actionSpec{Roles: []string{"buyer", "vendor", "moderator", "admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: saveAccountXMPP})
	registerAction("/admin/xmpp", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: saveAdminXMPP})
	registerLoader("account", xmppAccountLoader)
	registerLoader("admin", xmppAdminLoader)
}

func xmppAccountLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	d.XMPPAccount = &XMPPAccountView{}
	if err := a.db.QueryRowContext(ctx, "SELECT xmpp,xmpp_enabled,xmpp_include_contents FROM users WHERE id=$1", d.User.ID).Scan(&d.XMPPAccount.Destination, &d.XMPPAccount.Enabled, &d.XMPPAccount.IncludeContents); err != nil {
		return err
	}
	cfg, err := readStoredXMPPConfig(ctx, a.db)
	if err == nil {
		d.XMPPAccount.AllowContents = cfg.AllowContents
	}
	d.XMPPAccount.KeyUsable, err = userCanReceiveEncrypted(ctx, a.db, d.User.ID)
	if err != nil {
		return err
	}
	switch {
	case !d.XMPPAccount.AllowContents:
		d.XMPPAccount.ContentsUnavailable = "The administrator has not allowed message contents in XMPP. You can still receive notification summaries."
	case !d.XMPPAccount.KeyUsable:
		d.XMPPAccount.ContentsUnavailable = "Add a usable PGP encryption public key under Public profile before choosing message contents. PGP sign-in is not required."
	}
	return nil
}

func xmppAdminLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" || d.AdminSection != "site" {
		return nil
	}
	cfg, err := readStoredXMPPConfig(ctx, a.db)
	d.XMPPAdmin = &XMPPAdminView{Enabled: cfg.Enabled, AllowContents: cfg.AllowContents, Sender: cfg.Sender, Host: cfg.Host, Port: cfg.Port, TLSMode: cfg.TLSMode, PasswordConfigured: cfg.PasswordSealed != ""}
	if err != nil {
		d.XMPPAdmin.Error = "The outgoing XMPP configuration cannot be read. Save the settings and a new sender password to replace it."
		return nil
	}
	if cfg.PasswordSealed != "" {
		if _, err := a.open("xmpp-password", cfg.PasswordSealed); err != nil {
			d.XMPPAdmin.Error = "The sender password cannot be opened. Enter it again and save these settings."
		}
	}
	return nil
}

func readStoredXMPPConfig(ctx context.Context, q rowQuerier) (storedXMPPConfig, error) {
	cfg := storedXMPPConfig{Port: 5222, TLSMode: "starttls"}
	var value string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", settingXMPPConfig).Scan(&value)
	if err == sql.ErrNoRows {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if len(value) > 8192 || json.Unmarshal([]byte(value), &cfg) != nil {
		return storedXMPPConfig{}, errors.New("outgoing XMPP configuration is invalid")
	}
	return cfg, nil
}

func (a *App) loadXMPPConfig(ctx context.Context, q rowQuerier) (xmppConfig, error) {
	stored, err := readStoredXMPPConfig(ctx, q)
	if err != nil {
		return xmppConfig{}, err
	}
	cfg := xmppConfig{Enabled: stored.Enabled, AllowContents: stored.AllowContents, ContentPolicyVersion: stored.ContentPolicyVersion, Sender: stored.Sender, Host: stored.Host, Port: stored.Port, TLSMode: stored.TLSMode}
	if !cfg.Enabled {
		return cfg, nil
	}
	if !validStoredXMPPConfig(stored) || stored.PasswordSealed == "" {
		return xmppConfig{}, errors.New("outgoing XMPP configuration is invalid")
	}
	cfg.Password, err = a.open("xmpp-password", stored.PasswordSealed)
	if err != nil || cfg.Password == "" || len(cfg.Password) > 1024 || strings.ContainsRune(cfg.Password, 0) || !utf8.ValidString(cfg.Password) {
		return xmppConfig{}, errors.New("outgoing XMPP credential is unavailable")
	}
	return cfg, nil
}

func validStoredXMPPConfig(cfg storedXMPPConfig) bool {
	return validateXMPPJID(cfg.Sender) && validXMPPHost(cfg.Host) && cfg.Port > 0 && cfg.Port <= 65535 && (cfg.TLSMode == "starttls" || cfg.TLSMode == "direct")
}

func xmppFormEnabled(value string) (bool, error) {
	if value != "" && value != "1" {
		return false, fail(400, "Choose whether XMPP notifications are enabled.")
	}
	return value == "1", nil
}

func saveAccountXMPP(c *actionCtx) (actionResult, error) {
	want := strings.TrimSpace(c.Form.Get("xmpp"))
	enabled, err := xmppFormEnabled(c.Form.Get("xmpp_enabled"))
	if err != nil {
		return actionResult{}, err
	}
	if (enabled || want != "") && !validateXMPPJID(want) {
		return actionResult{}, fail(400, "Use a bare XMPP address such as account@example.org, without a resource or control characters.")
	}
	includeContents, err := xmppFormEnabled(c.Form.Get("xmpp_include_contents"))
	if err != nil {
		return actionResult{}, err
	}
	if includeContents && !enabled {
		return actionResult{}, fail(400, "Message contents require XMPP notifications. To turn off XMPP, clear both choices.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		if includeContents {
			// confirmedTx already holds the account lock, including its saved key.
			// Locking policy alongside the eligibility check serializes revocation.
			var raw string
			err := c.Tx.QueryRowContext(c.Ctx(), "SELECT value FROM settings WHERE key=$1 FOR SHARE", settingXMPPConfig).Scan(&raw)
			var cfg storedXMPPConfig
			if err != nil && err != sql.ErrNoRows {
				return actionResult{}, err
			}
			if err == sql.ErrNoRows || len(raw) > 8192 || json.Unmarshal([]byte(raw), &cfg) != nil || !cfg.AllowContents {
				return actionResult{}, fail(403, "The administrator has not allowed message contents in XMPP.")
			}
			usable, err := userCanReceiveEncrypted(c.Ctx(), c.Tx, c.User.ID)
			if err != nil {
				return actionResult{}, err
			}
			if !usable {
				return actionResult{}, fail(400, "Add a usable PGP encryption public key before choosing message contents. PGP sign-in is not required.")
			}
		}
		result, err := c.Tx.ExecContext(c.Ctx(), `UPDATE users SET xmpp=$2,xmpp_enabled=$3,xmpp_include_contents=$4,xmpp_version=xmpp_version+1
			WHERE id=$1 AND (xmpp<>$2 OR xmpp_enabled<>$3 OR xmpp_include_contents<>$4)`, c.User.ID, want, enabled, includeContents)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return actionResult{}, fail(409, "Your XMPP preferences are already saved.")
		}
		return actionResult{Redirect: "/account#xmpp-notifications", Audit: "Changed XMPP notification preferences"}, nil
	})
}

func saveAdminXMPP(c *actionCtx) (actionResult, error) {
	enabled, err := xmppFormEnabled(c.Form.Get("xmpp_enabled"))
	if err != nil {
		return actionResult{}, err
	}
	allowContents, err := xmppFormEnabled(c.Form.Get("xmpp_allow_contents"))
	if err != nil {
		return actionResult{}, err
	}
	want := storedXMPPConfig{Enabled: enabled, AllowContents: allowContents, Sender: strings.TrimSpace(c.Form.Get("xmpp_sender")), Host: strings.TrimSpace(c.Form.Get("xmpp_host")), TLSMode: c.Form.Get("xmpp_tls_mode")}
	want.Port, err = strconv.Atoi(c.Form.Get("xmpp_port"))
	if err != nil || !validStoredXMPPConfig(want) {
		return actionResult{}, fail(400, "Use a valid bare sender XMPP address, server hostname, port from 1 to 65535, and STARTTLS or direct TLS.")
	}
	password := c.Form.Get("xmpp_password")
	if len(password) > 1024 || strings.ContainsRune(password, 0) || !utf8.ValidString(password) {
		return actionResult{}, fail(400, "The sender password must contain at most 1024 UTF-8 bytes and no NUL character.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		if _, err := c.Tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'{}') ON CONFLICT(key) DO NOTHING", settingXMPPConfig); err != nil {
			return actionResult{}, err
		}
		var raw string
		if err := c.Tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingXMPPConfig).Scan(&raw); err != nil {
			return actionResult{}, err
		}
		var old storedXMPPConfig
		if len(raw) > 8192 || json.Unmarshal([]byte(raw), &old) != nil {
			if password == "" {
				return actionResult{}, fail(400, "Enter the sender password to replace the unreadable XMPP configuration.")
			}
		}
		want.PasswordSealed = old.PasswordSealed
		want.ContentPolicyVersion = old.ContentPolicyVersion
		if old.AllowContents != want.AllowContents {
			want.ContentPolicyVersion++
		}
		if password != "" {
			var err error
			want.PasswordSealed, err = c.A.seal("xmpp-password", password)
			if err != nil {
				return actionResult{}, err
			}
		} else if want.Enabled {
			if opened, err := c.A.open("xmpp-password", want.PasswordSealed); err != nil || opened == "" {
				return actionResult{}, fail(400, "Enter the outgoing account's XMPP password before enabling delivery.")
			}
		}
		encoded, err := json.Marshal(want)
		if err != nil {
			return actionResult{}, err
		}
		if err := setSetting(c, settingXMPPConfig, string(encoded)); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/site#xmpp-outgoing", Audit: "Changed outgoing XMPP notification settings"}, nil
	})
}
