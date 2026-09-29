package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log"
	"net/url"
	"os"
	"testing"
)

// TestMain gives this package's database tests a database of their own. PostgreSQL advisory locks belong to a
// database, not a schema, and `go test ./...` runs this package alongside internal/market against the same
// TEST_DATABASE_URL. TestHostCommandsRunWithoutAppTimeouts holds the startup lock (782493) for longer than the
// application's 3 s lock_timeout on purpose, so an internal/market request that takes that lock at the same time
// (/register) failed with 503 (CI run 36622876821, TestRateLimitLoginAndRegisterPerHandle).
func TestMain(m *testing.M) {
	os.Exit(runInOwnDatabase(m))
}

func runInOwnDatabase(m *testing.M) int {
	base := os.Getenv("TEST_DATABASE_URL")
	if _, helper := os.LookupEnv("OPSMKT_HELPER_ARGS"); base == "" || helper {
		// A TestHelperServerProcess child inherits the parent's own database and exits inside main, before any
		// DROP here would run.
		return m.Run()
	}
	u, err := url.Parse(base)
	if err != nil {
		return m.Run() // the tests report the unusable URL themselves
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return m.Run()
	}
	defer admin.Close()
	b := make([]byte, 6)
	rand.Read(b)
	name := "opsecmkt_cmd_server_" + hex.EncodeToString(b)
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		log.Printf("cmd/server tests: no database of their own (%v); using TEST_DATABASE_URL's database, where their advisory locks can delay other packages' tests", err)
		return m.Run()
	}
	u.Path = "/" + name
	os.Setenv("TEST_DATABASE_URL", u.String())
	code := m.Run()
	os.Setenv("TEST_DATABASE_URL", base)
	if _, err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
		log.Printf("cmd/server tests: could not drop database %s: %v", name, err)
	}
	return code
}
