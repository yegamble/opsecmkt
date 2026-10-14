package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func TestExportAuditCLIIsPrivateReadOnlyAndVerifiable(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var before int
	if err = db.QueryRow("SELECT count(*) FROM audit_events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	var firstID, secondID int64
	if err = db.QueryRow("INSERT INTO audit_events(action,created) VALUES('private operator event','2026-01-01T00:00:00Z') RETURNING id").Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("INSERT INTO audit_events(action,created) VALUES('second event','2026-01-02T00:00:00Z') RETURNING id").Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	upto := strconv.FormatInt(secondID, 10)
	base := fmt.Sprintf("audit-events-upto-%d", secondID)

	const seed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	t.Setenv("AUDIT_SIGNING_KEY", seed)
	dir := t.TempDir()
	// Invalid wallet URLs are intentionally present: the host-only command must never initialize a wallet.
	env := []string{"AUDIT_SIGNING_KEY=" + seed, "BITCOIN_RPC_URL=http://127.0.0.1:1/forbidden", "MONERO_WALLET_RPC_URL=http://127.0.0.1:1/forbidden"}
	out, stderr, code := runServer(t, env, "-export-audit", upto, "-export-audit-dir", dir)
	if code != 0 || stderr != "" || !strings.Contains(out, "Saved ") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	dataPath, sigPath := filepath.Join(dir, base+".jsonl"), filepath.Join(dir, base+".sig")
	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sig)), "\n") {
		k, v, _ := strings.Cut(line, ": ")
		fields[k] = v
	}
	pub, _ := hex.DecodeString(fields["public_key"])
	signature, _ := hex.DecodeString(fields["signature"])
	seedBytes, _ := hex.DecodeString(seed)
	pinned := ed25519.NewKeyFromSeed(seedBytes).Public().(ed25519.PublicKey)
	digest := sha256.Sum256(data)
	if !bytes.Equal(pub, pinned) || !ed25519.Verify(pinned, data, signature) || fields["sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatal("CLI signature/hash does not verify under the configured key")
	}
	foundFirst, foundSecond := false, false
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var row struct {
			ID     int64
			Action string
		}
		if err = json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row.ID == firstID {
			foundFirst = row.Action == "private operator event"
		}
		if row.ID == secondID {
			foundSecond = row.Action == "second event"
		}
	}
	if !foundFirst || !foundSecond {
		t.Fatal("export omitted fixture events")
	}

	for _, p := range []string{dataPath, sigPath} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("private file permissions: %s %v", p, err)
		}
	}
	if _, stderr, code = runServer(t, env, "-export-audit", upto, "-export-audit-dir", dir); code == 0 || !strings.Contains(stderr, "cannot create export file") {
		t.Fatalf("overwrite accepted: %d %q", code, stderr)
	}
	preserved, _ := os.ReadFile(dataPath)
	if !bytes.Equal(preserved, data) {
		t.Fatal("refused overwrite changed existing data")
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != before+2 {
		t.Fatalf("host export mutated audit rows: %d %v", count, err)
	}
	rangeDir := t.TempDir()
	if _, stderr, code = runServer(t, env, "-export-audit", upto, "-export-audit-from", strconv.FormatInt(firstID, 10), "-export-audit-dir", rangeDir); code != 0 || stderr != "" {
		t.Fatalf("range exit=%d err=%q", code, stderr)
	}
	ranged, err := os.ReadFile(filepath.Join(rangeDir, fmt.Sprintf("audit-events-from-%d-upto-%d.jsonl", firstID, secondID)))
	if err != nil || bytes.Contains(ranged, []byte("private operator event")) || !bytes.Contains(ranged, []byte("second event")) {
		t.Fatalf("range %q %v", ranged, err)
	}
	// Existing signature refuses the pair and removes only the data file just created by this invocation.
	blocked := t.TempDir()
	existing := filepath.Join(blocked, base+".sig")
	if err = os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code = runServer(t, env, "-export-audit", upto, "-export-audit-dir", blocked); code == 0 || !strings.Contains(stderr, "cannot create signature file") {
		t.Fatalf("existing signature accepted: %d %q", code, stderr)
	}
	if _, err = os.Stat(filepath.Join(blocked, base+".jsonl")); !os.IsNotExist(err) {
		t.Fatal("failed pair kept partial data file")
	}
	saved, _ := os.ReadFile(existing)
	if string(saved) != "keep" {
		t.Fatal("failed pair changed existing signature")
	}
}

func TestExportAuditRefusesInvalidInvocationBeforeConnecting(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://private:secret@127.0.0.1:1/private?sslmode=disable")
	for _, tc := range []struct {
		from, upto int64
		flags      []string
		args       []string
	}{
		{0, 0, []string{"export-audit"}, nil}, {-1, 2, []string{"export-audit"}, nil}, {3, 2, []string{"export-audit"}, nil},
		{0, 2, []string{"export-audit-from"}, nil}, {0, 2, []string{"export-audit", "preview"}, nil},
		{0, 2, []string{"export-audit", "set-role"}, nil}, {0, 2, []string{"export-audit"}, []string{"unexpected"}},
	} {
		var out bytes.Buffer
		given := func(s string) bool {
			for _, f := range tc.flags {
				if f == s {
					return true
				}
			}
			return false
		}
		if err := exportAudit(context.Background(), tc.from, tc.upto, t.TempDir(), given, tc.args, &out); err == nil || out.Len() != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid invocation: err=%v output=%q", err, out.String())
		}
	}
}

