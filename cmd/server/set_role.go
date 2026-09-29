package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"os"
)

// setRole is -set-role HANDLE ROLE: the host command that changes an account's role, and the only way to add or
// remove an administrator (A-156; market.SetRole). It reads nothing from stdin, connects with DATABASE_URL and
// starts no HTTP server, payment provider or background work. given reports which flags were set; args are the
// arguments after the flags (the role). Success output is the one-line summary of the change.
func setRole(ctx context.Context, handle string, given func(string) bool, args []string, out io.Writer) error {
	for _, other := range []string{"preview", "reset-admin-password", "bootstrap-admin", "site-name"} {
		if given(other) {
			return errors.New("cannot be combined with -" + other)
		}
	}
	if handle == "" || len(args) != 1 {
		return errors.New("give the account's handle and its new role: -set-role HANDLE ROLE (ROLE is buyer, vendor, moderator or admin)")
	}
	role := args[0]
	if err := market.CheckRoleInput(role); err != nil {
		return err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		summary, err := market.SetRole(ctx, db, handle, role)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, summary+".")
		return nil
	})
}
