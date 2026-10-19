package market

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A-550: the Monero escrow market fee is host configuration only. No registered action reads a fee field, no settings
// row is read as the fee, the admin pages render no fee input, a post of fee fields to every administrator action
// leaves the fee in force and the next checkout's snapshot unchanged, and Admin → Payments shows the fee read-only with
// the operator command that changes it. DB integration against the simulated wallet RPC.
func TestXMRFeeHostOnly(t *testing.T) {
	// Static: no non-test source of the package reads a request field whose name mentions a fee, except the retired
	// Bitcoin handlers, which are never registered (retiredBitcoinRoute); and the fee record is used by recordXMRFee only.
	retired := map[string]string{"escrow_fee.go": "/admin/escrow-fee", "escrow_bump.go": "/escrow/bump"} // the Bitcoin market fee; a network fee bump
	for file, path := range retired {
		if _, ok := actions[path]; ok || !retiredBitcoinRoute(path) {
			t.Fatalf("%s (%s) is routable", path, file)
		}
	}
	files, err := filepath.Glob("internal/market/*.go") // TestMain runs the tests from the repository root
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seenUses := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		name = filepath.Base(name)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "xmrFeeSeenSetting" && name != "escrow_xmr_fee.go" {
				t.Errorf("%s: the fee record is used outside recordXMRFee: it is a record, never an input", fset.Position(id.Pos()))
			} else if ok && id.Name == "xmrFeeSeenSetting" {
				seenUses++
			}
			var lit *ast.BasicLit
			switch v := n.(type) {
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok || len(v.Args) == 0 || !map[string]bool{"Get": true, "Has": true, "FormValue": true, "PostFormValue": true}[sel.Sel.Name] {
					return true
				}
				lit, _ = v.Args[0].(*ast.BasicLit)
			case *ast.IndexExpr:
				if sel, ok := v.X.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Form" || sel.Sel.Name == "PostForm") {
					lit, _ = v.Index.(*ast.BasicLit)
				}
			}
			if lit != nil && lit.Kind == token.STRING && strings.Contains(strings.ToLower(lit.Value), "fee") && retired[name] == "" {
				t.Errorf("%s: reads request field %s: the market fee is set on the server only (A-550)", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	if len(files) < 100 {
		t.Fatalf("scanned %d source files", len(files))
	}
	if seenUses != 4 { // the declaration, lockSetting, the read and the write
		t.Errorf("xmrFeeSeenSetting is used %d times in escrow_xmr_fee.go, want 4", seenUses)
	}
	for path := range actions {
		if strings.Contains(strings.ToLower(path), "fee") {
			t.Errorf("registered action %s names a fee", path)
		}
	}

	fee := testXMRAddr("host fee", false)
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", fee)
	x.restart()
	_, admin := x.user("feeadmin_"+randomToken()[:6], "admin")
	agExec(x.testEnv, "UPDATE users SET last_authenticated_at=now()")

	// Every admin page renders no input, select or textarea whose name mentions a fee.
	feeInput := regexp.MustCompile(`(?is)<(input|select|textarea)\b[^>]*\bname="[^"]*fee[^"]*"`)
	for _, s := range adminSections {
		w := x.do("GET", s.URL(), admin, nil)
		x.check(w, 200)
		if m := feeInput.FindString(w.Body.String()); m != "" {
			t.Errorf("GET %s renders a fee field: %s", s.URL(), m)
		}
	}
	body := x.do("GET", "/admin/payments", admin, nil).Body.String()
	for _, want := range []string{`<h3 id="market-fee">Market fee</h3>`, "Set on the server; administrators cannot change it here.",
		"250 basis points (2.5% of the price)", `id="market-fee-address">` + fee + "</p>", "ESCROW_XMR_FEE_BPS", "ESCROW_XMR_FEE_ADDRESS",
		"./opsecmkt deploy redeploy --dir DIR --app-image IMAGE@sha256:DIGEST", "keys, endpoints and workers are kept"} {
		if !strings.Contains(body, want) {
			t.Errorf("Admin → Payments lacks %q", want)
		}
	}
	_, section, ok := strings.Cut(body, `id="market-fee"`)
	if section, _, _ = strings.Cut(section, "<h3 "); !ok || strings.Contains(section, "<form") || strings.Contains(section, "<input") {
		t.Fatal("the market fee section is missing or holds a form")
	}

	// A post carrying fee fields to every administrator action changes neither the fee in force nor any setting to
	// the injected values.
	evil := testXMRAddr("attacker fee", false)
	var paths []string
	for path, spec := range actions {
		for _, r := range spec.Roles {
			if r == "admin" {
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	form := url.Values{"password": {testPassword}, "fee_bps": {"9999"}, "fee_address": {evil}, "fee_destination": {evil},
		"ESCROW_XMR_FEE_BPS": {"9999"}, "ESCROW_XMR_FEE_ADDRESS": {evil}, "key": {xmrFeeSeenSetting}, "value": {"9999 " + evil}}
	for _, path := range paths {
		x.do("POST", path, admin, form)
	}
	if m := x.A.escrowXMR; m.feeBps != 250 || m.feeAddress != fee {
		t.Fatalf("fee in force after admin posts = %d %q", m.feeBps, m.feeAddress)
	}
	if n := agStrings(t, x.testEnv, "SELECT key FROM settings WHERE value LIKE '%'||$1||'%' OR value LIKE '9999%'", evil); len(n) > 0 {
		t.Fatalf("admin posts stored the injected fee in settings %v", n)
	}
	if got := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrFeeSeenSetting); len(got) != 1 || got[0] != "250 "+fee {
		t.Fatalf("fee record = %q", got)
	}
	x.mustPoll()
	order := x.draft(x.product)
	x.mustCheckout(order)
	var bps int64
	var dest string
	if err := x.DB.QueryRow("SELECT fee_bps,fee_destination FROM escrows WHERE order_id=$1", order).Scan(&bps, &dest); err != nil || bps != 250 || dest != fee {
		t.Fatalf("checkout snapshot = %d %q (%v), want the host fee", bps, dest, err)
	}
}

// A-550: each start compares the configured fee with the one last recorded. A change writes one audit row naming the
// old and new rate and address and one notice per administrator; an unchanged restart writes nothing; editing or
// restoring the record never changes the fee in force (it only makes the next start announce the host's value again).
// At 0 basis points Admin → Payments says no market fee is charged. DB integration against the simulated wallet RPC.
func TestXMRFeeChangeAnnouncedAtStart(t *testing.T) {
	x := newXMREscrowEnv(t) // the first start (fee blank) recorded 0 with no administrator to notify
	adminA, sessA := x.user("feeadm_a_"+randomToken()[:6], "admin")
	adminB, _ := x.user("feeadm_b_"+randomToken()[:6], "admin")
	suspended, _ := x.user("feeadm_s_"+randomToken()[:6], "admin")
	agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", suspended)
	audits := func() []string {
		return agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE action LIKE 'Monero escrow market fee changed%' ORDER BY id")
	}
	notices := func(uid string) []string {
		return agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'The Monero escrow market fee%' ORDER BY created,id", uid)
	}
	if got := audits(); len(got) != 1 || !strings.Contains(got[0], "none recorded before → no market fee (0 basis points)") {
		t.Fatalf("first start audit rows %q", got)
	}
	restartWith := func(bps, addr string) {
		t.Helper()
		t.Setenv("ESCROW_XMR_FEE_BPS", bps)
		t.Setenv("ESCROW_XMR_FEE_ADDRESS", addr)
		x.restart()
	}
	steady := func(step string, audit, perAdmin int) {
		t.Helper()
		if got := audits(); len(got) != audit {
			t.Fatalf("%s: %d fee audit rows, want %d: %q", step, len(got), audit, got)
		}
		for _, uid := range []string{adminA, adminB} {
			if got := notices(uid); len(got) != perAdmin {
				t.Fatalf("%s: administrator has %d fee notices, want %d", step, len(got), perAdmin)
			}
		}
		if got := notices(suspended); len(got) != 0 {
			t.Fatalf("%s: a suspended administrator was notified", step)
		}
	}

	restartWith("", "")
	steady("unchanged restart", 1, 0)
	page := x.do("GET", "/admin/payments", sessA, nil).Body.String()
	if !strings.Contains(page, "Rate: <strong>0 basis points</strong>. No market fee is charged") || strings.Contains(page, `id="market-fee-address"`) {
		t.Fatal("Admin → Payments does not say that no market fee is charged at 0 basis points")
	}

	first, second := testXMRAddr("fee one", false), testXMRAddr("fee two", true)
	restartWith("250", first)
	steady("first change", 2, 1)
	want := "no market fee (0 basis points) → 250 basis points (2.5%) paid to " + first
	if got := audits()[1]; !strings.Contains(got, want) {
		t.Fatalf("audit row %q lacks %q", got, want)
	}
	if got := notices(adminA)[0]; !strings.Contains(got, "it was no market fee (0 basis points); it is now 250 basis points (2.5%) paid to "+first) ||
		!strings.Contains(got, "Administrators cannot change the fee on /admin") {
		t.Fatalf("notice %q", got)
	}
	restartWith("250", first)
	restartWith("250", first)
	steady("unchanged restarts", 2, 1)

	restartWith("300", second)
	steady("second change", 3, 2)
	if got := audits()[2]; !strings.Contains(got, "250 basis points (2.5%) paid to "+first+" → 300 basis points (3%) paid to "+second) {
		t.Fatalf("audit row %q", got)
	}

	// A restored (older) or edited record is not the fee: the host value stays in force, and the next start announces
	// it once.
	agExec(x.testEnv, "UPDATE settings SET value=$2 WHERE key=$1", xmrFeeSeenSetting, "250 "+first)
	restartWith("300", second)
	steady("after a record rollback", 4, 3)
	if m := x.A.escrowXMR; m.feeBps != 300 || m.feeAddress != second {
		t.Fatalf("fee in force after a record rollback = %d %q", m.feeBps, m.feeAddress)
	}
	agExec(x.testEnv, "UPDATE settings SET value='not a record' WHERE key=$1", xmrFeeSeenSetting)
	restartWith("300", second)
	steady("after an unreadable record", 5, 4)
	if got := audits()[4]; !strings.Contains(got, "an unreadable record → 300 basis points (3%) paid to "+second) {
		t.Fatalf("audit row %q", got)
	}
	restartWith("300", second)
	steady("unchanged after the rollbacks", 5, 4)

	restartWith("0", "ignored at 0")
	steady("fee removed", 6, 5)
	if got := audits()[5]; !strings.Contains(got, "300 basis points (3%) paid to "+second+" → no market fee (0 basis points)") {
		t.Fatalf("audit row %q", got)
	}
	if got := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrFeeSeenSetting); len(got) != 1 || got[0] != strconv.Itoa(0)+" " {
		t.Fatalf("fee record at 0 = %q", got)
	}
}
