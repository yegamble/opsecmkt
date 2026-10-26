package market

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-652a (PAY87-7): moderation never moves money (AGENTS.md; docs/acceptance.md "Moderation cannot move money, change
// an order, ban an account or hide a listing"). TestModerationNeverMovesMoney scans the moderation layer, as
// TestModerationNeverWritesReviewState does: the engine and model path (internal/moderation), the worker, private panel,
// budget and offline review in internal/market (moderation*.go), the worker's launcher (cmd/server/moderation*.go) and
// the panel's templates. None of it may name a money or ban route, a settlement or refund address, the state machine,
// the ban refund, a wallet send, or anything the money files declare; the engine imports no database package, and the
// rest writes only moderation's own tables and the two below.
//
// Reads stay allowed, deliberately: the worker snapshots the listing, dispute or payment flag it scores (products,
// disputes, orders and payments, read under row locks for one consistent snapshot) and the panel links the reviewer to
// the target's own read-only page (/product, /order). Neither is a write, so neither needs an allowlist entry.

// moderationWriteTables are the tables moderation code may write, besides its own moderation_* tables.
var moderationWriteTables = []string{
	"audit_events", // the panel's and the offline review's record of a human review decision (append-only)
	"settings",     // the worker's on/off switch (key moderation_enabled, moderation_worker.go)
}

var (
	// Money and ban routes, settlements, refund addresses, the state machine, the ban refund and wallet sends.
	moderationMoneyRef = regexp.MustCompile(`/admin/ban\b|/admin/suspend\b|/escrow|/resolve\b|/orders/(?:cancel|complete|pay|ship|deliver|escrow)|/disputes/concede|/moderator/(?:payment-review|xmr-collision)` +
		`|\bescrowSettleTerminal\b|xmr_settlement|\bsettlements\b|\bpayouts\b|refund_address|\broleBanRefund\b|\bwithBanRefund\b|\btransition\(|\bbanned_(?:at|by)\b` +
		`|"(?:transfer|transfer_split|relay_tx|sweep_all|sweep_single|sweep_dust|sweep_unmixable)"|\b(?:buildTransfer|relayTx|walletCall|walletCallWithin)\b`)
	// A SQL write and its table (FOR UPDATE OF, FOR UPDATE SKIP LOCKED, DO UPDATE SET and FOR NO KEY UPDATE are not).
	moderationSQLWrite = regexp.MustCompile(`(?i)\b(?:update|insert\s+into|delete\s+from)\s+(?:only\s+)?"?([a-z_][a-z0-9_]*)`)
	// A form in the panel's templates.
	moderationFormAction = regexp.MustCompile(`action="([^"]*)"`)
)

// moneyDeclFiles are the files whose top-level declarations move money or ban: the state machine, escrow and its
// wallet adapter, the pooled payment history, the ban and ban-refund actions, and the order and dispute actions.
var moneyDeclFiles = []string{"orders_state.go", "escrow*.go", "payments*.go", "admin_ban*.go", "actions_orders.go", "orders_actions.go", "dispute_concede.go"}

func TestModerationNeverMovesMoney(t *testing.T) {
	fset := token.NewFileSet()
	money := map[string]string{} // top-level function, var, const and type names -> their file
	for _, pattern := range moneyDeclFiles {
		paths, err := filepath.Glob("internal/market/" + pattern)
		if err != nil || len(paths) == 0 {
			t.Fatalf("no internal/market/%s: update moneyDeclFiles (%v)", pattern, err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					// Methods are left out: their names (Close, Error) are generic; the wallet's are named above.
					if d.Recv == nil && d.Name.Name != "init" {
						money[d.Name.Name] = filepath.Base(path)
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch spec := spec.(type) {
						case *ast.ValueSpec:
							for _, n := range spec.Names {
								// The order state names (statePaid, ...) are read-only labels; moderation may compare them.
								if !strings.HasPrefix(n.Name, "state") {
									money[n.Name] = filepath.Base(path)
								}
							}
						case *ast.TypeSpec:
							money[spec.Name.Name] = filepath.Base(path)
						}
					}
				}
			}
		}
	}
	for _, name := range []string{"escrowClaimAction", "withBanRefund", "banOrdersAction", "insertXMRRelease", "xmrAddressUse"} {
		if money[name] == "" {
			t.Fatalf("the money files no longer declare %s: update moneyDeclFiles", name)
		}
	}

	groups := []string{"internal/moderation/", "internal/market/moderation", "cmd/server/moderation"}
	scanned := map[string]int{}
	var found []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel := filepath.ToSlash(path)
			group := ""
			for _, prefix := range groups {
				if strings.HasPrefix(rel, prefix) {
					group = prefix
				}
			}
			if group == "" {
				return nil
			}
			scanned[group]++
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range moderationMoneyRef.FindAll(src, -1) {
				found = append(found, rel+": "+string(m))
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			// The engine and model path have no database at all; their phrase lists are text, not SQL.
			engine := group == "internal/moderation/"
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); engine && (p == "database/sql" || strings.Contains(p, "pgx") || p == "opsecmkt/internal/market") {
					found = append(found, rel+": imports "+p)
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					if file := money[n.Name]; file != "" && strings.HasPrefix(rel, "internal/market/") {
						found = append(found, rel+": "+n.Name+" ("+file+")")
					}
				case *ast.BasicLit:
					if n.Kind != token.STRING || engine {
						return true
					}
					s, err := strconv.Unquote(n.Value)
					if err != nil {
						t.Fatalf("%s: %v", fset.Position(n.Pos()), err)
					}
					for _, m := range moderationSQLWrite.FindAllStringSubmatch(s, -1) {
						table := strings.ToLower(m[1])
						if slices.Contains([]string{"of", "skip", "set", "nowait"}, table) || strings.HasPrefix(table, "moderation_") || slices.Contains(moderationWriteTables, table) {
							continue
						}
						found = append(found, rel+": writes "+table)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, prefix := range groups {
		if scanned[prefix] == 0 {
			t.Fatalf("the guard scanned no %s* file", prefix)
		}
	}
	templates, err := filepath.Glob("web/templates/pages/moderation*.html")
	if err != nil || len(templates) == 0 {
		t.Fatal("no moderation templates", err)
	}
	for _, path := range templates {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range moderationMoneyRef.FindAll(src, -1) {
			found = append(found, path+": "+string(m))
		}
		for _, m := range moderationFormAction.FindAllSubmatch(src, -1) {
			if a := string(m[1]); a != "/admin/moderation" && !strings.HasPrefix(a, "/admin/moderation/") {
				found = append(found, path+": a form posts to "+a)
			}
		}
	}
	if len(found) > 0 {
		t.Fatalf("moderation code reaches money, orders or bans:\n%s", strings.Join(found, "\n"))
	}

	e := newTestApp(t) // skips without TEST_DATABASE_URL, after the source checks above
	// The capture triggers and every other moderation function in the database write only moderation's tables.
	if fns := agStrings(t, e, `SELECT proname::text FROM pg_proc WHERE proname LIKE 'moderation%' AND prosrc ~* '(update|insert\s+into|delete\s+from)\s+(only\s+)?"?(payments|orders|escrows|xmr_\w+|settlements|payouts|users|disputes|products)\M'`); len(fns) > 0 {
		t.Fatalf("moderation database functions write money, order or account tables: %v", fns)
	}
}
