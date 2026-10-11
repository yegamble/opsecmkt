package market

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// CheckBootstrapInput applies first-run setup's handle and marketplace-name rules, so the host tool refuses a
// mistake before it reads the password. BootstrapAdmin applies them again. The handle is taken exactly (no
// trimming), since the operator signs in with exactly what was given.
func CheckBootstrapInput(handle, siteName string) error {
	if !handlePattern.MatchString(handle) {
		return errors.New("use a 3–32 character handle (letters, digits, underscores)")
	}
	if _, err := setupSiteName(siteName); err != nil {
		return errors.New("use a marketplace name of at most 80 characters")
	}
	return nil
}

// BootstrapAdmin is first-run setup from the host (cmd/server -bootstrap-admin), for an operator who can run
// the binary with DATABASE_URL; it needs no setup token and has no web path (D-26). It applies /setup's handle,
// password and marketplace-name rules, hashes at bcryptCost and then, in one transaction under the lock /setup
// and startup take, refuses a database without the application schema or upgraded by a newer release, and runs
// installFirstAdmin: the market is installed with this one administrator exactly as POST /setup would do, or,
// when it is already installed, nothing changes. It creates no session.
func BootstrapAdmin(ctx context.Context, db *sql.DB, handle, password, siteName string) error {
	if err := CheckBootstrapInput(handle, siteName); err != nil {
		return err
	}
	if !validPassword(password) {
		return errors.New("use a password of 12–72 bytes")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return bootstrapFailed(ctx, err)
	}
	defer tx.Rollback()
	if err = lockKnownSchema(ctx, tx); err != nil {
		return bootstrapFailed(ctx, err)
	}
	if _, err = installFirstAdmin(ctx, tx, handle, hash, siteName); err != nil {
		return bootstrapFailed(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return errors.New("saving the setup failed; run the command again (it refuses if setup did complete)")
	}
	return nil
}

// bootstrapFailed turns a failure before commit into the operator's message; the transaction is rolled back,
// so nothing was changed.
func bootstrapFailed(ctx context.Context, err error) error {
	var he *httpError
	var ce *pgconn.ConnectError
	switch {
	case errors.Is(err, errSetupComplete):
		return errors.New("this market is already set up; nothing was changed. Sign in as its administrator (a forgotten administrator password is reset with -reset-admin-password)")
	case ctx.Err() != nil:
		return errors.New("timed out waiting for the database; another server instance may be migrating or setting up the same database. Nothing was changed; try again")
	case errors.As(err, &ce): // its text names the DATABASE_URL user and database
		return errors.New("database connection failed: " + connectErrorClass(err) + "; nothing was changed")
	case errors.As(err, &he):
		return errors.New("setup failed: " + he.Msg + "; nothing was changed")
	}
	return err
}