func TestExportAuditRecipientRefusedBeforeDatabaseLookup(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AUDIT_SIGNING_KEY", "")
	path := filepath.Join(t.TempDir(), "private-recipient-path.asc")
	t.Setenv("AUDIT_EXPORT_RECIPIENT", path)
	dir := t.TempDir()
	var out bytes.Buffer
	given := func(s string) bool { return s == "export-audit" }
	for _, data := range []string{"", "invalid-private-key-marker", strings.Repeat("X", 64<<10+1)} {
		if data != "" {
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		err := exportAudit(context.Background(), 0, 1, dir, given, nil, &out)
		if err == nil || !strings.Contains(err.Error(), "AUDIT_EXPORT_RECIPIENT") || strings.Contains(err.Error(), "DATABASE_URL") || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "invalid-private-key-marker") || out.Len() != 0 {
			t.Fatalf("recipient failure did not precede database lookup safely: %v %q", err, out.String())
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid recipient created outputs: %v %v", entries, err)
		}
	}
}

func TestExportAuditCLIEncryptedPairIsPrivateAndVerifiable(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var upto int64
	if err := db.QueryRow("INSERT INTO audit_events(action) VALUES('encrypted-private-event-marker') RETURNING id").Scan(&upto); err != nil {
		t.Fatal(err)
	}
	const seed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	t.Setenv("AUDIT_SIGNING_KEY", seed)
	key, err := openpgp.NewEntity("audit offline custody", "", "audit@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var pub bytes.Buffer
	w, err := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = key.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "recipient.asc")
	if err = os.WriteFile(keyPath, pub.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIT_EXPORT_RECIPIENT", keyPath)
	want, wantSig, err := market.ExportAudit(context.Background(), db, 0, upto)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := []string{"AUDIT_SIGNING_KEY=" + seed, "AUDIT_EXPORT_RECIPIENT=" + keyPath, "BITCOIN_RPC_URL=http://127.0.0.1:1/forbidden", "MONERO_WALLET_RPC_URL=http://127.0.0.1:1/forbidden"}
	out, stderr, code := runServer(t, env, "-export-audit", fmt.Sprint(upto), "-export-audit-dir", dir)
	if code != 0 || stderr != "" || !strings.Contains(out, "Decrypt both offline") || strings.Contains(out, "encrypted-private-event-marker") {
		t.Fatalf("encrypted CLI: %d %q %q", code, out, stderr)
	}
	base := fmt.Sprintf("audit-events-upto-%d", upto)
	decrypt := func(ciphertext []byte) []byte {
		t.Helper()
		block, err := armor.Decode(bytes.NewReader(ciphertext))
		if err != nil {
			t.Fatal(err)
		}
		md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(md.UnverifiedBody)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	sealed := make(map[string][]byte)
	for _, tc := range []struct {
		ext  string
		want []byte
	}{{".jsonl", want}, {".sig", wantSig}} {
		path := filepath.Join(dir, base+tc.ext+".asc")
		ciphertext, err := os.ReadFile(path)
		if err != nil || !bytes.HasPrefix(ciphertext, []byte("-----BEGIN PGP MESSAGE-----")) || bytes.Contains(ciphertext, []byte("encrypted-private-event-marker")) {
			t.Fatalf("ciphertext output: %q %v", ciphertext, err)
		}
		sealed[tc.ext] = ciphertext
		if got := decrypt(ciphertext); !bytes.Equal(got, tc.want) {
			t.Fatalf("decrypted %s differs from raw signed export", tc.ext)
		}
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("ciphertext permissions: %v %v", st, err)
		}
		if _, err := os.Stat(filepath.Join(dir, base+tc.ext)); !os.IsNotExist(err) {
			t.Fatal("configured recipient left a plaintext export file")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("encrypted export created unexpected files: %v %v", entries, err)
	}
	// The decrypted detached signature is still Ed25519 over the exact plaintext.
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(decrypt(sealed[".sig"]))), "\n") {
		k, v, _ := strings.Cut(line, ": ")
		fields[k] = v
	}
	signature, _ := hex.DecodeString(fields["signature"])
	seedBytes, _ := hex.DecodeString(seed)
	pinned := ed25519.NewKeyFromSeed(seedBytes).Public().(ed25519.PublicKey)
	if !ed25519.Verify(pinned, decrypt(sealed[".jsonl"]), signature) {
		t.Fatal("offline decrypted signature failed verification")
	}
	if _, stderr, code = runServer(t, env, "-export-audit", fmt.Sprint(upto), "-export-audit-dir", dir); code == 0 || !strings.Contains(stderr, "cannot create export file") {
		t.Fatalf("ciphertext overwrite allowed: %d %q", code, stderr)
	}
	preserved, _ := os.ReadFile(filepath.Join(dir, base+".jsonl.asc"))
	if !bytes.Equal(preserved, sealed[".jsonl"]) {
		t.Fatal("refused ciphertext overwrite changed existing bytes")
	}
	// Failure to create the second file removes only this invocation's ciphertext.
	blocked := t.TempDir()
	existing := filepath.Join(blocked, base+".sig.asc")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code = runServer(t, env, "-export-audit", fmt.Sprint(upto), "-export-audit-dir", blocked); code == 0 || !strings.Contains(stderr, "cannot create signature file") {
		t.Fatalf("existing encrypted signature allowed: %d %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(blocked, base+".jsonl.asc")); !os.IsNotExist(err) {
		t.Fatal("failed encrypted pair kept partial ciphertext")
	}
	saved, _ := os.ReadFile(existing)
	if string(saved) != "keep" {
		t.Fatal("failed pair changed existing encrypted signature")
	}
}
