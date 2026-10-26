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

// A-652a (PAY87-1, PAY87-7; owner decision 2026-10-25 "refund + hold, no new payee"): a ban pays only the order's
// pinned refund address or the vendor's own claim address. There is no forfeit kind and no administrator-chosen payee.
// TestNoForfeitSettlementKind turns that rule into a guard on the three places a new payee could come from: a
// migration widening the settlement kinds or output roles, a database function creating a settlement, and a Go
// statement creating a settlement somewhere other than the reviewed sites below.

// The Monero settlement kinds (migration 084) and output roles (084) as the owner ruled them: any other value is a new
// payee, whatever it is called.
var (
	settlementKinds = []string{"refund", "release", "stray_refund"}
	settlementRoles = []string{"buyer_return", "claimant", "market_fee"}
)

// firstGuardedMigration is the first migration the source half reads: 148 and every later one (migrations up to 147
// were reviewed when merged; the database half below covers their combined result).
const firstGuardedMigration = 148

// xmrSettlementSites lists every non-test Go function (file and name) that creates an xmr_settlements row, with the
// statement text it must still contain, which names where that row's destination comes from. A new site, or a reviewed
// site paying anything else, fails the guard: review it against PAY87-1 before adding it here.
var xmrSettlementSites = map[string]string{
	// The refund of a cancelled order or a dispute decided for the buyer, by the state machine (transition), the A-652b
	// ban refund included: it pays the refund address the buyer pinned at checkout. The table is a variable (Bitcoin
	// history uses settlements), so the guard cannot see its name and pins the rest of the statement.
	"internal/market/escrow_settle.go escrowSettleTerminal": "(order_id,kind,destination) SELECT order_id,'refund',refund_address FROM escrows WHERE order_id=$1",
	// The vendor's claim of a completed order (and the release requested at shipment): the vendor's own claim address,
	// from its form, refused if this market knows it (xmrClaimAddress).
	"internal/market/escrow_xmr_release.go insertXMRRelease": "INSERT INTO xmr_settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3)",
	// An administrator's re-request of a failed settlement: the same order, kind and destination as the one it supersedes.
	"internal/market/escrow_xmr_rerequest.go xmrReRequest": "INSERT INTO xmr_settlements(order_id,kind,destination,requested_by,supersedes) SELECT order_id,kind,destination,$2,id FROM xmr_settlements WHERE id=$1",
	// The stray refund of a deposit to a closed order: the pinned refund address.
	"internal/market/escrow_xmr_settle.go xmrRequestStray": "INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1",
}

var (
	// A statement that creates xmr_settlements rows by name.
	xmrSettlementInsert = regexp.MustCompile(`(?i)\b(?:insert\s+into|copy)\s+(?:\w+\.)?"?xmr_settlements\b`)
	// A statement whose table is not a literal: an INSERT INTO followed by a concatenation or a format verb.
	dynamicInsert = regexp.MustCompile(`(?i)\binsert\s+into\s*(?:%|$)`)
)

func TestNoForfeitSettlementKind(t *testing.T) {
	var found []string
	found = append(found, forfeitMigrationFindings(t)...)
	found = append(found, settlementSiteFindings(t)...)
	if len(found) > 0 {
		t.Fatalf("a new settlement kind, output role, payee or settlement site (PAY87-1: review it, then update this guard only if the owner ruled it):\n%s", strings.Join(found, "\n"))
	}

	e := newTestApp(t) // skips without TEST_DATABASE_URL, after the source checks above
	for _, c := range []struct{ table, column string }{{"xmr_settlements", "kind"}, {"xmr_settlement_outputs", "role"}} {
		defs := agStrings(t, e, `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=ANY(c.conkey)
			WHERE c.conrelid=$1::regclass AND c.contype='c' AND cardinality(c.conkey)=1 AND a.attname=$2`, c.table, c.column)
		want := settlementKinds
		if c.column == "role" {
			want = settlementRoles
		}
		if len(defs) != 1 || !slices.Equal(sqlLiterals(defs[0]), want) {
			t.Fatalf("%s.%s CHECK is %q, want exactly one allowing %v", c.table, c.column, defs, want)
		}
	}
	if fns := agStrings(t, e, `SELECT proname::text FROM pg_proc WHERE prosrc ~* '(insert\s+into|copy)\s+("?\w+"?\.)?"?xmr_settlements\M'`); len(fns) > 0 {
		t.Fatalf("database functions create settlements: %v", fns)
	}
}

