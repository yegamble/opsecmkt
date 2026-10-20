package market

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/moderation"
)

func signedModerationToken(t *testing.T, key ed25519.PrivateKey, token ModerationActionToken) []byte {
	t.Helper()
	raw, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	return signedModerationRaw(key, raw)
}

func signedModerationRaw(key ed25519.PrivateKey, raw []byte) []byte {
	sig := ed25519.Sign(key, append([]byte(moderationActionDomain), raw...))
	return append(append(append([]byte(`{"token":`), raw...), []byte(`,"signature":"`+hex.EncodeToString(sig)+`"}`)...), '\n')
}

func TestModerationOfflineTokenBoundaries(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	audience := strings.Repeat("a", 64)
	base := ModerationActionToken{Version: 1, Audience: audience, SuggestionID: strings.Repeat("b", 64), ContentHash: strings.Repeat("c", 64), Action: "hold", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Nonce: strings.Repeat("d", 64)}
	if _, err = VerifyModerationActionToken(signedModerationToken(t, key, base), public, audience, now); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*ModerationActionToken){
		"other installation":  func(v *ModerationActionToken) { v.Audience = strings.Repeat("e", 64) },
		"release money":       func(v *ModerationActionToken) { v.Action = "release" },
		"ban account":         func(v *ModerationActionToken) { v.Action = "ban" },
		"expired":             func(v *ModerationActionToken) { v.ExpiresAt = now.Format(time.RFC3339) },
		"unbounded lifetime":  func(v *ModerationActionToken) { v.ExpiresAt = now.Add(8 * 24 * time.Hour).Format(time.RFC3339) },
		"non UTC":             func(v *ModerationActionToken) { v.ExpiresAt = "2026-10-20T01:00:00+00:00" },
		"missing binding":     func(v *ModerationActionToken) { v.ContentHash = "" },
		"unsupported version": func(v *ModerationActionToken) { v.Version = 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			token := base
			mutate(&token)
			if _, err := VerifyModerationActionToken(signedModerationToken(t, key, token), public, audience, now); err == nil {
				t.Fatal("accepted invalid signed authority")
			}
		})
	}
	raw, _ := json.Marshal(base)
	for name, payload := range map[string][]byte{
		"duplicate token key": signedModerationRaw(key, bytes.Replace(raw, []byte(`"action":"hold"`), []byte(`"action":"accept","action":"hold"`), 1)),
		"missing field":       signedModerationRaw(key, bytes.Replace(raw, []byte(`"version":1,`), nil, 1)),
		"null field":          signedModerationRaw(key, bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":null`), 1)),
		"extra field":         signedModerationRaw(key, bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"execute":"refund"`), 1)),
		"tamper":              bytes.Replace(signedModerationToken(t, key, base), []byte(`"action":"hold"`), []byte(`"action":"accept"`), 1),
		"trailing object":     append(signedModerationToken(t, key, base), []byte(`{}`)...),
		"oversized":           bytes.Repeat([]byte(" "), moderationActionTokenLimit+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyModerationActionToken(payload, public, audience, now); err == nil {
				t.Fatal("accepted ambiguous or tampered token")
			}
		})
	}
}

func TestModerationOfflineSignerInteroperates(t *testing.T) {
	for _, command := range []string{"openssl", "python3"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skip(command + " unavailable")
		}
	}
	dir := t.TempDir()
	private := filepath.Join(dir, "offline.pem")
	if out, err := exec.Command("openssl", "genpkey", "-algorithm", "ED25519", "-out", private).CombinedOutput(); err != nil {
		t.Fatalf("OpenSSL key fixture: %v: %s", err, out)
	}
	der, err := exec.Command("openssl", "pkey", "-in", private, "-pubout", "-outform", "DER").Output()
	if err != nil || len(der) < 32 {
		t.Fatal("could not read test public key")
	}
	now := time.Now().UTC()
	audience := strings.Repeat("a", 64)
	token := ModerationActionToken{Version: 1, Audience: audience, SuggestionID: strings.Repeat("b", 64), ContentHash: strings.Repeat("c", 64), Action: "dismiss", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Nonce: strings.Repeat("d", 64)}
	raw, _ := json.Marshal(token)
	in, out := filepath.Join(dir, "token.json"), filepath.Join(dir, "signed.json")
	if err = os.WriteFile(in, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "scripts/sign-moderation-action.py", "--token", in, "--key", private, "--out", out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("documented offline signer failed: %v: %s", err, output)
	}
	envelope, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyModerationActionToken(envelope, ed25519.PublicKey(der[len(der)-32:]), audience, now); err != nil {
		t.Fatal("offline signer produced an invalid token:", err)
	}
	info, _ := os.Stat(out)
	if info.Mode().Perm() != 0600 {
		t.Fatal("signed token permissions are not private")
	}
	if output, err := exec.Command("python3", "scripts/sign-moderation-action.py", "--token", in, "--key", private, "--out", out).CombinedOutput(); err == nil || bytes.Contains(output, []byte("PRIVATE KEY")) {
		t.Fatal("signer overwrote an existing envelope or exposed key material")
	}
}

func TestModerationOfflineReviewNoLoginReplayOrStaleAcceptance(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/test-private/admin.sock"
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	keyFile := filepath.Join(t.TempDir(), "review.pub")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(public)), 0644); err != nil {
		t.Fatal(err)
	}
	audience := strings.Repeat("a", 64)
	t.Setenv("MODERATION_ACTION_PUBLIC_KEY", keyFile)
	t.Setenv("MODERATION_ACTION_AUDIENCE", audience)
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	vendor, _ := e.user("token_vendor", "vendor")
	product := e.product(vendor, "physical")
	// Moderation jobs can retain only metadata. The authorized source record is
	// the reference for restoring this unchanged-content replay fixture.
	var original string
	if err := e.DB.QueryRow("SELECT description FROM products WHERE id=$1", product).Scan(&original); err != nil {
		t.Fatal(err)
	}
	w := ModerationWorker{DB: e.DB, Engine: moderation.Engine{Config: moderation.DefaultConfig()}}
	if _, err := w.Pass(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var id, contentHash string
	var expiry time.Time
	if err := e.DB.QueryRow("SELECT id,content_hash,expires_at FROM moderation_suggestions WHERE target_id=$1", product).Scan(&id, &contentHash, &expiry); err != nil {
		t.Fatal(err)
	}
	token := ModerationActionToken{Version: 1, Audience: audience, SuggestionID: id, ContentHash: contentHash, Action: "accept", ExpiresAt: expiry.Add(-time.Second).UTC().Format(time.RFC3339), Nonce: strings.Repeat("d", 64)}
	anon := randomToken()
	path := "/admin/moderation/offline"
	// A closed, members-only storefront must not require an account session
	// before the independently authenticated private review token can be used.
	agExec(e, "UPDATE settings SET value='members' WHERE key='site_access'")
	e.check(ingressDo(e, true, "GET", path, anon, nil), 200)
	form := url.Values{"signed_token": {string(signedModerationToken(t, key, token))}}
	e.check(ingressDo(e, false, "POST", path, anon, form), 404)
	// Disabling capture must not permit accepting stale target content.
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE products SET description='content changed with capture disabled' WHERE id=$1", product)
	e.check(ingressDo(e, true, "POST", path, anon, form), 409)
	agExec(e, "UPDATE products SET description=$2 WHERE id=$1", product, original)
	wrec := ingressDo(e, true, "POST", path, anon, form)
	e.check(wrec, 303)
	e.check(ingressDo(e, true, "GET", wrec.Header().Get("Location"), anon, nil), 200)
	e.check(ingressDo(e, true, "POST", path, anon, form), 409)
	if n := agStr(e, "SELECT count(*)::text FROM moderation_action_receipts"); n != "1" {
		t.Fatal("token replay changed receipts:", n)
	}
	if n := agStr(e, "SELECT count(*)::text FROM moderation_reviews WHERE reviewer_user_id IS NULL AND action='accept'"); n != "1" {
		t.Fatal("offline review missing:", n)
	}
	if state := agStr(e, "SELECT archived::text FROM products WHERE id=$1", product); state != "false" {
		t.Fatal("proposal acceptance changed target state:", state)
	}
}
