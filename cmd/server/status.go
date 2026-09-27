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
// or another secret.
func status(ctx context.Context, dsn string, getenv func(string) string, out io.Writer) error {
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	installed, schema, err := market.Status(ctx, dsn)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "installed=%t schema=%03d keyroot=%s\n", installed, schema, market.KeyRootSource(getenv))
	return err
}
