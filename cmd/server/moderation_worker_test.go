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
