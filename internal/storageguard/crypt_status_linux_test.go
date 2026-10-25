//go:build linux

package storageguard

import (
	"io"
	"testing"
)

func TestCryptStatusOutputLimit(t *testing.T) {
	var output statusOutput
	if _, err := output.Write(make([]byte, 16<<10)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{'x'}); err != io.ErrShortBuffer || !output.exceeded || len(output.data) != 16<<10 {
		t.Fatal("oversized status output was accepted")
	}
}

func TestFilesystemUUIDOutputLimit(t *testing.T) {
	var output filesystemUUIDOutput
	if _, err := output.Write(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{'x'}); err != io.ErrShortBuffer || !output.exceeded || len(output.data) != 64 {
		t.Fatal("oversized UUID header output was accepted")
	}
}

func TestCryptTargetRejectsInvalidKernelDeviceNames(t *testing.T) {
	for _, node := range []string{"/sys/devices/plain", "/sys/devices/dm-invalid", "/sys/devices/dm-"} {
		if err := verifyCryptTarget("253:0", node); err == nil {
			t.Fatal("invalid mapping accepted")
		}
	}
}
