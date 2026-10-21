package ramsecrets

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRecoveryAPICollectsAndStrictlyValidatesBoundedInstallation(t *testing.T) {
	dir, _ := fixture(t)
	data, err := CollectInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ParseInstallation(data)
	if err != nil || len(bundle.Files) != 2 {
		t.Fatalf("collected bundle: %v", err)
	}
	for _, data := range [][]byte{nil, bytes.Repeat([]byte{'x'}, maxBundle+1), append(data, []byte(`{}`)...), []byte(`{"version":1,"unexpected":true}`)} {
		if _, err := ParseInstallation(data); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	bundle.Files[0].Path = "../escape"
	data, _ = json.Marshal(bundle)
	if _, err := ParseInstallation(data); err == nil {
		t.Fatal("traversal accepted")
	}
	if _, err := EncryptInstallation(nil, bytes.Repeat([]byte{'x'}, 2*maxBundle+1)); err == nil {
		t.Fatal("oversized recovery document accepted")
	}
}
