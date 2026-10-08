package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportAuditCLIIsPrivateReadOnlyAndVerifiable(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO audit_events(action,created) VALUES('private operator event','2026-01-01T00:00:00Z'),('second event','2026-01-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	const seed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	t.Setenv("AUDIT_SIGNING_KEY", seed)
	dir := t.TempDir()
	// Invalid wallet URLs are intentionally present: the host-only command must never initialize a wallet.
	env := []string{"AUDIT_SIGNING_KEY=" + seed, "BITCOIN_RPC_URL=http://127.0.0.1:1/forbidden", "MONERO_WALLET_RPC_URL=http://127.0.0.1:1/forbidden"}
	out, stderr, code := runServer(t, env, "-export-audit", "2", "-export-audit-dir", dir)
	if code != 0 || stderr != "" || !strings.Contains(out, "Saved ") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	dataPath, sigPath := filepath.Join(dir, "audit-events-upto-2.jsonl"), filepath.Join(dir, "audit-events-upto-2.sig")
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
	if !ed25519.Verify(pub, data, signature) {
		t.Fatal("CLI detached signature fails verification")
	}
	var row struct {
		ID     int64
		Action string
	}
	if err = json.Unmarshal(bytes.Split(data, []byte("\n"))[0], &row); err != nil || row.ID != 1 || row.Action != "private operator event" {
		t.Fatalf("row %+v err %v", row, err)
	}
	for _, p := range []string{dataPath, sigPath} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("private file permissions: %s %v", p, err)
		}
	}
	if _, stderr, code = runServer(t, env, "-export-audit", "2", "-export-audit-dir", dir); code == 0 || !strings.Contains(stderr, "cannot create export file") {
		t.Fatalf("overwrite accepted: %d %q", code, stderr)
	}
	preserved, _ := os.ReadFile(dataPath)
	if !bytes.Equal(preserved, data) {
		t.Fatal("refused overwrite changed existing data")
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != 2 {
		t.Fatalf("host export mutated audit rows: %d %v", count, err)
	}
	rangeDir := t.TempDir()
	if _, stderr, code = runServer(t, env, "-export-audit", "2", "-export-audit-from", "1", "-export-audit-dir", rangeDir); code != 0 || stderr != "" {
		t.Fatalf("range exit=%d err=%q", code, stderr)
	}
	ranged, err := os.ReadFile(filepath.Join(rangeDir, "audit-events-from-1-upto-2.jsonl"))
	if err != nil || bytes.Contains(ranged, []byte("private operator event")) || !bytes.Contains(ranged, []byte("second event")) {
		t.Fatalf("range %q %v", ranged, err)
	}
	// Existing signature refuses the pair and removes only the data file just created by this invocation.
	blocked := t.TempDir()
	existing := filepath.Join(blocked, "audit-events-upto-2.sig")
	if err = os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code = runServer(t, env, "-export-audit", "2", "-export-audit-dir", blocked); code == 0 || !strings.Contains(stderr, "cannot create signature file") {
		t.Fatalf("existing signature accepted: %d %q", code, stderr)
	}
	if _, err = os.Stat(filepath.Join(blocked, "audit-events-upto-2.jsonl")); !os.IsNotExist(err) {
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
