package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// runCmd runs the command line with stdin and an environment of its own and returns the exit code and both outputs.
func runCmd(t *testing.T, env map[string]string, stdin []byte, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, bytes.NewReader(stdin), &out, &errOut, func(k string) string { return env[k] })
	return code, out.String(), errOut.String()
}

// secretFree fails the test when any output repeats the seed (hex or raw) or a tprv.
func secretFree(t *testing.T, seed []byte, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		if strings.Contains(o, "tprv") || strings.Contains(o, hex.EncodeToString(seed)) || bytes.Contains([]byte(o), seed) {
			t.Fatalf("output holds key material: %q", o)
		}
	}
}

// TestSignerInit: init writes 32 (or, asked, 64) random bytes to a new mode-0400 file and prints the account tpub (as
// Core's getdescriptorinfo gives it) and fingerprint; it refuses a file that exists (unchanged) and any other size, and
// writes nothing without Core's answer (no RPC URL, the node down, getdescriptorinfo not whitelisted); info prints the
// same and refuses a seed file readable by group or others, or of the wrong size. No request body holds the master
// tprv.
func TestSignerInit(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "seed")
	f := newFakeCore(t)
	core := map[string]string{"ESCROW_SIGNER_RPC_URL": f.url()}
	env := map[string]string{"ESCROW_SIGNER_SEED_FILE": seedPath, "ESCROW_SIGNER_RPC_URL": f.url()}
	code, out, errOut := runCmd(t, env, nil, "init")
	if code != 0 {
		t.Fatalf("init: %d %s", code, errOut)
	}
	st, err := os.Stat(seedPath)
	seed, _ := os.ReadFile(seedPath)
	if err != nil || st.Mode().Perm() != 0o400 || len(seed) != 32 || bytes.Equal(seed, make([]byte, 32)) {
		t.Fatalf("seed file mode %v, %d bytes, %v", st.Mode(), len(seed), err)
	}
	keys := testKeys(t, newFakeCore(t), seed)
	if !strings.Contains(out, keys.accountTpub()) || !strings.Contains(out, keys.fingerprint()) {
		t.Fatalf("init output %q", out)
	}
	secretFree(t, seed, out, errOut)
	if code, out2, errOut2 := runCmd(t, env, nil, "init"); code == 0 {
		t.Fatal("init overwrote an existing seed file")
	} else if now, _ := os.ReadFile(seedPath); !bytes.Equal(now, seed) {
		t.Fatal("the existing seed file changed")
	} else {
		secretFree(t, seed, out2, errOut2)
	}
	code, info, errOut := runCmd(t, env, nil, "info")
	if code != 0 || !strings.Contains(info, keys.accountTpub()) || !strings.Contains(info, keys.fingerprint()) || !strings.Contains(info, "version 1") {
		t.Fatalf("info: %d %q %s", code, info, errOut)
	}
	secretFree(t, seed, info, errOut)
	f.noBodyHolds(t, masterTprv(t, seed), "master tprv")
	if code, _, errOut := runCmd(t, map[string]string{"ESCROW_SIGNER_SEED_FILE": seedPath}, nil, "info"); code == 0 || !strings.Contains(errOut, "ESCROW_SIGNER_RPC_URL") {
		t.Fatalf("info without ESCROW_SIGNER_RPC_URL: %d %s", code, errOut)
	}

	// QA4W2-1: without Core's account key init writes nothing.
	forbidden := newFakeCore(t)
	forbidden.forbidden = map[string]bool{"getdescriptorinfo": true}
	for name, e := range map[string]map[string]string{
		"no RPC URL":                        nil,
		"node down":                         {"ESCROW_SIGNER_RPC_URL": "http://u:hunter2@127.0.0.1:1"},
		"getdescriptorinfo not whitelisted": {"ESCROW_SIGNER_RPC_URL": forbidden.url()},
	} {
		path := filepath.Join(dir, "refused-"+strings.ReplaceAll(name, " ", "-"))
		code, out, errOut := runCmd(t, e, nil, "init", "-seed", path)
		if code == 0 || strings.Contains(out+errOut, "hunter2") || strings.Contains(out+errOut, "tprv") {
			t.Fatalf("init with %s: %d %q %q", name, code, out, errOut)
		}
		if name == "getdescriptorinfo not whitelisted" && !strings.Contains(errOut, "getdescriptorinfo") {
			t.Fatalf("init with %s does not name the method: %q", name, errOut)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("init with %s wrote a seed file", name)
		}
	}

	big := filepath.Join(dir, "seed64")
	if code, _, errOut := runCmd(t, core, nil, "init", "-seed", big, "-bytes", "64"); code != 0 {
		t.Fatalf("init -bytes 64: %s", errOut)
	}
	if b, _ := os.ReadFile(big); len(b) != 64 {
		t.Fatalf("64-byte seed has %d bytes", len(b))
	}
	if code, _, _ := runCmd(t, core, nil, "init", "-seed", filepath.Join(dir, "seed16"), "-bytes", "16"); code == 0 {
		t.Fatal("a 16-byte seed was made")
	}
	if _, err := os.Stat(filepath.Join(dir, "seed16")); err == nil {
		t.Fatal("a refused init left a file")
	}
	// A seed readable by others, or of the wrong size, is refused (serve and info read it the same way).
	loose := filepath.Join(dir, "loose")
	os.WriteFile(loose, seed, 0o640)
	if code, out, errOut := runCmd(t, core, nil, "info", "-seed", loose); code == 0 {
		t.Fatal("info accepted a seed file readable by its group")
	} else {
		secretFree(t, seed, out, errOut)
	}
	short := filepath.Join(dir, "short")
	os.WriteFile(short, seed[:31], 0o400)
	if code, out, errOut := runCmd(t, core, nil, "info", "-seed", short); code == 0 {
		t.Fatal("info accepted a 31-byte seed")
	} else {
		secretFree(t, seed, out, errOut)
	}
	if code, _, _ := runCmd(t, nil, nil, "frobnicate"); code != 2 {
		t.Fatal("an unknown command did not exit 2")
	}
}

