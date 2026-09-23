package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func signed(t *testing.T, export string) (pubHex string, sig string, key ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(export))
	return hex.EncodeToString(pub), fmt.Sprintf("algorithm: ed25519\npublic_key: %x\nsha256: %x\nsignature: %x\n", pub, sum, ed25519.Sign(key, []byte(export))), key
}

func files(t *testing.T, export, sig string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	e, s := filepath.Join(dir, "export.jsonl"), filepath.Join(dir, "export.sig")
	if os.WriteFile(e, []byte(export), 0o600) != nil || os.WriteFile(s, []byte(sig), 0o600) != nil {
		t.Fatal("write")
	}
	return e, s
}

const sample = `{"id":1,"user_id":"a","handle":"admin","action":"Account created: admin","created":"2026-09-23T10:00:00Z"}
{"id":3,"user_id":null,"handle":null,"action":"x","created":"2026-09-23T10:01:00Z"}
`

func TestRun(t *testing.T) {
	pub, sig, _ := signed(t, sample)
	e, s := files(t, sample, sig)
	var out, errb bytes.Buffer
	if code := run([]string{"-pub", pub, e, s}, &out, &errb); code != 0 || !strings.Contains(out.String(), "OK: 2 events (last id 3)") {
		t.Fatalf("valid export: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	otherPub, otherSig, _ := signed(t, sample)
	tampered := strings.Replace(sample, `"x"`, `"y"`, 1)
	reordered := "{\"id\":3,\"action\":\"x\",\"created\":\"c\"}\n{\"id\":1,\"action\":\"x\",\"created\":\"c\"}\n"
	_, reorderedSig, _ := signed(t, reordered)
	for name, tc := range map[string]struct{ pub, export, sig, want string }{
		"tampered export":        {pub, tampered, sig, "sha256 does not match"},
		"other pinned key":       {otherPub, sample, sig, "different public key"},
		"sig names pinned key":   {otherPub, sample, strings.Replace(sig, pub, otherPub, 1), "does not verify"},
		"forged key and sig":     {pub, sample, otherSig, "different public key"},
		"bad pub":                {"zz", sample, sig, "64 hexadecimal"},
		"wrong algorithm":        {pub, sample, strings.Replace(sig, "ed25519", "rsa", 1), "unsupported algorithm"},
		"ids not ascending":      {strings.Fields(reorderedSig)[3], reordered, reorderedSig, "ascending"},
		"truncated signature":    {pub, sample, sig[:len(sig)-10] + "\n", "malformed signature"},
		"garbage signature file": {pub, sample, "nothing here", "malformed signature file"},
	} {
		t.Run(name, func(t *testing.T) {
			e, s := files(t, tc.export, tc.sig)
			var out, errb bytes.Buffer
			if code := run([]string{"-pub", tc.pub, e, s}, &out, &errb); code != 1 || !strings.Contains(errb.String(), tc.want) || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q; want %q", code, out.String(), errb.String(), tc.want)
			}
		})
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{e, s}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("missing -pub: %d %q", code, errb.String())
	}
	if code := run([]string{"-pub", pub, e, filepath.Join(t.TempDir(), "missing.sig")}, &out, &errb); code != 1 {
		t.Fatalf("missing file: %d", code)
	}
}
