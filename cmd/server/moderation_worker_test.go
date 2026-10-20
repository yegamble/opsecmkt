package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModerationWorkerDisabledAndHelpRequireNoSecretsOrNetwork(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	t.Setenv("DATABASE_URL", "postgres://must-never-be-used.invalid/disabled")
	t.Setenv("APP_SECRET_KEY_FILE", "/nonexistent-secret")
	for _, args := range [][]string{nil, {"--help"}} {
		var out bytes.Buffer
		if err := runModerationWorker(context.Background(), args, &out); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 || strings.Contains(out.String(), "must-never-be-used") {
			t.Fatal("missing or unsafe worker output")
		}
	}
}

func TestModerationWorkerRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	for _, args := range [][]string{{"--enabled", "--disable"}, {"--interval", "1ms"}, {"--batch", "101"}, {"--sla", "0s"}, {"--confidence-floor", "NaN"}, {"--new-actor-days", "-1"}, {"--prompt-version", "unreviewed"}, {"--enabled", "--model"}, {"--unknown"}} {
		if err := runModerationWorker(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestModerationWorkerRejectsRetiredPromptProtocol(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	for _, retired := range []string{"moderation-v1", "moderation-v2", "moderation-v3"} {
		if err := runModerationWorker(context.Background(), []string{"--prompt-version", retired}, &bytes.Buffer{}); err == nil {
			t.Fatalf("retired model protocol %s was accepted", retired)
		}
	}
}

func TestModerationWorkerRejectsUnsafeCapacityBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	for _, args := range [][]string{{"--max-pending", "0"}, {"--max-pending", "10001"}, {"--max-records", "0"}, {"--max-records", "1000001"}, {"--max-reserved-bytes", "524287"}, {"--max-reserved-bytes", "1099511627777"}} {
		err := runModerationWorker(context.Background(), args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "invalid capacity limits") {
			t.Fatalf("unsafe capacity was not refused before database access: %v: %v", args, err)
		}
	}
}

func TestModerationWorkerDisableOverridesInheritedEnabledSetting(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "on")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", "")
	err := runModerationWorker(context.Background(), []string{"--disable"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "database input required") {
		t.Fatalf("disable did not override inherited enabled setting: %v", err)
	}
}

func TestModerationWorkerReadsOnlyPrivateSelectedSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker-database")
	if err := os.WriteFile(path, []byte("test-only-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readModerationPrivate(path)
	if err != nil || string(got) != "test-only-value" {
		t.Fatalf("read=%q err=%v", got, err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = readModerationPrivate(path); err == nil {
		t.Fatal("accepted public secret")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = readModerationPrivate(link); err == nil {
		t.Fatal("accepted secret symlink")
	}
}
