package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"opsecmkt/internal/market"
	"os"
	"path/filepath"
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
	// A-720: the operator is asked about, and scripts/restore.sh names, the orders around the backup point and the
	// vendors' grace.
	restoreScript, err := os.ReadFile("scripts/restore.sh") // cliSchema runs from the repository root
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"the reconciliation steps": strings.Join(reconciliationSteps, "\n"), "scripts/restore.sh": string(restoreScript)} {
		for _, want := range []string{"shipped, delivered or cancelled around the backup point", "72 hours", "re-ship or re-deliver"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s do not say %q", name, want)
			}
		}
	}
	// A-729: public access is stopped while the operator answers, so the attestation asks for what can be done then: a
	// review as far as the evidence goes, and telling the vendors once public access returns. It never asks for a
	// conversation with a party that needs that access. The number of steps is fixed by scripts/test-backup-restore.sh.
	if len(reconciliationSteps) != 5 {
		t.Errorf("%d reconciliation steps; scripts/test-backup-restore.sh answers five", len(reconciliationSteps))
	}
	steps := strings.Join(reconciliationSteps, "\n")
	for _, want := range []string{"as far as the evidence allows", "you will tell vendors once public access returns", "not to ship an order whose refund the wallet history shows"} {
		if !strings.Contains(steps, want) {
			t.Errorf("the reconciliation steps do not say %q", want)
		}
	}
	for _, refused := range []string{"were told", "with the parties"} {
		if strings.Contains(steps, refused) {
			t.Errorf("the reconciliation steps ask for %q while public access is stopped", refused)
		}
	}
	if !strings.Contains(string(restoreScript), "no form corrects the record") {
		t.Error("scripts/restore.sh does not say that no form corrects the record")
	}

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
	if err == nil || !strings.Contains(err.Error(), "the application is running against this database (or is starting): 1 database session(s) hold the serving lock (session start, UTC: 20") || out != "" || prompt != "" {
		t.Fatalf("clear while serving: err %v out %q prompt %q", err, out, prompt)
	}
	unchanged("set and clear while serving", gate0, others0, xmr0, nil)

	// -set-restore-gate applies the protection with one audit row and no user. A backup taken while an outage's recheck was
	// pending carries that request: the restore overwrites it, so the recheck says a restore (A-730).
	if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('escrow_xmr_release_recheck','outage') ON CONFLICT (key) DO UPDATE SET value='outage'"); err != nil {
		t.Fatal(err)
	}
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
	// Right after the set no application has started on the database (its startup replays the retention state and
	// clears retention_restore_required): the clear is refused before any question is asked.
	if out, prompt, err := clear("root_admin", all); err == nil || !strings.Contains(err.Error(), "the application has not started on this restored database yet") || out != "" || prompt != "" {
		t.Fatalf("clear before the first start: err %v out %q prompt %q", err, out, prompt)
	}
	unchanged("clear before the first start", gate, others, xmr, audits)
	if _, err = db.Exec("UPDATE settings SET value='false' WHERE key='retention_restore_required'"); err != nil {
		t.Fatal(err)
	}
	others = strings.Replace(others, "retention_restore_required=true", "retention_restore_required=false", 1)

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
		strings.Count(prompt, "Type yes if this is done: ") != len(reconciliationSteps) || !strings.HasPrefix(prompt, "Note: the Monero escrow restore sweep has not completed") {
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

// -request-deadline-recheck's invocation mistakes are refused before the database is touched (OPS104-1).
func TestRestoreGateRequestRecheckRefusesBadInvocation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	for _, tc := range []struct {
		flags []string
		args  []string
		msg   string
	}{
		{flags: []string{"request-deadline-recheck", "set-restore-gate"}, msg: "-set-restore-gate"},
		{flags: []string{"request-deadline-recheck", "clear-restore-gate"}, msg: "-clear-restore-gate"},
		{flags: []string{"request-deadline-recheck", "preview"}, msg: "-preview"},
		{flags: []string{"request-deadline-recheck", "set-role"}, msg: "-set-role"},
		{flags: []string{"request-deadline-recheck"}, args: []string{"extra"}, msg: "takes no arguments"},
	} {
		var out bytes.Buffer
		err := requestDeadlineRecheck(context.Background(), given(tc.flags...), tc.args, &out)
		if err == nil || !strings.Contains(err.Error(), tc.msg) || out.Len() != 0 {
			t.Fatalf("%+v: err %v out %q", tc, err, out.String())
		}
	}
	// Each other restore gate command refuses the new flag as well.
	var out, prompt bytes.Buffer
	if err := setRestoreGate(context.Background(), given("set-restore-gate", "request-deadline-recheck"), nil, &out); err == nil || !strings.Contains(err.Error(), "-request-deadline-recheck") {
		t.Fatalf("set with the new flag: %v", err)
	}
	if err := clearRestoreGate(context.Background(), "root_admin", given("clear-restore-gate", "request-deadline-recheck"), nil, strings.NewReader(""), true, &out, &prompt); err == nil || !strings.Contains(err.Error(), "-request-deadline-recheck") {
		t.Fatalf("clear with the new flag: %v", err)
	}
	t.Setenv("DATABASE_URL", "")
	if err := requestDeadlineRecheck(context.Background(), given("request-deadline-recheck"), nil, &out); err == nil || !strings.Contains(err.Error(), "DATABASE_URL required") || out.Len() != 0 {
		t.Fatalf("no database: err %v out %q", err, out.String())
	}
	for _, args := range [][]string{
		{"-request-deadline-recheck", "-set-role", "root_admin", "buyer"}, {"-request-deadline-recheck", "-status"}, {"-request-deadline-recheck", "-healthcheck"},
		{"-request-deadline-recheck", "-check-startup"}, {"-request-deadline-recheck", "-enroll-retention"}, {"-request-deadline-recheck", "-export-audit", "1"},
	} {
		stdout, stderr, code := runServer(t, []string{"DATABASE_URL=postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"}, args...)
		if code == 0 || stdout != "" || !strings.Contains(stderr, "combined") {
			t.Fatalf("%v: code %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
	if _, stderr, code := runServer(t, nil, "-request-deadline-recheck=false"); code != 2 || !strings.Contains(stderr, "without =false") {
		t.Fatalf("=false: code %d stderr %q", code, stderr)
	}
}

// -request-deadline-recheck against a real database (OPS104-1): it asks for the outage's recheck with the application
// running, from DATABASE_URL or DATABASE_URL_FILE; says so when one is already pending, an outage's or a restore's; refuses
// under the restore gate; a new request adds one audit row with no actor in the request's transaction and changes nothing
// else, anything else adds none (PM-3), and it prints no connection detail.
func TestRestoreGateRequestRecheckCLI(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const key = "escrow_xmr_release_recheck"
	settingsNow := func() string {
		t.Helper()
		var s string
		if err := db.QueryRow(`SELECT COALESCE((SELECT string_agg(key||'='||value, ',' ORDER BY key) FROM settings),'')||'/'||(SELECT count(*) FROM audit_events)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	settingsOnly := func() string {
		t.Helper()
		var s string
		if err := db.QueryRow(`SELECT COALESCE((SELECT string_agg(key||'='||value, ',' ORDER BY key) FROM settings),'')`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	const requestRow = "Monero deadline recheck requested from the host"
	auditRows := func() (rows []string) {
		t.Helper()
		r, err := db.Query(`SELECT COALESCE(user_id,'-')||':'||action FROM audit_events ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var row string
			if err := r.Scan(&row); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		}
		return rows
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	value := func() string {
		t.Helper()
		var v sql.NullString
		if err := db.QueryRow("SELECT (SELECT value FROM settings WHERE key=$1)", key).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.String
	}
	noURL := func(s string) {
		t.Helper()
		if strings.Contains(s, "postgres://") || strings.Contains(s, dsn) {
			t.Fatalf("output names the database URL: %q", s)
		}
	}

	// None pending, while an application serves the database (market.New holds the serving lock until Close): now pending.
	t.Setenv("SETUP_TOKEN", "integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef")
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	exec("DELETE FROM settings WHERE key=$1", key)
	before, auditBefore := settingsOnly(), len(auditRows())
	stdout, stderr, code := runServer(t, nil, "-request-deadline-recheck")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "Deadline recheck now pending (outage).") || !strings.Contains(stdout, "cannot undo a completion") || !strings.Contains(stdout, "within every 72 hours") || !strings.Contains(stdout, "Confirm on Admin > Payments") {
		t.Fatalf("none pending: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A-761: it says to run it once more on return, and does not promise a next pass with Monero escrow off. DA108-2: it states
	// the hold on cancels, which a recheck that moves no deadline places too. QA108-2: and that each repeat notifies again.
	for _, want := range []string{"run this once more when public access returns", "With Monero escrow on, the watcher's next pass", "With ESCROW_XMR off the request stays pending until the first pass after it is on again", "a buyer's no-ship cancel past its time waits until then",
		"also holds a buyer's no-ship cancel for 72 hours, even if it moves no deadline", "Each repeat notifies every buyer and vendor it moves again"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the output lacks %q: %q", want, stdout)
		}
	}
	noURL(stdout + stderr)
	if value() != "outage" {
		t.Fatalf("the request wrote %q", value())
	}
	if rows := auditRows(); len(rows) != auditBefore+1 || rows[len(rows)-1] != "-:"+requestRow {
		t.Fatalf("the request's audit rows: %q, %d before", rows, auditBefore)
	}
	exec("DELETE FROM settings WHERE key=$1", key)
	if settingsOnly() != before {
		t.Fatalf("the request changed more than its key: %s, was %s", settingsOnly(), before)
	}
	exec("INSERT INTO settings(key,value) VALUES($1,'outage')", key)
	wrote := settingsNow()

	// An outage's already pending: said so, nothing changed.
	stdout, stderr, code = runServer(t, nil, "-request-deadline-recheck")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "Deadline recheck already pending (outage); nothing was changed.") {
		t.Fatalf("outage pending: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	noURL(stdout + stderr)
	if settingsNow() != wrote {
		t.Fatalf("settings %s, was %s", settingsNow(), wrote)
	}

	// A restore's pending keeps saying a restore: an outage's value must not replace it. Any other value is a restore's.
	for _, restore := range []string{"true", "outgae"} {
		exec("UPDATE settings SET value=$2 WHERE key=$1", key, restore)
		held := settingsNow()
		stdout, stderr, code = runServer(t, nil, "-request-deadline-recheck")
		if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "A restore's deadline recheck is already pending; nothing was changed.") {
			t.Fatalf("value %s pending: code %d stdout %q stderr %q", restore, code, stdout, stderr)
		}
		noURL(stdout + stderr)
		if settingsNow() != held {
			t.Fatalf("settings %s, was %s", settingsNow(), held)
		}
	}

	// The restore gate, with or without a request: refused, and nothing written.
	exec("INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	for _, withRequest := range []bool{true, false} {
		if !withRequest {
			exec("DELETE FROM settings WHERE key=$1", key)
		}
		held := settingsNow()
		stdout, stderr, code = runServer(t, nil, "-request-deadline-recheck")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "request-deadline-recheck: the restore gate is set") || !strings.Contains(stderr, "nothing was changed") {
			t.Fatalf("under the gate: code %d stdout %q stderr %q", code, stdout, stderr)
		}
		noURL(stdout + stderr)
		if settingsNow() != held {
			t.Fatalf("settings %s, was %s", settingsNow(), held)
		}
	}
	// A gate that was cleared (the value false) stops nothing. From DATABASE_URL_FILE, as a native install runs it.
	exec("UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
	file := filepath.Join(t.TempDir(), "database.url")
	if err = os.WriteFile(file, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	auditBefore = len(auditRows())
	stdout, stderr, code = runServer(t, []string{"DATABASE_URL=", "DATABASE_URL_FILE=" + file}, "-request-deadline-recheck")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "Deadline recheck now pending (outage).") || value() != "outage" {
		t.Fatalf("from the file: code %d stdout %q stderr %q value %q", code, stdout, stderr, value())
	}
	noURL(stdout + stderr)
	if rows := auditRows(); len(rows) != auditBefore+1 || rows[len(rows)-1] != "-:"+requestRow {
		t.Fatalf("the audit rows from the file: %q, %d before", rows, auditBefore)
	}

	// PM-3: one transaction. An audit row that cannot be written leaves no request, and the failure names no connection detail.
	exec("DELETE FROM settings WHERE key=$1", key)
	exec(`CREATE FUNCTION refuse_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$`)
	exec("CREATE TRIGGER refuse_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_audit()")
	held := settingsNow()
	stdout, stderr, code = runServer(t, nil, "-request-deadline-recheck")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "database statement failed") || settingsNow() != held || value() != "" {
		t.Fatalf("an audit row refused: code %d stdout %q stderr %q value %q", code, stdout, stderr, value())
	}
	noURL(stdout + stderr)
}

// OPS105-N1: a failed statement is reported by class, naming no user, database or host of the URL. A search path with no
// settings table makes the statement fail after the connection check succeeded.
func TestRestoreGateRequestRecheckErrorIsClassOnly(t *testing.T) {
	u, err := url.Parse(cliSchema(t))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", "no_such_schema")
	u.RawQuery = q.Encode()
	stdout, stderr, code := runServer(t, []string{"DATABASE_URL=" + u.String()}, "-request-deadline-recheck")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "database statement failed (42P01)") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, private := range []string{u.User.Username(), strings.TrimPrefix(u.Path, "/"), u.Hostname(), "settings", "does not exist"} {
		if strings.Contains(stderr, private) {
			t.Fatalf("the error names %q: %q", private, stderr)
		}
	}
}
