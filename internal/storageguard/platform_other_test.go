//go:build !linux

package storageguard

import "testing"

func TestPersistentVerificationUnsupportedOutsideLinux(t *testing.T) {
	if _, err := Inspect("/"); err == nil {
		t.Fatal("non-Linux production storage verification accepted")
	}
	if err := VerifyPersistent("/"); err == nil {
		t.Fatal("non-Linux persistent storage verification accepted")
	}
}
