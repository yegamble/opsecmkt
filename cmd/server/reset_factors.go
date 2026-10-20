package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"os"
	"strings"
)

// resetAdminFactors is -reset-admin-factors HANDLE: the host break-glass that turns off an administrator's second
// factors (A-137; market.ResetAdminFactors), for example after a SETUP_TOKEN/APP_SECRET_KEY rotation left its TOTP
// secret unreadable. It reads nothing from stdin, connects with DATABASE_URL and starts no HTTP server, payment
// provider or background work. given reports which flags were set; args are the arguments after the flags.
// An optional -reset-admin-pgp-key-file replaces a lost key using public material only.
// Success output is the one-line summary of the change.
func resetAdminFactors(ctx context.Context, handle string, given func(string) bool, args []string, out io.Writer) error {
	return resetAdminFactorsWithPublicKeyFile(ctx, handle, "", given, args, out)
}

func resetAdminFactorsWithPublicKeyFile(ctx context.Context, handle, publicKeyFile string, given func(string) bool, args []string, out io.Writer) error {
	for _, other := range []string{"preview", "reset-admin-password", "bootstrap-admin", "site-name", "set-role"} {
		if given(other) {
			return errors.New("cannot be combined with -" + other)
		}
	}
	if handle == "" || len(args) > 0 {
		return errors.New("give the administrator's handle only: -reset-admin-factors HANDLE")
	}
	var public string
	if given("reset-admin-pgp-key-file") {
		var err error
		public, err = readRecoveryPublicKeyFile(publicKeyFile)
		if err != nil {
			return err
		}
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		summary, err := market.ResetAdminFactorsWithPublicKey(ctx, db, handle, public)
		if err != nil {
			return err
		}
		instruction := "Sign in again, then set up TOTP or PGP sign-in again. Mandatory PGP authentication still applies."
		if public != "" {
			instruction = "Sign in with your password and decrypt the challenge using the replacement private key, then set up TOTP again. Mandatory PGP authentication still applies."
		}
		fmt.Fprintln(out, summary+". "+instruction)
		return nil
	})
}

// readRecoveryPublicKeyFile bounds local input without echoing its contents. The
// market transaction validates public-only packets and current encryption usability.
func readRecoveryPublicKeyFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("give a public-key file: -reset-admin-pgp-key-file FILE")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read replacement PGP public-key file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("replacement PGP public-key file must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 || strings.TrimSpace(string(data)) == "" {
		return "", errors.New("replacement PGP public-key file must contain at most 16 KiB of public key material")
	}
	return string(data), nil
}
