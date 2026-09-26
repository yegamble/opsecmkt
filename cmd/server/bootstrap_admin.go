package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"os"
)

// flagGiven reports whether the named flag was set on the command line.
func flagGiven(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// bootstrapAdmin is -bootstrap-admin: first-run setup from the host, for the operator who can run the binary
// with DATABASE_URL (that is the security boundary; there is no web path, D-26). It never reads SETUP_TOKEN.
// It reads the first administrator's password from stdin like -reset-admin-password, then installs the market
// with that one administrator and the -site-name marketplace name exactly as POST /setup would
// (market.BootstrapAdmin). It starts no HTTP server, payment provider or background work. given reports which
// flags were set. Success output names the handle only.
func bootstrapAdmin(ctx context.Context, handle, siteName string, given func(string) bool, args []string, in *os.File, out, prompt io.Writer) error {
	switch {
	case !given("bootstrap-admin"):
		return errors.New("-site-name needs -bootstrap-admin HANDLE")
	case given("preview"):
		return errors.New("cannot be combined with -preview")
	case given("reset-admin-password"):
		return errors.New("cannot be combined with -reset-admin-password")
	case handle == "":
		return errors.New("give the first administrator's handle: -bootstrap-admin HANDLE")
	case len(args) > 0:
		return errors.New("unexpected arguments after the handle; the password is read from stdin, never from the command line")
	}
	if err := market.CheckBootstrapInput(handle, siteName); err != nil {
		return err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	return runBootstrap(ctx, dsn, handle, siteName, in, func() (func(), error) { return hideUntilDone(in) }, out, prompt)
}

// runBootstrap reads the password (readNewPassword: asked twice with echo off on a terminal, or one line from a
// pipe), connects with dsn and runs market.BootstrapAdmin, then reports success on out.
func runBootstrap(ctx context.Context, dsn, handle, siteName string, in io.Reader, hide func() (func(), error), out, prompt io.Writer) error {
	password, err := readNewPassword(in, prompt, handle, hide)
	if err != nil {
		return err
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		if err := market.BootstrapAdmin(ctx, db, handle, password, siteName); err != nil {
			return err
		}
		fmt.Fprintf(out, "Setup complete: administrator %s created. Sign in with that handle and password.\n", handle)
		return nil
	})
}
