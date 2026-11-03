package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"time"
)

// status is -status: it reads, without migrating or starting anything, whether first-run setup has completed
// and the highest applied migration, and prints exactly "installed=true|false schema=NNN keyroot=ROOT", where
// ROOT names the secret this environment derives the app's keys from (app_secret_key, or setup_token on a
// legacy install; market.KeyRootSource). Errors carry a class only; neither output ever contains DATABASE_URL
// or another secret. When active staff accounts have no second factor (A-706: staff actions are refused until
// they enrol), a count, never a handle, goes to warn: the installer parses the stdout line exactly. So does the
// warning that an installed market never enrolled retention, so its backups cannot reopen without it (A-772), and the
// fixed reason an outage's deadline recheck waits, for an operator whose panel shares the failed ingress (A-782).
func status(ctx context.Context, dsn string, getenv func(string) string, out, warn io.Writer) error {
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	installed, schema, staffWithoutFactor, retentionUnenrolled, recheckWaitWhy, err := market.Status(ctx, dsn)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(out, "installed=%t schema=%03d keyroot=%s\n", installed, schema, market.KeyRootSource(getenv)); err != nil {
		return err
	}
	if staffWithoutFactor > 0 {
		if _, err = fmt.Fprintf(warn, "warning: %d active staff account(s) have no second factor and cannot take staff actions until they turn on an authenticator or PGP sign-in; Admin > Users > Staff roster lists them\n", staffWithoutFactor); err != nil {
			return err
		}
	}
	if recheckWaitWhy != "" {
		if _, err = fmt.Fprintf(warn, "warning: an outage's deadline recheck is waiting: %s; its request stays pending and every hold stays in force\n", recheckWaitWhy); err != nil {
			return err
		}
	}
	if retentionUnenrolled {
		_, err = fmt.Fprintln(warn, "warning: this database has never enrolled retention, so a backup of it cannot reopen once the original database is lost: its restored copy refuses to start and no other path exists today. Enroll the current original database now, after reading docs/retention.md#before-enabling: set RETENTION_ENABLED=on in .env, run docker compose run --rm app -enroll-retention, then docker compose up -d, and keep the retention state apart from the database. Without Compose, see docs/retention.md#backups-of-a-never-enrolled-database.")
	}
	return err
}
