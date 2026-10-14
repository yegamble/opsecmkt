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

func enrollRetention(ctx context.Context, given func(string) bool, args []string, out io.Writer) error {
	for _, other := range []string{"preview", "healthcheck", "status", "reset-admin-password", "reset-admin-factors", "set-role", "bootstrap-admin", "site-name", "export-audit", "export-audit-from", "export-audit-dir"} {
		if given(other) {
			return errors.New("cannot be combined with -" + other)
		}
	}
	if len(args) != 0 {
		return errors.New("-enroll-retention takes no arguments")
	}
	if _, err := market.RetentionEnrollmentPath(os.Getenv); err != nil {
		return err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		if err := market.EnrollRetention(ctx, db, os.Getenv); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Original database retention enrolled; original grace clocks preserved. No expiry, replay, server or wallets started. Keep this current external state separate from database backups.")
		return err
	})
}
