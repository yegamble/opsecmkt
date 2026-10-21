package main

import (
	"bytes"
	"context"
	"database/sql"
	"opsecmkt/internal/market"
	"strings"
	"testing"
)

// Invocation mistakes are refused before stdin is read or the database is touched.
func TestRestoreGateRefusesBadInvocation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	for _, tc := range []struct {
		clear    bool
		handle   string
		flags    []string
		args     []string
		terminal bool
		dsn      bool
		msg      string
	}{
		{flags: []string{"set-restore-gate", "clear-restore-gate"}, dsn: true, msg: "-clear-restore-gate"},
		{flags: []string{"set-restore-gate", "set-role"}, dsn: true, msg: "-set-role"},
		{flags: []string{"set-restore-gate", "preview"}, dsn: true, msg: "-preview"},
		{flags: []string{"set-restore-gate"}, args: []string{"extra"}, dsn: true, msg: "takes no arguments"},
		{flags: []string{"set-restore-gate"}, msg: "DATABASE_URL required"},
		{clear: true, handle: "root_admin", flags: []string{"clear-restore-gate", "set-restore-gate"}, terminal: true, dsn: true, msg: "-set-restore-gate"},
		{clear: true, handle: "root_admin", flags: []string{"clear-restore-gate", "reset-admin-factors"}, terminal: true, dsn: true, msg: "-reset-admin-factors"},
		{clear: true, flags: []string{"clear-restore-gate"}, terminal: true, dsn: true, msg: "-clear-restore-gate HANDLE"},
		{clear: true, handle: "root_admin", flags: []string{"clear-restore-gate"}, args: []string{"yes"}, terminal: true, dsn: true, msg: "-clear-restore-gate HANDLE"},
		{clear: true, handle: "root_admin", flags: []string{"clear-restore-gate"}, terminal: true, msg: "DATABASE_URL required"},
		{clear: true, handle: "root_admin", flags: []string{"clear-restore-gate"}, dsn: true, msg: "stdin is not one"},
	} {
		if !tc.dsn {
			t.Setenv("DATABASE_URL", "")
		}
		var out, prompt bytes.Buffer
		var err error
		if tc.clear {
			err = clearRestoreGate(context.Background(), tc.handle, given(tc.flags...), tc.args, strings.NewReader(strings.Repeat("yes\n", 5)), tc.terminal, &out, &prompt)
		} else {
			err = setRestoreGate(context.Background(), given(tc.flags...), tc.args, &out)
		}
		if err == nil || !strings.Contains(err.Error(), tc.msg) || out.Len() != 0 || prompt.Len() != 0 {
			t.Fatalf("%+v: err %v out %q prompt %q", tc, err, out.String(), prompt.String())
		}
		t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	}
}

// restoreGateState is the gate, the other restore settings, the Monero escrow fixture's states and the audit rows.
func restoreGateState(t *testing.T, db *sql.DB) (gate, others, xmr string, audits []string) {
	t.Helper()
	if err := db.QueryRow(`SELECT COALESCE((SELECT value FROM settings WHERE key='payments_recovery_required'),'-'),
		COALESCE((SELECT string_agg(key||'='||value, ',' ORDER BY key) FROM settings WHERE key IN ('retention_restore_required','escrow_xmr_release_recheck','xmr_escrow_sweep_required')),'-'),
		(SELECT string_agg(state, ',' ORDER BY state) FROM xmr_escrow_wallets)||'/'||(SELECT string_agg(state, ',' ORDER BY state) FROM xmr_escrow_slots)||'/'||(SELECT string_agg(state, ',' ORDER BY state) FROM xmr_bond_wallets)`).Scan(&gate, &others, &xmr); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT COALESCE(user_id,'-')||':'||action FROM audit_events WHERE action LIKE 'Payout recovery gate%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err = rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, a)
	}
	return gate, others, xmr, audits
}

