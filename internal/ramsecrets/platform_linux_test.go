//go:build linux

package ramsecrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxPublicationNeverReplacesExistingDirectory(t *testing.T) {
	parent := t.TempDir()
	stage := filepath.Join(parent, "stage")
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "file"), []byte("disposable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := publishMemory(stage, target); err == nil {
		t.Fatal("replaced existing directory")
	}
	if _, err := os.Stat(filepath.Join(stage, "file")); err != nil {
		t.Fatal("failed publication modified source")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := publishMemory(stage, target); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxFilesystemInspectionUsesNearestExistingParent(t *testing.T) {
	parent := t.TempDir()
	expected, err := IsTmpfs(parent)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := IsTmpfs(filepath.Join(parent, "not-yet-created", "retention"))
	if err != nil || actual != expected {
		t.Fatalf("nearest parent classification: %v %v", actual, err)
	}
	if Verify("/") == nil {
		t.Fatal("accepted root directory")
	}
}
