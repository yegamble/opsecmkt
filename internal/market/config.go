package market

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"log"
	"path"
	"path/filepath"
	"strings"
)

// startupConfig is what New checks in the environment before it opens the database (D8, I8). Every refusal
// names the variable and how to fix it, never its value.
type startupConfig struct {
	appSecretKey  string // APP_SECRET_KEY: the root of every derived key when set
	setupToken    string // SETUP_TOKEN: authorizes /setup until installed; the legacy key root without APP_SECRET_KEY
	secure        bool   // COOKIE_SECURE (unset = true)
	adminSocket   string // ADMIN_SOCKET: separate private administrator Unix listener; blank preserves one-listener installs
	retention     retentionConfig
	initialAccess initialSiteAccess
	notes         []string // startup log lines (warnings and the legacy key-root notice); never a secret
}

// keyRoot is the secret the CSRF/CAPTCHA HMAC key and the sealing keys derive from: APP_SECRET_KEY when set,
// else SETUP_TOKEN (legacy, as before the split, so existing installs keep their keys).
func (c startupConfig) keyRoot() (root string, legacy bool) {
	if c.appSecretKey != "" {
		return c.appSecretKey, false
	}
	return c.setupToken, true
}

// KeyRootSource names, without its value, the secret the app's keys derive from under getenv: "app_secret_key"
// or "setup_token" (legacy). -status prints it so the installer removes SETUP_TOKEN only from an install whose
// running app no longer derives anything from it.
func KeyRootSource(getenv func(string) string) string {
	if _, legacy := (startupConfig{appSecretKey: getenv("APP_SECRET_KEY"), setupToken: getenv("SETUP_TOKEN")}).keyRoot(); legacy {
		return "setup_token"
	}
	return "app_secret_key"
}

// useKeyRoot derives the CSRF/CAPTCHA HMAC key and the CAPTCHA challenge-id key from root and keeps root for
// sealing (crypto.go). The "csrf:" derivation string is the one used before APP_SECRET_KEY existed. The CSRF token
// is HMAC(a.key, cookie value) of a client-chosen 64-character value, so every new input HMAC'd with a.key must
// carry a distinct prefix, as captchaSeed's "captcha:" does (SEC-3).
func (a *App) useKeyRoot(root string) {
	a.keyRoot = root
	sum := sha256.Sum256([]byte("csrf:" + root))
	a.key = sum[:]
	id := sha256.Sum256([]byte("captcha-id:" + root))
	a.captchaIDKey = id[:]
}

// dbPlaceholderMarkers are the example-value words refused in DATABASE_URL (compared case-insensitively;
// "REPLACE" alone also in upper case). Plain words such as "replacement" in a host name are allowed.
var dbPlaceholderMarkers = []string{"replace_me", "replace-me", "replace_with", "changeme", "change_me", "change-me"}

// loadStartupConfig checks the environment (getenv) for mode, which New has already validated. It refuses
// weak or missing secrets, a COOKIE_SECURE other than true/false, a placeholder DATABASE_URL and an APP_MODE
// that disagrees with the Compose overlay in COMPOSE_FILE.
func loadStartupConfig(getenv func(string) string, mode string) (startupConfig, error) {
	c := startupConfig{appSecretKey: getenv("APP_SECRET_KEY"), setupToken: getenv("SETUP_TOKEN")}
	if err := CheckDeploymentProfile(getenv); err != nil {
		return c, err
	}
	if err := refuseBitcoinEnv(getenv); err != nil {
		return c, err
	}
	if err := refuseObsoletePooledEnv(getenv); err != nil {
		return c, err
	}
	if c.appSecretKey != "" {
		if err := checkSecret("APP_SECRET_KEY", c.appSecretKey); err != nil {
			return c, err
		}
	}
	if c.setupToken != "" {
		if err := checkSecret("SETUP_TOKEN", c.setupToken); err != nil {
			return c, err
		}
	}
	if c.appSecretKey == "" && c.setupToken == "" {
		return c, errors.New("APP_SECRET_KEY is not set: it derives the CSRF/CAPTCHA key and the key sealing TOTP secrets. Generate one with `openssl rand -hex 32` and put it in .env (an install that predates it keeps working with SETUP_TOKEN alone; see UPGRADING.md)")
	}
	switch getenv("COOKIE_SECURE") {
	case "", "true":
		c.secure = true
	case "false":
	default:
		return c, errors.New(`COOKIE_SECURE must be "true" or "false" (or unset, which means true); fix it in .env`)
	}
	dsn := getenv("DATABASE_URL")
	if dsn == "" {
		return c, errors.New("DATABASE_URL required; use -preview for read-only sample UI")
	}
	if dbPlaceholder(dsn) {
		return c, errors.New("DATABASE_URL still contains a placeholder (REPLACE_ME, CHANGEME or similar, as in .env.example); set the real database password and host in .env (scripts/install.sh generates them)")
	}
	if err := checkComposeOverlay(mode, getenv("COMPOSE_FILE")); err != nil {
		return c, err
	}
	var err error
	if c.adminSocket, err = adminSocketConfig(getenv, mode); err != nil {
		return c, err
	}
	if c.retention, err = retentionConfigFromEnv(getenv); err != nil {
		return c, err
	}
	if c.initialAccess, err = initialSiteAccessFromEnv(getenv); err != nil {
		return c, err
	}
	if mode == "tor" && c.secure {
		c.notes = append(c.notes, "warning: APP_MODE=tor with COOKIE_SECURE=true: the onion is served over plain HTTP, so only browsers that treat .onion addresses as secure keep the Secure session cookie. The installer writes COOKIE_SECURE=false for Tor; set that in .env unless you mean it")
	}
	if _, legacy := c.keyRoot(); legacy {
		c.notes = append(c.notes, "keys: derived from SETUP_TOKEN (legacy); add APP_SECRET_KEY to .env with the same value to keep them, as UPGRADING.md describes")
	}
	return c, nil
}