// TestSignerBackupRestore: backup writes the seed encrypted to the operator's public key (an armored PGP MESSAGE that
// only the operator's private key opens, never the seed in clear); restore reads the decrypted seed from stdin into a
// new mode-0400 file that gives the same account, refuses to overwrite a seed file, refuses a seed of the wrong size and
// one whose account fingerprint is not the one the operator expects (nothing written), and refuses to run without
// -fingerprint (QA4W2-5) or without Core (it asks Core for the account key to check the fingerprint). backup needs no
// Core.
func TestSignerBackupRestore(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "seed")
	f := newFakeCore(t)
	core := map[string]string{"ESCROW_SIGNER_RPC_URL": f.url()}
	if code, _, errOut := runCmd(t, core, nil, "init", "-seed", seedPath); code != 0 {
		t.Fatal(errOut)
	}
	seed, _ := os.ReadFile(seedPath)
	keys := testKeys(t, newFakeCore(t), seed)

	operator, err := openpgp.NewEntity("Operator", "", "operator@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var pub bytes.Buffer
	w, _ := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	operator.Serialize(w)
	w.Close()
	keyPath := filepath.Join(dir, "operator.asc")
	os.WriteFile(keyPath, pub.Bytes(), 0o644)

	code, backup, errOut := runCmd(t, nil, nil, "backup", "-seed", seedPath, "-key", keyPath)
	if code != 0 || !strings.HasPrefix(backup, "-----BEGIN PGP MESSAGE-----") {
		t.Fatalf("backup: %d %.60q %s", code, backup, errOut)
	}
	secretFree(t, seed, backup, errOut)
	blk, err := armor.Decode(strings.NewReader(backup))
	if err != nil {
		t.Fatal(err)
	}
	md, err := openpgp.ReadMessage(blk.Body, openpgp.EntityList{operator}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(md.UnverifiedBody)
	if !bytes.Equal(plain, seed) {
		t.Fatal("the backup does not decrypt to the seed")
	}
	if code, _, _ := runCmd(t, nil, nil, "backup", "-seed", seedPath, "-key", filepath.Join(dir, "missing.asc")); code == 0 {
		t.Fatal("backup without a key file succeeded")
	}
	os.WriteFile(filepath.Join(dir, "empty.asc"), []byte("not a key"), 0o644)
	if code, out, _ := runCmd(t, nil, nil, "backup", "-seed", seedPath, "-key", filepath.Join(dir, "empty.asc")); code == 0 || out != "" {
		t.Fatal("backup to a file with no key succeeded")
	}

	restored := filepath.Join(dir, "restored", "seed")
	// QA4W2-5: restore needs -fingerprint (the pinned one); without it nothing is written.
	if code, out, errOut := runCmd(t, core, plain, "restore", "-seed", restored); code == 0 || !strings.Contains(errOut, "-fingerprint") {
		t.Fatalf("restore without -fingerprint: %d %q %q", code, out, errOut)
	} else if _, err := os.Stat(restored); err == nil {
		t.Fatal("a restore without -fingerprint wrote the seed file")
	}
	if code, _, errOut := runCmd(t, nil, plain, "restore", "-seed", restored, "-fingerprint", keys.fingerprint()); code == 0 || !strings.Contains(errOut, "ESCROW_SIGNER_RPC_URL") {
		t.Fatalf("restore without ESCROW_SIGNER_RPC_URL: %d %q", code, errOut)
	} else if _, err := os.Stat(restored); err == nil {
		t.Fatal("a restore without Core wrote the seed file")
	}
	code, out, errOut := runCmd(t, core, plain, "restore", "-seed", restored, "-fingerprint", keys.fingerprint())
	if code != 0 {
		t.Fatalf("restore: %s", errOut)
	}
	st, err := os.Stat(restored)
	back, _ := os.ReadFile(restored)
	if err != nil || st.Mode().Perm() != 0o400 || !bytes.Equal(back, seed) || !strings.Contains(out, keys.fingerprint()) {
		t.Fatalf("restored seed mode %v, equal %v, output %q", st.Mode(), bytes.Equal(back, seed), out)
	}
	secretFree(t, seed, out, errOut)
	if code, _, _ := runCmd(t, core, plain, "restore", "-seed", restored, "-fingerprint", keys.fingerprint()); code == 0 {
		t.Fatal("restore overwrote a seed file")
	}
	other := filepath.Join(dir, "other")
	if code, out, errOut := runCmd(t, core, plain, "restore", "-seed", other, "-fingerprint", "00000000"); code == 0 {
		t.Fatal("restore accepted a seed whose fingerprint differs from the expected one")
	} else {
		secretFree(t, seed, out, errOut)
	}
	if code, _, _ := runCmd(t, core, append(plain, '\n'), "restore", "-seed", other, "-fingerprint", keys.fingerprint()); code == 0 {
		t.Fatal("restore accepted 33 bytes")
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatal("a refused restore left a file")
	}
	f.noBodyHolds(t, masterTprv(t, seed), "master tprv")
}

// TestSignerServeNeedsCore: serve refuses to start without ESCROW_SIGNER_RPC_URL, with a seed it cannot read, with a
// socket path that is not absolute, or without Core's account key (the node down, getdescriptorinfo not whitelisted),
// creating no socket, and its errors never repeat the RPC URL's password.
func TestSignerServeNeedsCore(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "seed")
	if code, _, errOut := runCmd(t, map[string]string{"ESCROW_SIGNER_RPC_URL": newFakeCore(t).url()}, nil, "init", "-seed", seedPath); code != 0 {
		t.Fatal(errOut)
	}
	forbidden := newFakeCore(t)
	forbidden.forbidden = map[string]bool{"getdescriptorinfo": true}
	sock := filepath.Join(dir, "s.sock")
	for name, c := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"no RPC URL":                        {map[string]string{}, []string{"serve", "-seed", seedPath, "-socket", sock}},
		"bad RPC URL":                       {map[string]string{"ESCROW_SIGNER_RPC_URL": "http://u@h:1"}, []string{"serve", "-seed", seedPath, "-socket", sock}},
		"no seed":                           {map[string]string{"ESCROW_SIGNER_RPC_URL": "http://u:hunter2@127.0.0.1:1"}, []string{"serve", "-seed", seedPath + "x", "-socket", sock}},
		"relative socket":                   {map[string]string{"ESCROW_SIGNER_RPC_URL": "http://u:hunter2@127.0.0.1:1"}, []string{"serve", "-seed", seedPath, "-socket", "s.sock"}},
		"stray positional":                  {map[string]string{"ESCROW_SIGNER_RPC_URL": "http://u:hunter2@127.0.0.1:1"}, []string{"serve", "-seed", seedPath, "-socket", sock, "extra"}},
		"node down":                         {map[string]string{"ESCROW_SIGNER_RPC_URL": "http://u:hunter2@127.0.0.1:1"}, []string{"serve", "-seed", seedPath, "-socket", sock}},
		"getdescriptorinfo not whitelisted": {map[string]string{"ESCROW_SIGNER_RPC_URL": forbidden.url()}, []string{"serve", "-seed", seedPath, "-socket", sock}},
	} {
		code, out, errOut := runCmd(t, c.env, nil, c.args...)
		if code == 0 || strings.Contains(out+errOut, "hunter2") || strings.Contains(out+errOut, "tprv") {
			t.Fatalf("%s: %d %q %q", name, code, out, errOut)
		}
		if _, err := os.Lstat(sock); err == nil {
			t.Fatalf("%s: serve created its socket", name)
		}
	}
}
