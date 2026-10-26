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

// A-652a (PAY87-7, PAY89-8, PAY89-10): moderation never moves money (AGENTS.md; docs/acceptance.md "Moderation cannot
// move money, change an order, ban an account or hide a listing"). TestModerationNeverMovesMoney scans the moderation
// layer, as TestModerationNeverWritesReviewState does: the engine and model path (internal/moderation), the worker,
// private panel, budget and offline review in internal/market (moderation*.go), the worker's launcher
// (cmd/server/moderation*.go), the Go reputation rule (reputation*.go, A-653), every other non-test file that imports
// internal/moderation, the panel's templates and the reputation partial. None of it may name a money or ban route, a
// settlement or refund address, the state machine, the ban refund, a wallet send, or anything the money files declare
// (comments are not read). The engine imports no database package; the rest writes only moderation_* tables, appends to
// audit_events, and writes settings only under literal moderation_ keys.
//
// Reads stay allowed, deliberately: the worker snapshots the listing, dispute or payment flag it scores (products,
// disputes, orders and payments, read under row locks for one consistent snapshot), the reputation rule counts a
// vendor's orders and disputes, and the panel links the reviewer to the target's own read-only page (/product, /order).
// None is a write, so none needs an allowlist entry.

var (
	// Money and ban routes, settlements, refund addresses, the state machine, the ban refund and wallet sends.
	moderationMoneyRef = regexp.MustCompile(`/admin/ban\b|/admin/suspend\b|/escrow|/resolve\b|/orders/(?:cancel|complete|pay|ship|deliver|escrow)|/disputes/concede|/moderator/(?:payment-review|xmr-collision)` +
		`|\bescrowSettleTerminal\b|xmr_settlement|\bsettlements\b|\bpayouts\b|refund_address|\broleBanRefund\b|\bwithBanRefund\b|\btransition\(|\bbanned_(?:at|by)\b` +
		`|"(?:transfer|transfer_split|relay_tx|sweep_all|sweep_single|sweep_dust|sweep_unmixable)"|\b(?:buildTransfer|relayTx|walletCall|walletCallWithin)\b`)
	// A SQL write: its verb and table (FOR UPDATE OF, FOR UPDATE SKIP LOCKED, DO UPDATE SET and FOR NO KEY UPDATE are not).
	moderationSQLWrite = regexp.MustCompile(`(?i)\b(update|insert\s+into|delete\s+from)\s+(?:only\s+)?"?([a-z_][a-z0-9_]*)`)
	// A settings key that is not one literal (a parameter, a list, an array or a negation), a statement that changes the
	// key column, and the literal keys a statement names.
	settingsKeyParam   = regexp.MustCompile(`(?i)\bkey\s*(?:=\s*(?:\$|any\b)|in\b|<>|!=)|\bvalues\s*\(\s*\$|\bset\s+(?:[^;]*?,\s*)?key\s*=`)
	settingsKeyLiteral = regexp.MustCompile(`(?i)\bkey\s*=\s*'([^']*)'|\bsettings\s*\(\s*key\b[^)]*\)\s*values\s*\(\s*'([^']*)'`)
	// A form in the panel's templates.
	moderationFormAction = regexp.MustCompile(`action="([^"]*)"`)
	// Template comments, which are not read.
	templateComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}|<!--.*?-->`)
)

// moderationSettingsWrite reports whether SQL literal s, which writes settings, names only literal moderation_ keys.
func moderationSettingsWrite(s string) bool {
	if settingsKeyParam.MatchString(s) {
		return false
	}
	keys := settingsKeyLiteral.FindAllStringSubmatch(s, -1)
	for _, k := range keys {
		if !strings.HasPrefix(k[1]+k[2], "moderation_") {
			return false
		}
	}
	return len(keys) > 0
}

// moneyDeclFiles are the files whose top-level declarations move money or ban: the state machine, escrow and its
// wallet adapter, the pooled payment history, the ban and ban-refund actions, and the order and dispute actions.
var moneyDeclFiles = []string{"orders_state.go", "escrow*.go", "payments*.go", "admin_ban*.go", "actions_orders.go", "orders_actions.go", "dispute_concede.go"}

// moneyLabels are money-file declarations the moderation layer may read: fixed status texts, never a writer.
var moneyLabels = []string{
	"disputeOutcomes",  // the resolution text of a decided dispute (actions_orders.go); reputation.go counts by it
	"concededByBuyer",  // the resolution text of a buyer's concession (dispute_concede.go); reputation.go counts by it
	"concededByVendor", // the resolution text of a vendor's concession (dispute_concede.go); reputation.go counts by it
}

const moderationImport = "opsecmkt/internal/moderation"

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
	for _, name := range moneyLabels {
		if money[name] == "" {
			t.Fatalf("the money files no longer declare %s: update moneyLabels", name)
		}
		delete(money, name)
	}

	const importers = "importers of " + moderationImport
	groups := []string{"internal/moderation/", "internal/market/moderation", "cmd/server/moderation", "internal/market/reputation"}
	scanned := map[string]int{}
	var found []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel := filepath.ToSlash(path)
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
			if err != nil {
				return err
			}
			group := ""
			for _, prefix := range groups {
				if strings.HasPrefix(rel, prefix) {
					group = prefix
				}
			}
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == moderationImport && group == "" {
					group = importers
				}
			}
			if group == "" {
				return nil
			}
			scanned[group]++
			code := slices.Clone(src) // the source with its comments blanked: prose about money is not a reference to it
			for _, c := range f.Comments {
				for i := fset.Position(c.Pos()).Offset; i < fset.Position(c.End()).Offset; i++ {
					if code[i] != '\n' {
						code[i] = ' '
					}
				}
			}
			for _, m := range moderationMoneyRef.FindAll(code, -1) {
				found = append(found, rel+": "+string(m))
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
						verb, table := strings.ToLower(strings.Fields(m[1])[0]), strings.ToLower(m[2])
						switch {
						case slices.Contains([]string{"of", "skip", "set", "nowait"}, table), strings.HasPrefix(table, "moderation_"):
						case table == "audit_events" && verb == "insert": // the record of a human review decision, append-only
						case table == "settings" && moderationSettingsWrite(s): // the worker's switch, key moderation_enabled
						default:
							found = append(found, fset.Position(n.Pos()).String()+": "+verb+" "+table+": "+sqlSnippet(s))
						}
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
	for _, prefix := range append(groups, importers) {
		if scanned[prefix] == 0 {
			t.Fatalf("the guard scanned no file in %s", prefix)
		}
	}
	pages, err := filepath.Glob("web/templates/pages/moderation*.html")
	if err != nil || len(pages) == 0 {
		t.Fatal("no moderation templates", err)
	}
	partials, err := filepath.Glob("web/templates/partials/admin-reputation*.html")
	if err != nil || len(partials) == 0 {
		t.Fatal("no reputation partial", err)
	}
	for _, path := range append(pages, partials...) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src = templateComment.ReplaceAll(src, nil)
		for _, m := range moderationMoneyRef.FindAll(src, -1) {
			found = append(found, path+": "+string(m))
		}
		// The moderation pages post only to moderation's own routes. The reputation partial's one form is the
		// administrator's confirmed publish-without-review switch (/admin/vendor-auto-approve, A-653), no money route.
		for _, m := range moderationFormAction.FindAllSubmatch(src, -1) {
			if a := string(m[1]); slices.Contains(pages, path) && a != "/admin/moderation" && !strings.HasPrefix(a, "/admin/moderation/") {
				found = append(found, path+": a form posts to "+a)
			}
		}
	}
	if len(found) > 0 {
		t.Fatalf("moderation code reaches money, orders or bans:\n%s", strings.Join(found, "\n"))
	}

	e := newTestApp(t) // skips without TEST_DATABASE_URL, after the source checks above
	// The capture triggers and every other moderation function in the database write only moderation's tables.
	if fns := agStrings(t, e, `SELECT proname::text FROM pg_proc WHERE proname LIKE 'moderation%' AND prosrc ~* '(update|insert\s+into|delete\s+from)\s+(only\s+)?"?(payments|orders|escrows|xmr_\w+|settlements|payouts|users|disputes|products|settings|audit_events)\M'`); len(fns) > 0 {
		t.Fatalf("moderation database functions write money, order, account or settings tables: %v", fns)
	}
}