func retentionConfigFromEnv(getenv func(string) string) (retentionConfig, error) {
	var c retentionConfig
	switch getenv("RETENTION_ENABLED") {
	case "", "off", "false":
	case "on", "true":
		c.Enabled = true
	default:
		return c, errors.New("RETENTION_ENABLED must be on or off")
	}
	c.StateDir = getenv("RETENTION_STATE_DIR")
	if c.Enabled && (!filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir) {
		return c, errors.New("RETENTION_STATE_DIR must be an absolute clean path to persistent external retention state")
	}
	return c, nil
}

// refuseObsoletePooledEnv prevents a release with no pooled payment runtime from silently ignoring an old pooled
// wallet configuration. These variables are exclusive to the removed pooled providers. Escrow credentials and
// shared chain, confirmation, freshness and watcher settings remain valid and are intentionally not listed here.
func refuseObsoletePooledEnv(getenv func(string) string) error {
	var configured []string
	for _, name := range []string{"BITCOIN_RPC_URL", "BITCOIN_WALLET", "MONERO_WALLET_RPC_URL", "MONERO_RPC_URL"} {
		if strings.TrimSpace(getenv(name)) != "" {
			configured = append(configured, name)
		}
	}
	if len(configured) == 0 {
		return nil
	}
	return errors.New("obsolete pooled payment configuration is set (" + strings.Join(configured, ", ") + "); remove these variables from .env. This release accepts escrow or paused intake only, and no database changes were made")
}