// forfeitMigrationFindings reads migrations firstGuardedMigration and later, statement by statement.
func forfeitMigrationFindings(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("internal/market/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("no migrations", err)
	}
	var (
		settlementTable = `(?:\w+\.)?"?(?:xmr_settlements|xmr_settlement_outputs|settlements|settlement_outputs)"?\b`
		// Creating a settlement table again, or adding, dropping or retyping its kind or role column or dropping that
		// column's CHECK (a CHECK added in its place is read by the value checks below).
		alterKind = regexp.MustCompile(`(?is)\bcreate\s+table\s+(?:if\s+not\s+exists\s+)?` + settlementTable + `|\balter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?` + settlementTable +
			`.*?\b(?:(?:add|drop|alter)\s+(?:column\s+)?(?:if\s+(?:not\s+)?exists\s+)?"?(?:kind|role)"?\b|drop\s+constraint\s+(?:if\s+exists\s+)?"?\w*(?:kind|role)_check\b)`)
		// Creating a settlement in the database, or changing whom one or an escrow pays.
		payeeWrite = regexp.MustCompile(`(?is)\binsert\s+into\s+` + settlementTable + `|\bupdate\s+(?:only\s+)?(?:\w+\.)?"?(?:xmr_settlements|settlements)"?\s+set\b.*\b(?:kind|destination)\s*=|\bupdate\s+(?:only\s+)?(?:\w+\.)?"?(?:xmr_settlement_outputs|settlement_outputs)"?\s+set\b|\bupdate\s+(?:only\s+)?(?:\w+\.)?"?escrows"?\s+set\b.*\brefund_address\s*=`)
		// A statement that reads or decides on settlements: its kind and role values are checked.
		settlementStmt = regexp.MustCompile(`(?i)\b(?:xmr_settlements|xmr_settlement_outputs|settlements|settlement_outputs|escrow_market_may_sign|settlement_claimant)\b`)
		// The places a kind or role value appears: compared, listed, matched in a CASE or in an array.
		valueEq   = regexp.MustCompile(`(?i)\b(kind|role)\s*(?:=|<>|!=)\s*'([^']*)'`)
		valueList = regexp.MustCompile(`(?is)\b(kind|role)\s+(?:not\s+)?in\s*\(([^)]*)\)|\b(kind|role)\s*(?:=|<>|!=)\s*(?:any|all)\s*\(([^)]*)\)`)
		valueCase = regexp.MustCompile(`(?is)\bcase\s+(?:\w+\.)?(kind|role)\b(.*?)\bend\b`)
		whenValue = regexp.MustCompile(`(?i)\bwhen\s+'([^']*)'`)
	)
	var found []string
	for _, path := range files {
		n, err := strconv.Atoi(strings.SplitN(filepath.Base(path), "_", 2)[0])
		if err != nil {
			t.Fatalf("migration %s has no number", path)
		}
		if n < firstGuardedMigration {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range sqlStatements(string(src)) {
			where := filepath.Base(path) + ": " + sqlSnippet(stmt)
			if alterKind.MatchString(stmt) {
				found = append(found, where+": changes a settlement table's kind or role column")
			}
			if payeeWrite.MatchString(stmt) {
				found = append(found, where+": creates a settlement or changes whom a settlement or an escrow pays")
			}
			if !settlementStmt.MatchString(stmt) {
				continue
			}
			check := func(column string, values []string) {
				want := settlementKinds
				if strings.EqualFold(column, "role") {
					want = settlementRoles
				}
				for _, v := range values {
					if !slices.Contains(want, v) {
						found = append(found, where+": "+strings.ToLower(column)+" value '"+v+"' is not one of "+strings.Join(want, ", "))
					}
				}
			}
			for _, m := range valueEq.FindAllStringSubmatch(stmt, -1) {
				check(m[1], []string{m[2]})
			}
			for _, m := range valueList.FindAllStringSubmatch(stmt, -1) {
				column, list := m[1]+m[3], m[2]+m[4]
				check(column, sqlLiterals(list))
			}
			for _, m := range valueCase.FindAllStringSubmatch(stmt, -1) {
				var values []string
				for _, w := range whenValue.FindAllStringSubmatch(m[2], -1) {
					values = append(values, w[1])
				}
				check(m[1], values)
			}
		}
	}
	return found
}

// settlementSiteFindings walks the non-test Go of internal and cmd for statements that create xmr_settlements rows.
func settlementSiteFindings(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	sites := map[string][]string{} // file and function -> every string literal in it, when it creates settlements
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(path)
			named := map[ast.Node]string{} // each function, and each package-level const or var (a statement kept there)
			for _, decl := range f.Decls {
				switch decl := decl.(type) {
				case *ast.FuncDecl:
					named[decl] = decl.Name.Name
				case *ast.GenDecl:
					for _, spec := range decl.Specs {
						if v, ok := spec.(*ast.ValueSpec); ok {
							named[v] = v.Names[0].Name
						}
					}
				}
			}
			for node, name := range named {
				var lits []string
				creates := false
				ast.Inspect(node, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
					}
					s = strings.Join(strings.Fields(s), " ")
					lits = append(lits, s)
					if xmrSettlementInsert.MatchString(s) || dynamicInsert.MatchString(s) {
						creates = true
					}
					return true
				})
				if creates {
					key := rel + " " + name
					sites[key] = append(sites[key], lits...)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var found []string
	for key, lits := range sites {
		want, ok := xmrSettlementSites[key]
		switch {
		case !ok:
			found = append(found, key+": a new statement creating settlements (or an INSERT whose table the guard cannot read)")
		case !slices.ContainsFunc(lits, func(s string) bool { return strings.Contains(s, want) }):
			found = append(found, key+": no longer contains "+want+": its destination changed")
		}
	}
	for key := range xmrSettlementSites {
		if _, ok := sites[key]; !ok {
			found = append(found, key+": reviewed site not found (renamed or removed): update xmrSettlementSites")
		}
	}
	slices.Sort(found)
	return found
}

// sqlStatements splits a migration into its statements at semicolons outside comments, quoted strings and
// dollar-quoted bodies (so a function is one statement), with comments removed.
func sqlStatements(src string) []string {
	var out []string
	var b strings.Builder
	dollar := regexp.MustCompile(`^\$[A-Za-z_0-9]*\$`)
	for i := 0; i < len(src); i++ {
		switch {
		case strings.HasPrefix(src[i:], "--"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 3
			}
			b.WriteByte(' ')
		case src[i] == '\'':
			end := i + 1
			for end < len(src) && (src[end] != '\'' || end+1 < len(src) && src[end+1] == '\'') {
				if src[end] == '\'' {
					end++ // a doubled quote inside the string
				}
				end++
			}
			b.WriteString(src[i:min(end+1, len(src))])
			i = end
		case dollar.MatchString(src[i:]):
			tag := dollar.FindString(src[i:])
			end := strings.Index(src[i+len(tag):], tag)
			if end < 0 {
				end = len(src) - i - len(tag)
			}
			// The body is SQL too (a function's statements): kept, with its own comments removed.
			b.WriteString(tag + strings.Join(sqlStatements(src[i+len(tag):i+len(tag)+end]), ";\n") + tag)
			i += len(tag) + end + len(tag) - 1
		case src[i] == ';':
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(src[i])
		}
	}
	out = append(out, b.String())
	return slices.DeleteFunc(out, func(s string) bool { return strings.TrimSpace(s) == "" })
}

// sqlLiterals lists the single-quoted values in a SQL fragment, sorted and unique.
func sqlLiterals(s string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`'((?:[^']|'')*)'`).FindAllStringSubmatch(s, -1) {
		out = append(out, strings.ReplaceAll(m[1], "''", "'"))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func sqlSnippet(stmt string) string {
	s := strings.Join(strings.Fields(stmt), " ")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
