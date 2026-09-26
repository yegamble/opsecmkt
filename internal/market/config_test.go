package market

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Installer redesign S1 (D3, D8; invariants I2-I4, I6, I8): APP_SECRET_KEY is the key root for the CSRF/CAPTCHA
// HMAC key and for sealing, SETUP_TOKEN only authorizes /setup, and startup refuses unusable configuration.

const testAppSecretKey = "9c1e5a7f3b2d8046e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8"

// Golden values produced by the code before the key-root split (key = sha256("csrf:"+SETUP_TOKEN), sealing
// key = sha256(label+":"+SETUP_TOKEN)) with SETUP_TOKEN=testSetupToken. They must keep opening/verifying.
const (
	goldenSealedTOTP     = "xXXdZsBio7cruRxoWNXsnASl/zKawqx1vayYuPptf8+kbU0XGoVLm7r7OrQ"
	goldenSealedRecovery = "81i6F3wccfJZwNZpS+ITMOceUwbo/z97s/I9AxrmoD4LmWgFnavgzkN7zSswh6ocZds"
	goldenCSRFSession    = "golden-session-token"
	goldenCSRF           = "66ca189785d6dbc5ce467a112de79d1bb88491f74646ec210fa83b2548de6c3f"
)

// env builds a getenv for loadStartupConfig from a base of valid values plus overrides ("" = unset).
func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		"APP_SECRET_KEY": testAppSecretKey,
		"SETUP_TOKEN":    testSetupToken,
		"DATABASE_URL":   "postgres://opsecmkt:5b0c7e1d9a3f@db:5432/opsecmkt?sslmode=disable",
		"COOKIE_SECURE":  "",
		"COMPOSE_FILE":   "",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

// appFor derives an App's keys the way New does from the given environment.
func appFor(t *testing.T, getenv func(string) string) *App {
	t.Helper()
	cfg, err := loadStartupConfig(getenv, "clearnet")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{}
	root, _ := cfg.keyRoot()
	a.useKeyRoot(root)
	return a
}

// setStartupEnv points New at an unreachable database with otherwise valid settings, for checks that must
// fail (or log) before any connection is attempted.
func setStartupEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	get := env(map[string]string{"DATABASE_URL": "postgres://nobody:s3cr3t-db-pass-7f1a@127.0.0.1:1/none?sslmode=disable"})
	for _, k := range []string{"APP_SECRET_KEY", "SETUP_TOKEN", "DATABASE_URL", "COOKIE_SECURE", "COMPOSE_FILE"} {
		t.Setenv(k, get(k))
	}
	t.Setenv("APP_MODE", "")
	t.Setenv("COMPOSE_PATH_SEPARATOR", "")
	for k, v := range overrides {
		t.Setenv(k, v)
	}
}

// newFails runs New and returns its error, failing when New succeeds.
func newFails(t *testing.T) error {
	t.Helper()
	a, err := New(context.Background(), false)
	if err == nil {
		a.Close()
		t.Fatal("New succeeded")
	}
	return err
}