func dbPlaceholder(dsn string) bool {
	if strings.Contains(dsn, "REPLACE") {
		return true
	}
	lower := strings.ToLower(dsn)
	for _, m := range dbPlaceholderMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// checkComposeOverlay refuses an APP_MODE that disagrees with the exposure overlay Compose was started with:
// tor needs compose.tor.yaml, clearnet compose.clearnet.yaml, and never both. COMPOSE_FILE is passed through
// by compose.yaml; blank (plain -f flags, tests, the bare binary) skips the check. It is split on both of
// Compose's default path separators (":" and ";"): COMPOSE_PATH_SEPARATOR does not reach the container, and
// only file names matter here, so a split Windows drive letter is harmless.
func checkComposeOverlay(mode, composeFile string) error {
	if strings.TrimSpace(composeFile) == "" {
		return nil
	}
	has := composeFiles(composeFile)
	const fix = "; APP_MODE and the Compose overlay must agree (scripts/install.sh writes both; see docs/operator-guide.md)"
	if has["compose.clearnet.yaml"] && has["compose.tor.yaml"] {
		return errors.New("COMPOSE_FILE lists both compose.clearnet.yaml and compose.tor.yaml; never run both overlays together" + fix)
	}
	if want := "compose." + mode + ".yaml"; !has[want] {
		return errors.New("APP_MODE=" + mode + " needs " + want + " in COMPOSE_FILE" + fix)
	}
	return nil
}

// composeFiles is the set of file names (base names) in COMPOSE_FILE, split as checkComposeOverlay describes.
func composeFiles(composeFile string) map[string]bool {
	has := map[string]bool{}
	for _, f := range strings.FieldsFunc(composeFile, func(r rune) bool { return r == ':' || r == ';' }) {
		has[path.Base(strings.ReplaceAll(strings.TrimSpace(f), `\`, "/"))] = true
	}
	return has
}

// errSetupTokenRequired stops a server that is not installed yet and has no SETUP_TOKEN: nobody could
// complete /setup.
var errSetupTokenRequired = errors.New("SETUP_TOKEN is not set and installation is not complete, so /setup cannot be used. Run `./scripts/install.sh setup-token --new` to issue one (or put the output of `openssl rand -hex 32` in .env as SETUP_TOKEN) and start again")

// checkInstallState applies the SETUP_TOKEN rules that need the database: required until installed, and
// only a hint (it grants nothing) afterwards.
func (a *App) checkInstallState(ctx context.Context, legacy bool) error {
	installed, err := isInstalled(ctx, a.db)
	if err != nil {
		return errors.New("database status query failed: " + connectErrorClass(err))
	}
	switch {
	case !installed && a.setupToken == "":
		return errSetupTokenRequired
	case installed && a.setupToken != "" && legacy:
		log.Printf("hint: SETUP_TOKEN is set but installation is complete; it no longer opens /setup. It still derives this server's keys (no APP_SECRET_KEY): copy its value to APP_SECRET_KEY before removing it, as UPGRADING.md describes")
	case installed && a.setupToken != "":
		log.Printf("hint: SETUP_TOKEN is set but installation is complete; it grants nothing now and can be removed from .env")
	}
	return nil
}

// isInstalled reports whether first-run setup has completed (the schema must be migrated).
func isInstalled(ctx context.Context, db *sql.DB) (bool, error) {
	var installed bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed)
	return installed, err
}

// Status reports, read-only, whether setup has completed and the highest applied migration (0 when none).
// Errors carry a class only, never DATABASE_URL or driver text (for -status).
func Status(ctx context.Context, dsn string) (installed bool, schema int, err error) {
	db, err := OpenDB(dsn)
	if err != nil {
		return false, 0, err
	}
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		return false, 0, errors.New("database connection failed: " + connectErrorClass(err))
	}
	// A database that was never migrated has neither table: not installed, schema 0.
	var settings, migrations bool
	err = db.QueryRowContext(ctx, "SELECT to_regclass('settings') IS NOT NULL, to_regclass('schema_migrations') IS NOT NULL").Scan(&settings, &migrations)
	if err == nil && settings {
		installed, err = isInstalled(ctx, db)
	}
	if err == nil && migrations {
		err = db.QueryRowContext(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&schema)
	}
	if err != nil {
		return false, 0, errors.New("database status query failed: " + connectErrorClass(err))
	}
	return installed, schema, nil
}

// refuseBitcoinEnv fails before database migrations or node connections. Passive
// image/prune/confirmation/freshness settings from an old XMR-only install are
// harmless; credentials, chain selection, signer configuration and profiles are not.
func refuseBitcoinEnv(getenv func(string) string) error {
	var configured []string
	for _, name := range []string{"ESCROW_BTC", "BITCOIN_RPC_URL", "BITCOIN_WALLET", "BITCOIN_CHAIN", "BITCOIN_RPC_PASSWORD", "BITCOIN_RPC_AUTH",
		"ESCROW_BTC_RPC_URL", "ESCROW_BTC_ARBITER_WALLET", "ESCROW_BTC_WATCH_WALLET", "ESCROW_BTC_FEE_BPS", "ESCROW_BTC_FEE_ADDRESS", "ESCROW_BTC_FEE_RATE",
		"ESCROW_SIGNER_SOCKET", "ESCROW_SIGNER_RPC_URL", "ESCROW_SIGNER_SEED_FILE"} {
		value := strings.TrimSpace(getenv(name))
		if value == "" || name == "ESCROW_BTC" && (value == "off" || value == "false") {
			continue
		}
		configured = append(configured, name)
	}
	for _, profile := range strings.FieldsFunc(getenv("COMPOSE_PROFILES"), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if profile == "bitcoin" || profile == "escrow-signer" {
			configured = append(configured, "COMPOSE_PROFILES ("+profile+")")
		}
	}
	if len(configured) > 0 {
		return errors.New("Bitcoin is removed: this market accepts Monero (XMR) only; remove obsolete Bitcoin configuration (" + strings.Join(configured, ", ") + ") before restarting. No database changes were made")
	}
	return nil
}
