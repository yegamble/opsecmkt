package market

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
)

// A-481: the image description CHECK (length <= 250), handle rules, case folding and sorting assume character
// semantics, which only a UTF8 database gives (SQL_ASCII counts bytes). Startup refuses any other encoding
// before a migration runs, and every connection the application opens asks for client_encoding UTF8.

// encodingTestDB creates a database with the given encoding (from template0, C locale so any encoding is
// allowed) next to TEST_DATABASE_URL's and returns a DSN for it; it is dropped on cleanup.
func encodingTestDB(t *testing.T, encoding string) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "opsecmkt_enc_" + strings.ToLower(encoding) + "_" + randomToken()[:12]
	if _, err = admin.Exec("CREATE DATABASE " + name + " ENCODING '" + encoding + "' TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		admin.Close()
		t.Skipf("cannot create a %s database (needs CREATEDB): %v", encoding, err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	return testDatabaseURL(base, name)
}

func TestStartupRefusesNonUTF8Database(t *testing.T) {
	for _, encoding := range []string{"SQL_ASCII", "LATIN1"} {
		t.Run(encoding, func(t *testing.T) {
			dsn := encodingTestDB(t, encoding)
			t.Setenv("DATABASE_URL", dsn)
			t.Setenv("SETUP_TOKEN", testSetupToken)
			t.Setenv("COOKIE_SECURE", "false")
			a, err := New(context.Background(), false)
			if err == nil {
				a.Close()
				t.Fatalf("New started on a %s database", encoding)
			}
			for _, want := range []string{encoding, "UTF8", "CREATE DATABASE", "ENCODING 'UTF8' TEMPLATE template0", "docs/operator-guide.md",
				"UPGRADING.md section 42", "scripts/check-utf8.sql", "BACKUP_DUMP_ENCODING=SQL_ASCII"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
			// QA5W4B-1: a plain pg_dump and pg_restore double-encodes a LATIN1 database and copies bytes that are not
			// UTF-8 unchecked, the move section 42 calls unsafe.
			if strings.Contains(err.Error(), "pg_dump and pg_restore") {
				t.Errorf("error still advises a plain pg_dump and pg_restore: %v", err)
			}
			if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "postgres://") {
				t.Errorf("error leaks connection details: %v", err)
			}
			// Refused before the baseline or any migration: the database still has no tables.
			db, oerr := sql.Open("pgx", dsn)
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer db.Close()
			var n int
			if qerr := db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'").Scan(&n); qerr != nil || n != 0 {
				t.Fatalf("the refused start left %d tables behind (err %v)", n, qerr)
			}
		})
	}
}

func TestStartupAcceptsUTF8Database(t *testing.T) {
	dsn := encodingTestDB(t, "UTF8")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("COOKIE_SECURE", "false")
	a, err := New(context.Background(), false)
	if err != nil {
		t.Fatalf("New on a UTF8 database: %v", err)
	}
	defer a.Close()
	var enc, client string
	if err := a.db.QueryRow("SHOW server_encoding").Scan(&enc); err != nil || enc != "UTF8" {
		t.Fatalf("server_encoding = %q, %v", enc, err)
	}
	if err := a.db.QueryRow("SHOW client_encoding").Scan(&client); err != nil || client != "UTF8" {
		t.Fatalf("application pool client_encoding = %q, %v", client, err)
	}
}

// Every connection OpenDB makes uses client_encoding UTF8, whatever the DSN, the role or the environment say.
func TestOpenDBPinsClientEncoding(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("client_encoding", "LATIN1")
	u.RawQuery = q.Encode()
	for name, opts := range map[string][]DBOption{"plain": nil, "app": {WithAppTimeouts()}} {
		db, err := OpenDB(u.String(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		err = db.QueryRow("SHOW client_encoding").Scan(&got)
		db.Close()
		if err != nil || got != "UTF8" {
			t.Errorf("%s: client_encoding = %q, %v", name, got, err)
		}
	}
}