func TestKeyRootPrefersAppSecretKey(t *testing.T) {
	cfg, err := loadStartupConfig(env(nil), "clearnet")
	if err != nil {
		t.Fatal(err)
	}
	if root, legacy := cfg.keyRoot(); root != testAppSecretKey || legacy {
		t.Fatalf("keyRoot = %q, legacy=%v; want APP_SECRET_KEY", root, legacy)
	}
	for _, note := range cfg.notes {
		if strings.Contains(note, "legacy") {
			t.Fatalf("legacy note with APP_SECRET_KEY set: %q", note)
		}
	}
	both := appFor(t, env(nil))
	onlyKey := appFor(t, env(map[string]string{"SETUP_TOKEN": ""}))
	if both.csrf("s") != onlyKey.csrf("s") {
		t.Fatal("CSRF key depends on SETUP_TOKEN while APP_SECRET_KEY is set")
	}
	// The derivation strings are unchanged, now over APP_SECRET_KEY: HMAC key sha256("csrf:"+root), sealing
	// key sha256(label+":"+root).
	csrfKey := sha256.Sum256([]byte("csrf:" + testAppSecretKey))
	m := hmac.New(sha256.New, csrfKey[:])
	m.Write([]byte("s"))
	if both.csrf("s") != hex.EncodeToString(m.Sum(nil)) {
		t.Fatal(`CSRF key is not sha256("csrf:"+APP_SECRET_KEY)`)
	}
	sealKey := sha256.Sum256([]byte("totp:" + testAppSecretKey))
	block, _ := aes.NewCipher(sealKey[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	manual := base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte("manual"), []byte("totp")))
	if pt, err := both.open("totp", manual); err != nil || pt != "manual" {
		t.Fatalf(`sealing key is not sha256("totp:"+APP_SECRET_KEY): %q %v`, pt, err)
	}
	// I2: changing APP_SECRET_KEY changes both derived keys.
	other := appFor(t, env(map[string]string{"APP_SECRET_KEY": strings.Repeat("0123456789abcdef", 3) + "fedcba9876543210"}))
	if other.csrf("s") == both.csrf("s") {
		t.Fatal("CSRF key unchanged after APP_SECRET_KEY changed")
	}
	sealed, err := both.seal("totp", "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.open("totp", sealed); err == nil {
		t.Fatal("sealed value opened under another APP_SECRET_KEY")
	}
}

func TestLegacySetupTokenKeyRootUnchanged(t *testing.T) {
	cfg, err := loadStartupConfig(env(map[string]string{"APP_SECRET_KEY": ""}), "clearnet")
	if err != nil {
		t.Fatal(err)
	}
	if root, legacy := cfg.keyRoot(); root != testSetupToken || !legacy {
		t.Fatalf("keyRoot = %q, legacy=%v; want SETUP_TOKEN (legacy)", root, legacy)
	}
	legacyNotes := 0
	for _, note := range cfg.notes {
		if strings.Contains(note, "SETUP_TOKEN (legacy)") {
			legacyNotes++
		}
		if strings.Contains(note, testSetupToken) {
			t.Fatalf("note leaks SETUP_TOKEN: %q", note)
		}
	}
	if legacyNotes != 1 {
		t.Fatalf("want one legacy key-root note, got %q", cfg.notes)
	}
	// I3: only SETUP_TOKEN set, and the upgrade path (APP_SECRET_KEY = the old SETUP_TOKEN, a new or no
	// SETUP_TOKEN) both derive byte-identical keys to the pre-split code.
	for name, get := range map[string]func(string) string{
		"legacy SETUP_TOKEN only":           env(map[string]string{"APP_SECRET_KEY": ""}),
		"APP_SECRET_KEY = old SETUP_TOKEN":  env(map[string]string{"APP_SECRET_KEY": testSetupToken, "SETUP_TOKEN": ""}),
		"APP_SECRET_KEY = old, new SETUP_T": env(map[string]string{"APP_SECRET_KEY": testSetupToken}),
	} {
		t.Run(name, func(t *testing.T) {
			a := appFor(t, get)
			if got := a.csrf(goldenCSRFSession); got != goldenCSRF {
				t.Fatalf("CSRF token %s, want golden %s", got, goldenCSRF)
			}
			if pt, err := a.open("totp", goldenSealedTOTP); err != nil || pt != "JBSWY3DPEHPK3PXP" {
				t.Fatalf("golden TOTP secret: %q %v", pt, err)
			}
			if pt, err := a.open("recovery", goldenSealedRecovery); err != nil || pt != "golden-recovery-reveal" {
				t.Fatalf("golden recovery reveal: %q %v", pt, err)
			}
		})
	}
}

func TestSealedValueSurvivesSetupTokenChange(t *testing.T) {
	before := appFor(t, env(nil))
	sealed, err := before.seal("totp", "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"rotated": "d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3", "removed": ""} {
		t.Run(name, func(t *testing.T) {
			after := appFor(t, env(map[string]string{"SETUP_TOKEN": token}))
			if pt, err := after.open("totp", sealed); err != nil || pt != "JBSWY3DPEHPK3PXP" {
				t.Fatalf("open after SETUP_TOKEN %s: %q %v", name, pt, err)
			}
			if after.csrf("session") != before.csrf("session") {
				t.Fatalf("CSRF key changed after SETUP_TOKEN %s", name)
			}
			if after.captchaAnswer("id") != before.captchaAnswer("id") {
				t.Fatalf("CAPTCHA key changed after SETUP_TOKEN %s", name)
			}
		})
	}
}

