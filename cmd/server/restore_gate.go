package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"os"
	"strings"
)

// restoreGateExclusive are the host command flags the restore gate commands cannot be combined with.
var restoreGateExclusive = []string{"preview", "reset-admin-password", "reset-admin-factors", "reset-admin-pgp-key-file", "set-role", "bootstrap-admin", "site-name"}

// reconciliationSteps are the steps of docs/testnet-runbook.md ("Reconcile a restored database before enabling
// payouts") that -clear-restore-gate asks the operator to confirm, one by one, before it clears the gate. That the
// application is stopped is checked, not asked.
var reconciliationSteps = []string{
	"Public access stayed stopped, and the restored market ran only on the protected administrator connection with the current external retention state (runbook steps 1 to 4).",
	"The Monero wallet sweep finished: Admin shows no escrow wallet missing from disk and no unresolved wallet or balance (step 5).",
	"Every held settlement's transaction and key images were checked in the daemon and its transfer history in the wallet, and each was resolved on Admin (step 6).",
	"Orders funded or settled after the backup, balances and transaction history were checked, and an encrypted backup of the reconciled state was made (step 7).",
	"The Admin settings and vendor bonds rolled back by the restore were re-checked against the operator record, and the host fee address and rate confirmed (step 7).",
}

// setRestoreGate is -set-restore-gate: the protection of UPGRADING.md section 6 for a database restored without
// scripts/restore.sh (market.SetRestoreGate). Any operator who can run the binary with DATABASE_URL may set the gate;
// it reads no stdin, starts no HTTP server, payment provider or background work, and refuses while the application
// serves the database. Success output is counts only.
func setRestoreGate(ctx context.Context, given func(string) bool, args []string, out io.Writer) error {
	for _, other := range append([]string{"clear-restore-gate"}, restoreGateExclusive...) {
		if given(other) {
			return errors.New("cannot be combined with -" + other)
		}
	}
	if len(args) > 0 {
		return errors.New("-set-restore-gate takes no arguments")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		summary, err := market.SetRestoreGate(ctx, db)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, summary+". Reconcile the restored database (docs/testnet-runbook.md), then clear the gate with -clear-restore-gate HANDLE.")
		return nil
	})
}

// clearRestoreGate is -clear-restore-gate HANDLE: the operator's assertion, in an administrator's name, that the
// restored database was reconciled with the wallets (market.ClearRestoreGate; A-505, A-29: a host action, never a web
// one). It needs a terminal on stdin (terminal): there is no flag that answers for the operator. It checks the handle,
// the gate and that the application is stopped, asks for each of reconciliationSteps on prompt (each must be answered
// "yes"), then clears the gate in one audited transaction that checks everything again. Anything else changes nothing.
func clearRestoreGate(ctx context.Context, handle string, given func(string) bool, args []string, in io.Reader, terminal bool, out, prompt io.Writer) error {
	for _, other := range append([]string{"set-restore-gate"}, restoreGateExclusive...) {
		if given(other) {
			return errors.New("cannot be combined with -" + other)
		}
	}
	if handle == "" || len(args) > 0 {
		return errors.New("give the administrator's handle only: -clear-restore-gate HANDLE")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	if !terminal {
		return errors.New("the reconciliation steps are confirmed in a terminal, and stdin is not one; run it in a terminal " +
			"(docker compose run --rm app -clear-restore-gate HANDLE allocates one); nothing was changed")
	}
	var note string
	check := func(ctx context.Context, db *sql.DB) (err error) {
		note, err = market.ClearRestoreGate(ctx, db, handle, false)
		return err
	}
	if err := withHostDB(ctx, dsn, check); err != nil {
		return err
	}
	if note != "" {
		fmt.Fprintln(prompt, note)
	}
	fmt.Fprintln(prompt, "Clearing the restore gate asserts that reconciliation is complete (docs/testnet-runbook.md). Confirm each step:")
	lines := bufio.NewReader(io.LimitReader(in, 4096))
	for i, step := range reconciliationSteps {
		fmt.Fprintf(prompt, "%d. %s\n   Type yes if this is done: ", i+1, step)
		line, err := lines.ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			if err != nil && err != io.EOF {
				return err
			}
			return fmt.Errorf("reconciliation step %d not confirmed; finish it and run this again; nothing was changed", i+1)
		}
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		summary, err := market.ClearRestoreGate(ctx, db, handle, true)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, summary+". Start the application and verify Admin readiness before reopening public access.")
		return nil
	})
}