// -set-restore-gate and -clear-restore-gate HANDLE against a real database (A-505): the protection of UPGRADING.md
// section 6 and its audit row; refusal while the application serves the database; a clear refused for an unknown or
// non-administrator handle, without a terminal and without every reconciliation step confirmed, each changing nothing;
// the audited clear; and both run again.
func TestRestoreGateCLI(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	addr := func(c string) string { return strings.Repeat(c, 95) }
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role) VALUES('a1','root_admin','x','admin'),('v1','some_vendor','x','vendor')",
		"INSERT INTO xmr_escrow_wallets(name,month,primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at,state) VALUES('escrow-202610-01','2026-10-01','" + addr("A") + "',1,'sealed','" + strings.Repeat("A", 40) + "',now(),'active')",
		"INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) SELECT id,1,'" + addr("B") + "' FROM xmr_escrow_wallets",
		"INSERT INTO xmr_bond_wallets(name,primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at,state) VALUES('bond-01','" + addr("C") + "',1,'sealed','" + strings.Repeat("A", 40) + "',now(),'sealed')",
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	gate0, others0, xmr0, audits0 := restoreGateState(t, db)
	if gate0 == "true" || xmr0 != "active/free/sealed" || len(audits0) != 0 {
		t.Fatalf("fixture: gate %s xmr %s audits %v", gate0, xmr0, audits0)
	}
	unchanged := func(when string, gate, others, xmr string, audits []string) {
		t.Helper()
		g, o, x, a := restoreGateState(t, db)
		if g != gate || o != others || x != xmr || strings.Join(a, "|") != strings.Join(audits, "|") {
			t.Fatalf("%s changed the database: gate %s→%s, settings %s→%s, xmr %s→%s, audits %v→%v", when, gate, g, others, o, xmr, x, audits, a)
		}
	}

	clear := func(handle, answers string) (string, string, error) {
		var out, prompt bytes.Buffer
		err := clearRestoreGate(context.Background(), handle, given("clear-restore-gate"), nil, strings.NewReader(answers), true, &out, &prompt)
		return out.String(), prompt.String(), err
	}
	all := strings.Repeat("yes\n", len(reconciliationSteps))

	// While an application serves the database (market.New holds the serving lock until Close), both commands refuse
	// and change nothing; the clear is refused before any question is asked. (Once the gate is set, the application
	// would not start here without the retention state the set also requires.)
	t.Setenv("SETUP_TOKEN", "integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef") // market.New needs a key root; the commands read none
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runServer(t, nil, "-set-restore-gate")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "set-restore-gate: the application is running against this database") {
		app.Close()
		t.Fatalf("set while serving: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	out, prompt, err := clear("root_admin", all)
	app.Close()
	if err == nil || !strings.Contains(err.Error(), "the application is running") || out != "" || prompt != "" {
		t.Fatalf("clear while serving: err %v out %q prompt %q", err, out, prompt)
	}
	unchanged("set and clear while serving", gate0, others0, xmr0, nil)

	// -set-restore-gate applies the protection with one audit row and no user.
	stdout, stderr, code = runServer(t, nil, "-set-restore-gate")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "Restore gate set: ") ||
		!strings.Contains(stdout, "held 0 restored Monero escrow settlement(s), retired 1 free escrow account(s), moved 1 escrow wallet(s) to closing and closed 1 vendor bond wallet(s); audited.") {
		t.Fatalf("set: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	setAudit := "-:Payout recovery gate set by host command (-set-restore-gate); all outbound payouts paused until an administrator clears the gate"
	gate, others, xmr, audits := restoreGateState(t, db)
	if gate != "true" || others != "escrow_xmr_release_recheck=true,retention_restore_required=true,xmr_escrow_sweep_required=true" ||
		xmr != "closing/retired/closed" || strings.Join(audits, "|") != setAudit {
		t.Fatalf("after set: gate %s settings %s xmr %s audits %v", gate, others, xmr, audits)
	}
	// Set again: the same state, counted as nothing new, and one more audit row.
	stdout, _, code = runServer(t, nil, "-set-restore-gate")
	if code != 0 || !strings.Contains(stdout, "held 0 restored Monero escrow settlement(s), retired 0 free escrow account(s), moved 0 escrow wallet(s) to closing and closed 0 vendor bond wallet(s)") {
		t.Fatalf("set again: code %d stdout %q", code, stdout)
	}
	audits = append(audits, setAudit)
	unchanged("set again", gate, others, xmr, audits)
	if _, stderr, code = runServer(t, nil, "-set-restore-gate=false"); code != 2 || !strings.Contains(stderr, "without =false") {
		t.Fatalf("-set-restore-gate=false: code %d stderr %q", code, stderr)
	}

	// The binary with stdin that is not a terminal refuses the clear: no flag answers for the operator.
	stdout, stderr, code = runServer(t, nil, "-clear-restore-gate", "root_admin")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "stdin is not one") {
		t.Fatalf("clear without a terminal: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	unchanged("clear without a terminal", gate, others, xmr, audits)

	// An unknown, a differently cased or a non-administrator handle is refused before any question is asked.
	for _, handle := range []string{"nobody_here", "Root_admin", "some_vendor"} {
		out, prompt, err := clear(handle, all)
		if err == nil || !strings.Contains(err.Error(), "no administrator has the handle") || out != "" || prompt != "" {
			t.Fatalf("clear for %s: err %v out %q prompt %q", handle, err, out, prompt)
		}
		unchanged("clear for "+handle, gate, others, xmr, audits)
	}
	// Every step must be answered "yes": a "no", another word, or stdin ending early refuses at that step.
	for answers, step := range map[string]string{
		"yes\nyes\nno\nyes\nyes\n": "step 3",
		"y\n":                      "step 1",
		"yes\nyes\nyes\nyes\n":     "step 5",
		"":                         "step 1",
	} {
		out, prompt, err := clear("root_admin", answers)
		if err == nil || !strings.Contains(err.Error(), "reconciliation "+step+" not confirmed") || out != "" || !strings.Contains(prompt, "1. Public access stayed stopped") {
			t.Fatalf("answers %q: err %v out %q prompt %q", answers, err, out, prompt)
		}
		unchanged("unconfirmed clear", gate, others, xmr, audits)
	}

	// All confirmed: the gate is cleared, audited on the administrator; the other settings and holds stay.
	out, prompt, err = clear("root_admin", all)
	if err != nil || out != "Restore gate cleared by administrator root_admin; audited. Start the application and verify Admin readiness before reopening public access.\n" ||
		strings.Count(prompt, "Type yes if this is done: ") != len(reconciliationSteps) {
		t.Fatalf("clear: err %v out %q prompt %q", err, out, prompt)
	}
	audits = append(audits, "a1:Payout recovery gate cleared by root_admin by host command after reconciling the restored database with the wallets")
	unchanged("the clear (checked against the expected state)", "false", others, xmr, audits)
	// Clearing again is refused: the gate is not set.
	out, prompt, err = clear("root_admin", all)
	if err == nil || !strings.Contains(err.Error(), "the restore gate is not set; nothing was changed") || out != "" || prompt != "" {
		t.Fatalf("clear again: err %v out %q prompt %q", err, out, prompt)
	}
	unchanged("clear again", "false", others, xmr, audits)
	for _, s := range []string{stdout, stderr, out, prompt} {
		if strings.Contains(s, "postgres://") || strings.Contains(s, dsn) {
			t.Fatalf("output names the database URL: %q", s)
		}
	}
}
