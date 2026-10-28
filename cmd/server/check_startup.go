package main

import (
	"context"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"time"
)

// checkStartupLimit bounds serve --check-startup as a whole, inside the 60 seconds the deploy commands give its
// one-off container, so the process ends by itself even when the docker client waiting for it was stopped.
const checkStartupLimit = 45 * time.Second

// checkStartup is serve --check-startup (A-694): the listener settings main checks before New, then
// market.CheckStartup. It prints a line per check to out and returns the first refusal, with the server's own
// message; it migrates, serves and writes nothing.
func checkStartup(ctx context.Context, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, checkStartupLimit)
	defer cancel()
	if err := checkListenerSettings(); err != nil {
		return err
	}
	if err := market.CheckStartup(ctx, out); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("the startup check did not finish within %s (%w); check DATABASE_URL and service health", checkStartupLimit, err)
		}
		return err
	}
	return nil
}
