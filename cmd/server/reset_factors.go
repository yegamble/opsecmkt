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

// resetAdminFactors is -reset-admin-factors HANDLE: the host break-glass that turns off an administrator's second
// factors (A-137; market.ResetAdminFactors), for example after a SETUP_TOKEN/APP_SECRET_KEY rotation left its TOTP
// secret unreadable. It reads nothing from stdin, connects with DATABASE_URL and starts no HTTP server, payment
// provider or background work. given reports which flags were set; args are the arguments after the flags.
// Success output is the one-line summary of the change.
func resetAdminFactors(ctx context.Context, handle string, given func(string) bool, args []string, out io.Writer) error {
	for _, other := range []string{"preview", "reset-admin-password", "bootstrap-admin", "site-name", "set-role"} {
		if given(other) {
			return errors.New("cannot be combined with -" + other)
		}
	}
	if handle == "" || len(args) > 0 {
		return errors.New("give the administrator's handle only: -reset-admin-factors HANDLE")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		summary, err := market.ResetAdminFactors(ctx, db, handle)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, summary+". Sign in with the password alone, then set up TOTP or PGP sign-in again.")
		return nil
	})
}