func TestStartupRefusesBadCookieSecure(t *testing.T) {
	for _, v := range []string{"TRUE", "True", "1", "0", "yes", "no", " true", "false ", "off"} {
		t.Run(v, func(t *testing.T) {
			setStartupEnv(t, map[string]string{"COOKIE_SECURE": v})
			if err := newFails(t); !strings.Contains(err.Error(), `COOKIE_SECURE must be "true" or "false"`) {
				t.Fatalf("COOKIE_SECURE=%q: %v", v, err)
			}
		})
	}
	for v, want := range map[string]bool{"": true, "true": true, "false": false} {
		cfg, err := loadStartupConfig(env(map[string]string{"COOKIE_SECURE": v}), "clearnet")
		if err != nil || cfg.secure != want {
			t.Fatalf("COOKIE_SECURE=%q: secure=%v err=%v; want %v", v, cfg.secure, err, want)
		}
	}
}

func TestStartupRefusesDatabaseURLPlaceholder(t *testing.T) {
	for _, dsn := range []string{
		"postgres://opsecmkt:REPLACE_ME@db:5432/opsecmkt?sslmode=disable", // .env.example
		"postgres://opsecmkt:replace_me@db:5432/opsecmkt",
		"postgres://opsecmkt:CHANGEME@db:5432/opsecmkt",
		"postgres://opsecmkt:changeme@db:5432/opsecmkt",
		"postgres://opsecmkt:secret@REPLACE_WITH_HOST:5432/opsecmkt",
		"postgres://CHANGE_ME:pw@db:5432/opsecmkt",
	} {
		t.Run(dsn, func(t *testing.T) {
			setStartupEnv(t, map[string]string{"DATABASE_URL": dsn})
			err := newFails(t)
			if !strings.Contains(err.Error(), "DATABASE_URL still contains a placeholder") {
				t.Fatalf("%v", err)
			}
			u, _ := url.Parse(dsn)
			if strings.Contains(err.Error(), "@") || strings.Contains(err.Error(), "://") || strings.Contains(err.Error(), u.Host) {
				t.Fatalf("error repeats DATABASE_URL: %v", err)
			}
		})
	}
	// Real values that merely contain the letters are accepted.
	for _, dsn := range []string{
		"postgres://opsecmkt:9f8e7d6c5b4a@replacement-db.internal:5432/opsecmkt",
		"postgres://opsecmkt:9f8e7d6c5b4a@db:5432/opsecmkt?sslmode=disable",
	} {
		if _, err := loadStartupConfig(env(map[string]string{"DATABASE_URL": dsn}), "clearnet"); err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
	}
}

