package operator

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"
)

// startupCheckTimeout bounds the one-off application run of the startup check (A-694). serve --check-startup
// ends itself within 45 seconds; this also covers creating and removing the container.
var startupCheckTimeout = 60 * time.Second

// startCheckFlag registers doctor's --start-check; other commands get nil.
func startCheckFlag(command string, flags *flag.FlagSet) *bool {
	if command != "doctor" {
		return nil
	}
	return flags.Bool("start-check", false, "also run the app image once (serve --check-startup, no dependencies) and print why it would not start; migrates, serves and keeps nothing")
}

// runStartupCheck runs the app image once as `compose run --rm --no-deps -T app serve --check-startup`: no
// dependency service is started, no port is published, the container is removed when it exits, and the production
// app's logging driver (none) keeps nothing. Its output goes through dockerCall's redaction of every configured
// secret value and URL credential, as deploy logs does in development. It fails when the app would not start.
func runStartupCheck(ctx context.Context, dir string, cfg Config, out, errOut io.Writer) error {
	if _, err := privateFile(filepath.Join(dir, "compose.json"), 2<<20); err != nil {
		return errors.New("runtime configuration is missing; run deploy install first")
	}
	ctx, cancel := context.WithTimeout(ctx, startupCheckTimeout)
	defer cancel()
	if err := dockerCall(ctx, dir, cfg, out, errOut, "run", "--rm", "--no-deps", "-T", "app", "serve", "--check-startup"); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("the startup check did not finish within %s (deadline exceeded); configuration and volumes were retained", startupCheckTimeout)
		}
		return errors.New("the startup check says the application would not start; the refusal is printed above; configuration and volumes were retained")
	}
	return nil
}

// explainStartFailure follows a production up or redeploy whose wait failed (startErr): the app container keeps no
// logs, so the startup check runs once to print the cause. The original failure is still returned.
func explainStartFailure(ctx context.Context, dir string, cfg Config, out, errOut io.Writer, startErr error) error {
	if cfg.Mode != "prod" || ctx.Err() != nil {
		return startErr
	}
	_, _ = fmt.Fprintln(out, "The application did not become ready. Production keeps no container logs; running its startup check once:")
	if err := runStartupCheck(ctx, dir, cfg, out, errOut); err != nil {
		return fmt.Errorf("%w; %v", startErr, err)
	}
	return fmt.Errorf("%w; the startup check found no refusal, so the cause is a step it does not run (applying pending migrations within MIGRATION_TIMEOUT, wallet RPC, the scanner, listeners) or readiness; see deploy status", startErr)
}
