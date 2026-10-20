package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func TestResetAdminFactorsPublicKeyFileValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	dir := t.TempDir()
	for _, tc := range []struct {
		name, contents, want string
	}{
		{"empty", "", "at most 16 KiB"},
		{"oversized", strings.Repeat("x", 16385), "at most 16 KiB"},
	} {
		path := filepath.Join(dir, tc.name+".asc")
		if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := resetAdminFactorsWithPublicKeyFile(context.Background(), "root_admin", path, given("reset-admin-factors", "reset-admin-pgp-key-file"), nil, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Fatalf("%s: error=%v output=%q", tc.name, err, out.String())
		}
	}
	for _, path := range []string{"", filepath.Join(dir, "missing.asc"), dir} {
		var out bytes.Buffer
		err := resetAdminFactorsWithPublicKeyFile(context.Background(), "root_admin", path, given("reset-admin-factors", "reset-admin-pgp-key-file"), nil, &out)
		if err == nil || !strings.Contains(err.Error(), "file") || out.Len() != 0 {
			t.Fatalf("bad file accepted or reached database: error=%v output=%q", err, out.String())
		}
	}
	stdout, stderr, code := runServer(t, nil, "-reset-admin-pgp-key-file", filepath.Join(dir, "missing.asc"))
	if code != 2 || stdout != "" || !strings.Contains(stderr, "requires -reset-admin-factors HANDLE") {
		t.Fatalf("standalone key option: %d %q %q", code, stdout, stderr)
	}
}

// The actual binary rejects private material without changing factors or records,
// then installs a public replacement even when no optional factor is enrolled.
func TestResetAdminFactorsPublicKeyCLI(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, err := openpgp.NewEntity("host-recovery", "", "recovery@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var public, private bytes.Buffer
	pubArmor, err := armor.Encode(&public, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = key.Serialize(pubArmor); err != nil {
		t.Fatal(err)
	}
	if err = pubArmor.Close(); err != nil {
		t.Fatal(err)
	}
	privateArmor, err := armor.Encode(&private, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = key.SerializePrivate(privateArmor, nil); err != nil {
		t.Fatal(err)
	}
	if err = privateArmor.Close(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role,totp_enabled,pgp_2fa) VALUES('recover_admin','recover_admin','x','admin',false,false)",
		"UPDATE settings SET value='false' WHERE key='allow_login_without_pgp'",
		"INSERT INTO sessions(token_hash,user_id,expires) VALUES('old_session','recover_admin',now()+interval '1 hour')",
		"INSERT INTO pending_logins(token_hash,user_id,expires) VALUES('old_pending','recover_admin',now()+interval '10 minutes')",
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string) (n int) {
		t.Helper()
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	startAudits := count("SELECT count(*) FROM audit_events")
	publicFile := filepath.Join(t.TempDir(), "public.asc")
	for _, tc := range []struct {
		name, contents string
	}{
		{"private", private.String()},
		{"public-plus-private", public.String() + private.String()},
		{"garbage", "not public key material"},
	} {
		path := filepath.Join(t.TempDir(), tc.name+".asc")
		if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runServer(t, nil, "-reset-admin-factors", "recover_admin", "-reset-admin-pgp-key-file", path)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "replacement") || strings.Contains(stderr, "BEGIN PGP") {
			t.Fatalf("%s: %d %q %q", tc.name, code, stdout, stderr)
		}
		if count("SELECT count(*) FROM users WHERE id='recover_admin' AND pgp='' AND pgp_verified_at IS NULL") != 1 ||
			count("SELECT count(*) FROM sessions WHERE user_id='recover_admin'") != 1 ||
			count("SELECT count(*) FROM pending_logins WHERE user_id='recover_admin'") != 1 ||
			count("SELECT count(*) FROM audit_events") != startAudits ||
			count("SELECT count(*) FROM notifications WHERE user_id='recover_admin'") != 0 {
			t.Fatal("refused private or invalid key changed recovery state")
		}
	}
	if err := os.WriteFile(publicFile, public.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runServer(t, nil, "-reset-admin-factors", "recover_admin", "-reset-admin-pgp-key-file", publicFile)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "PGP public key replaced (fingerprint ") ||
		!strings.Contains(stdout, "decrypt the challenge using the replacement private key") || strings.Contains(stdout, "password alone") ||
		strings.Contains(stdout, "BEGIN PGP") {
		t.Fatalf("public replacement: %d %q %q", code, stdout, stderr)
	}
	if count("SELECT count(*) FROM users WHERE id='recover_admin' AND pgp LIKE '-----BEGIN PGP PUBLIC KEY BLOCK-----%' AND pgp_fingerprint<>'' AND pgp_verified_at IS NULL AND NOT pgp_2fa AND NOT totp_enabled") != 1 ||
		count("SELECT count(*) FROM settings WHERE key='allow_login_without_pgp' AND value='false'") != 1 ||
		count("SELECT count(*) FROM sessions WHERE user_id='recover_admin'") != 0 ||
		count("SELECT count(*) FROM pending_logins WHERE user_id='recover_admin'") != 0 ||
		count("SELECT count(*) FROM audit_events") != startAudits+1 ||
		count("SELECT count(*) FROM notifications WHERE user_id='recover_admin'") != 1 {
		t.Fatal("public replacement failed to preserve strict policy or clear old sign-in state")
	}
}