func TestStartupRefusesModeOverlayMismatch(t *testing.T) {
	const (
		clearnet = "compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml"
		tor      = "compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml"
		both     = "compose.yaml:compose.clearnet.yaml:compose.tor.yaml"
		neither  = "compose.yaml:compose.internal-db.yaml"
	)
	for _, c := range []struct {
		mode, files, want string
	}{
		{"tor", clearnet, "APP_MODE=tor needs compose.tor.yaml in COMPOSE_FILE"},
		{"tor", neither, "APP_MODE=tor needs compose.tor.yaml in COMPOSE_FILE"},
		{"clearnet", tor, "APP_MODE=clearnet needs compose.clearnet.yaml in COMPOSE_FILE"},
		{"", tor, "APP_MODE=clearnet needs compose.clearnet.yaml in COMPOSE_FILE"}, // blank APP_MODE is clearnet
		{"clearnet", neither, "APP_MODE=clearnet needs compose.clearnet.yaml in COMPOSE_FILE"},
		{"tor", both, "both compose.clearnet.yaml and compose.tor.yaml"},
		{"clearnet", both, "both compose.clearnet.yaml and compose.tor.yaml"},
	} {
		t.Run(c.mode+"|"+c.files, func(t *testing.T) {
			setStartupEnv(t, map[string]string{"APP_MODE": c.mode, "COMPOSE_FILE": c.files, "COOKIE_SECURE": "false"})
			if err := newFails(t); !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v; want %q", err, c.want)
			}
		})
	}
	for _, c := range []struct{ mode, files, sep string }{
		{"clearnet", clearnet, ""},
		{"tor", tor, ""},
		{"tor", tor + ":compose.external-egress.yaml", ""},
		{"tor", "/srv/opsecmkt/compose.yaml:/srv/opsecmkt/compose.tor.yaml", ""},
		{"tor", "compose.yaml;compose.tor.yaml", ";"},
		{"clearnet", "", ""}, // no COMPOSE_FILE (plain `-f` flags, tests, bare binary): not checked
		{"tor", "", ""},
	} {
		get := env(map[string]string{"COMPOSE_FILE": c.files, "COMPOSE_PATH_SEPARATOR": c.sep, "COOKIE_SECURE": "false"})
		if _, err := loadStartupConfig(get, c.mode); err != nil {
			t.Fatalf("%s %q: %v", c.mode, c.files, err)
		}
	}
}

func TestStartupWarnsTorWithSecureCookies(t *testing.T) {
	const warning = "APP_MODE=tor with COOKIE_SECURE=true"
	for _, c := range []struct {
		mode, secure string
		warn         bool
	}{
		{"tor", "true", true}, {"tor", "", true}, {"tor", "false", false}, {"clearnet", "true", false},
	} {
		cfg, err := loadStartupConfig(env(map[string]string{"COOKIE_SECURE": c.secure}), c.mode)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(cfg.notes, "\n"), warning); got != c.warn {
			t.Fatalf("%s COOKIE_SECURE=%q: warned=%v, notes %q", c.mode, c.secure, got, cfg.notes)
		}
	}
	// New logs it (a warning, not a refusal: startup goes on to the database, which is unreachable here).
	logs := captureLog(t)
	setStartupEnv(t, map[string]string{"APP_MODE": "tor", "COOKIE_SECURE": "true"})
	if err := newFails(t); !strings.Contains(err.Error(), "database connection failed") {
		t.Fatalf("want the connection failure after the warning, got %v", err)
	}
	if n := strings.Count(logs.String(), warning); n != 1 {
		t.Fatalf("warning logged %d times: %q", n, logs.String())
	}
	for _, secret := range []string{testAppSecretKey, testSetupToken, "s3cr3t-db-pass-7f1a"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("startup log leaks a secret: %q", logs.String())
		}
	}
}

func TestStartupRefusesWeakOrMissingSecrets(t *testing.T) {
	setStartupEnv(t, map[string]string{"APP_SECRET_KEY": "", "SETUP_TOKEN": ""})
	if err := newFails(t); !strings.Contains(err.Error(), "APP_SECRET_KEY") {
		t.Fatalf("neither secret set: %v", err)
	}
	for name, v := range map[string]string{"placeholder": "REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS", "short": "0123456789abcdef", "repeated": strings.Repeat("0123456789abcdef", 4)} {
		for _, key := range []string{"APP_SECRET_KEY", "SETUP_TOKEN"} {
			t.Run(key+" "+name, func(t *testing.T) {
				setStartupEnv(t, map[string]string{key: v})
				err := newFails(t)
				if !strings.HasPrefix(err.Error(), key+" ") || !strings.Contains(err.Error(), "openssl rand -hex 32") {
					t.Fatalf("%s=%s: %v", key, name, err)
				}
			})
		}
	}
	// Either one alone is enough to pass the secret checks.
	for _, get := range []func(string) string{env(map[string]string{"SETUP_TOKEN": ""}), env(map[string]string{"APP_SECRET_KEY": ""})} {
		if _, err := loadStartupConfig(get, "clearnet"); err != nil {
			t.Fatal(err)
		}
	}
}

