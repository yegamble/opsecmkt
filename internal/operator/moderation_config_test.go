package operator

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testModerationAudience = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func moderationReviewInitArgs(t *testing.T, dir, publicKey string) []string {
	t.Helper()
	return []string{"init", "--dir", dir, "--mode", "dev", "--exposure", "tor", "--app-image", testImage,
		"--tor-image", testImage, "--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t),
		"--moderation-action-public-key", publicKey, "--set", "MODERATION_ACTION_AUDIENCE=" + testModerationAudience}
}

func initModerationReview(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "moderation-install")
	publicKey := filepath.Join(t.TempDir(), "offline-admin.pub")
	if err := os.WriteFile(publicKey, []byte(strings.Repeat("ab", 32)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke(t, moderationReviewInitArgs(t, dir, publicKey)...); err != nil {
		t.Fatalf("init: %v (%s)", err, out)
	}
	return dir, publicKey
}

func TestModerationOfflineReviewImportsPublicPinWithoutInferenceMounts(t *testing.T) {
	dir, source := initModerationReview(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	pinPath := c.PublicFiles["MODERATION_ACTION_PUBLIC_KEY"]
	if pinPath == "" || c.Settings["MODERATION_ACTION_AUDIENCE"] != testModerationAudience {
		t.Fatal("offline review verifier configuration was not retained")
	}
	if c.SecretFiles["MODERATION_ACTION_PUBLIC_KEY"] != "" || c.SecretFiles["MODERATION_ACTION_PRIVATE_KEY"] != "" {
		t.Fatal("review verification created a signing-secret entry")
	}
	if err := os.WriteFile(source, []byte(strings.Repeat("cd", 32)), 0644); err != nil {
		t.Fatal(err)
	}
	pin, err := os.ReadFile(filepath.Join(dir, pinPath))
	if err != nil || !bytes.Equal(pin, []byte(strings.Repeat("ab", 32)+"\n")) {
		t.Fatal("source public-key replacement changed the installed pin")
	}
	compose, environment, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if environment["MODERATION_ACTION_PUBLIC_KEY"] != "/run/opsecmkt-secrets/"+filepath.Base(pinPath) ||
		environment["MODERATION_ACTION_AUDIENCE"] != testModerationAudience {
		t.Fatal("application verifier environment does not use the independently installed public pin")
	}
	for key := range environment {
		if strings.HasPrefix(key, "MODERATION_") && key != "MODERATION_ACTION_PUBLIC_KEY" && key != "MODERATION_ACTION_AUDIENCE" {
			t.Fatalf("application was given inference configuration %s", key)
		}
	}
	var decoded struct {
		Services map[string]struct {
			Volumes []any `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(compose, &decoded); err != nil {
		t.Fatal(err)
	}
	for name := range decoded.Services {
		if strings.Contains(name, "moderation") || strings.Contains(name, "inference") {
			t.Fatal("native app controller provisioned an inference or moderation worker")
		}
	}
	mounts, _ := json.Marshal(decoded.Services["app"].Volumes)
	if !strings.Contains(string(mounts), filepath.Join(dir, "secrets")+":/run/opsecmkt-secrets:ro") {
		t.Fatal("verification material is not mounted read-only")
	}
	for _, forbidden := range []string{"/models", "inference.sock", "/run/moderation"} {
		if strings.Contains(string(mounts), forbidden) {
			t.Fatalf("application received inference mount %s", forbidden)
		}
	}
}

func TestModerationOfflineReviewRejectsIncompleteOrUnprotectedConfiguration(t *testing.T) {
	for _, scenario := range []string{"key-only", "audience-only", "short-audience", "uppercase-audience", "public-ingress", "private-key", "private-pem", "set-secret"} {
		t.Run(scenario, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "invalid")
			key := filepath.Join(t.TempDir(), "key")
			contents := strings.Repeat("ab", 32)
			if scenario == "private-key" {
				contents = strings.Repeat("ab", 64) // Ed25519 private keys are 64 bytes, never a public import.
			} else if scenario == "private-pem" {
				contents = "-----BEGIN PRIVATE KEY-----\nsynthetic\n-----END PRIVATE KEY-----\n"
			}
			if err := os.WriteFile(key, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"init", "--dir", dir, "--app-image", testImage}
			if scenario != "public-ingress" {
				args = append(args, "--exposure", "tor", "--tor-image", testImage, "--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t))
			}
			if scenario != "audience-only" {
				args = append(args, "--moderation-action-public-key", key)
			}
			audience := testModerationAudience
			if scenario == "short-audience" {
				audience = "abcd"
			} else if scenario == "uppercase-audience" {
				audience = strings.ToUpper(audience)
			}
			if scenario != "key-only" {
				args = append(args, "--set", "MODERATION_ACTION_AUDIENCE="+audience)
			}
			if scenario == "set-secret" {
				args = append(args, "--set", "MODERATION_ACTION_PRIVATE_KEY=synthetic-never-import")
			}
			if _, err := invoke(t, args...); err == nil {
				t.Fatal("unsafe offline verification configuration was accepted")
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatal("invalid configuration published an installation")
			}
		})
	}
}

func TestModerationOfflineReviewVerifierSurvivesApplicationRedeploy(t *testing.T) {
	dir, _ := initModerationReview(t)
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "docker-args")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_CONFIG\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	if out, err := invoke(t, "install", "--dir", dir); err != nil {
		t.Fatalf("install: %v (%s)", err, out)
	}
	before, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	pinPath := filepath.Join(dir, before.PublicFiles["MODERATION_ACTION_PUBLIC_KEY"])
	pin, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	nextImage := "example.invalid/market@sha256:" + strings.Repeat("cd", 32)
	if out, err := invoke(t, "redeploy", "--dir", dir, "--app-image", nextImage); err != nil {
		t.Fatalf("redeploy: %v (%s)", err, out)
	}
	after, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.PublicFiles["MODERATION_ACTION_PUBLIC_KEY"] != before.PublicFiles["MODERATION_ACTION_PUBLIC_KEY"] ||
		after.Settings["MODERATION_ACTION_AUDIENCE"] != before.Settings["MODERATION_ACTION_AUDIENCE"] ||
		after.RetentionDir != before.RetentionDir || after.AdminOnionAddress != before.AdminOnionAddress {
		t.Fatal("application rebuild changed the offline verification, retention or private ingress configuration")
	}
	retainedPin, err := os.ReadFile(pinPath)
	if err != nil || !bytes.Equal(pin, retainedPin) {
		t.Fatal("redeploy changed the installed moderation public key")
	}
	arguments, err := os.ReadFile(record)
	if err != nil || strings.Count(string(arguments), "\n") != 1 ||
		!strings.HasSuffix(string(arguments), "up -d --wait --wait-timeout 600 --no-deps app\n") {
		t.Fatal("redeploy touched services other than the existing app")
	}
}

func TestModerationOfflineReviewRevalidatesInstalledPublicKey(t *testing.T) {
	for _, changed := range []string{strings.Repeat("ab", 64), strings.Repeat("AB", 32), strings.Repeat("ab", 32) + "\r\n"} {
		t.Run(changed[:8]+":"+strings.ReplaceAll(changed[len(changed)-2:], "\n", "LF"), func(t *testing.T) {
			dir, _ := initModerationReview(t)
			c, err := loadConfig(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, c.PublicFiles["MODERATION_ACTION_PUBLIC_KEY"]), []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := invoke(t, "plan", "--dir", dir); err == nil {
				t.Fatal("stored verification key was not validated on reload")
			}
		})
	}
}

func TestModerationOfflineReviewCanonicalizesImportedPublicKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "canonical-review")
	key := filepath.Join(t.TempDir(), "offline-admin.pub")
	if err := os.WriteFile(key, []byte(" \t"+strings.Repeat("AB", 32)+"\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke(t, moderationReviewInitArgs(t, dir, key)...); err != nil {
		t.Fatalf("init: %v (%s)", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := os.ReadFile(filepath.Join(dir, c.PublicFiles["MODERATION_ACTION_PUBLIC_KEY"]))
	if err != nil || string(pinned) != strings.Repeat("ab", 32)+"\n" {
		t.Fatal("import did not retain runtime-compatible canonical public-key bytes")
	}
	if err := validateModerationActionPublicKey(pinned); err != nil {
		t.Fatalf("imported key fails runtime-format validation: %v", err)
	}
}
