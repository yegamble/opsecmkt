//go:build linux

package emergency

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A harmless regular-file child proves descriptor routing survives replacement
// of the supplied pathname. No cryptsetup, block device or reboot is involved.
func TestDeviceChildKeepsOpenedDescriptorAfterPathReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selected")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := deviceCommand(context.Background(), "/bin/cat", f, inheritedDevicePath)
	got, err := cmd.Output()
	if err != nil || string(got) != "original" {
		t.Fatalf("held descriptor was redirected: %q, %v", got, err)
	}
}

func TestLUKSIdentityOutputRejectsMalformedAndOversizedValues(t *testing.T) {
	for _, value := range []string{"", "not-a-uuid", "11111111-1111-1111-1111-11111111111z", "111111111111-1111-1111-111111111111"} {
		if validLUKSUUID(value) {
			t.Fatalf("accepted malformed identity %q", value)
		}
	}
	if !validLUKSUUID("11111111-1111-1111-1111-111111111111") {
		t.Fatal("rejected canonical identity")
	}
	var out uuidOutput
	if _, err := out.Write(make([]byte, 65)); err == nil || len(out.data) != 0 {
		t.Fatal("unbounded identity output accepted")
	}
}