// startSchema points New at a fresh schema with the given secrets (DB-backed).
func startSchema(t *testing.T, appSecretKey, setupToken string) {
	t.Helper()
	t.Setenv("DATABASE_URL", testSchemaDSN(t))
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("APP_MODE", "")
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("APP_SECRET_KEY", appSecretKey)
	t.Setenv("SETUP_TOKEN", setupToken)
}

func TestStartupRequiresSetupTokenUntilInstalled(t *testing.T) {
	startSchema(t, testAppSecretKey, "")
	err := newFails(t)
	if !strings.Contains(err.Error(), "./scripts/install.sh setup-token --new") || strings.Contains(err.Error(), testAppSecretKey) {
		t.Fatalf("empty SETUP_TOKEN before install: %v", err)
	}
	t.Setenv("SETUP_TOKEN", testSetupToken)
	e := &testEnv{t: t}
	e.restart()
	t.Cleanup(func() { e.A.Close() })
	e.setup()
	// Installed: SETUP_TOKEN may now be removed.
	t.Setenv("SETUP_TOKEN", "")
	logs := captureLog(t)
	e.restart()
	if strings.Contains(logs.String(), "SETUP_TOKEN is set") {
		t.Fatalf("hint logged without SETUP_TOKEN: %q", logs.String())
	}
	// Even if the installed marker disappeared, an empty SETUP_TOKEN never matches an empty form token.
	if _, err := e.DB.Exec("DELETE FROM settings WHERE key='installed'"); err != nil {
		t.Fatal(err)
	}
	before := agInt(e, "SELECT count(*) FROM users")
	w := e.do("POST", "/setup", randomToken(), url.Values{"token": {""}, "handle": {"second_admin"}, "password": {testPassword}, "site_name": {"Hijacked"}})
	e.check(w, 403)
	if agInt(e, "SELECT count(*) FROM users") != before {
		t.Fatal("setup with an empty token created a user")
	}
}

func TestSetupTokenIgnoredAfterInstall(t *testing.T) {
	startSchema(t, testAppSecretKey, testSetupToken)
	e := &testEnv{t: t}
	e.restart()
	t.Cleanup(func() { e.A.Close() })
	admin := e.setup()
	if n := agInt(e, "SELECT count(*) FROM users WHERE role='admin'"); n != 1 {
		t.Fatalf("%d admins after setup", n)
	}
	csrf := e.A.csrf(admin)
	sealed, err := e.A.seal("totp", "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	const rotated = "d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3"
	for _, token := range []string{rotated, testSetupToken} {
		t.Setenv("SETUP_TOKEN", token)
		logs := captureLog(t)
		e.restart()
		if n := strings.Count(logs.String(), "SETUP_TOKEN is set but installation is complete"); n != 1 {
			t.Fatalf("want one post-install hint, got %q", logs.String())
		}
		if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), testAppSecretKey) {
			t.Fatalf("log leaks a secret: %q", logs.String())
		}
		// I4: SETUP_TOKEN grants nothing once installed, whatever its value.
		for _, try := range []string{token, testSetupToken, rotated, ""} {
			w := e.do("POST", "/setup", randomToken(), url.Values{"token": {try}, "handle": {"late_admin"}, "password": {testPassword}, "site_name": {"Late"}})
			e.check(w, 403)
		}
		if n := agInt(e, "SELECT count(*) FROM users"); n != 1 {
			t.Fatalf("%d users after post-install setup attempts", n)
		}
		// I2: keys come from APP_SECRET_KEY, so sessions' CSRF tokens and sealed secrets survive.
		if e.A.csrf(admin) != csrf {
			t.Fatal("CSRF token changed with SETUP_TOKEN")
		}
		if pt, err := e.A.open("totp", sealed); err != nil || pt != "JBSWY3DPEHPK3PXP" {
			t.Fatalf("sealed value after SETUP_TOKEN change: %q %v", pt, err)
		}
	}
	// The CSRF token issued before either change still authorizes the session's POST.
	r := httptest.NewRequest("POST", "/logout", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: admin})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	e.check(w, 303)
}
