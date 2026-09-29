package market

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A-244 (ARCH-1): every terminal-transition settlement goes through settleTerminal. The transition hook and the
// watcher's retry (settleOrder) are its only callers, and it is the only caller of enqueuePayout, so a later
// escrow branch cannot be bypassed by one call site that still queues a pooled payout.
func TestTerminalSettlementGoesThroughOneDispatcher(t *testing.T) {
	// TestMain runs the package's tests from the repository root.
	files, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("package sources not found: %v", err)
	}
	callers := map[string][]string{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "enqueuePayout" || sel.Sel.Name == "settleTerminal") {
						callers[sel.Sel.Name] = append(callers[sel.Sel.Name], fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if got := callers["enqueuePayout"]; !slices.Equal(got, []string{"settleTerminal"}) {
		t.Errorf("enqueuePayout called from %v; only settleTerminal may queue a pooled payout", got)
	}
	got := slices.Clone(callers["settleTerminal"])
	slices.Sort(got)
	if !slices.Equal(got, []string{"paymentsTransitionHook", "settleOrder"}) {
		t.Errorf("settleTerminal called from %v; want the transition hook and the watcher's settleOrder", got)
	}
}

// settleTerminal on a pooled order queues exactly what enqueuePayout queued before (the whole payout suite runs
// through it); an order marked escrow never gets a pooled payout (no escrow settlement exists yet, so it is
// refused and the transaction rolls back).
func TestSettleTerminalPooledAndEscrowBranches(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcherAmount(100000)
	p.move(p.order, statePaid, stateShipped, p.vendor)
	ctx := context.Background()
	settle := func(escrow bool) error {
		tx, err := p.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, "UPDATE orders SET state='completed' WHERE id=$1", p.order); err != nil {
			t.Fatal(err)
		}
		o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", p.order))
		if err != nil {
			t.Fatal(err)
		}
		if o.Escrow {
			t.Fatal("an order loaded today is marked escrow")
		}
		o.Escrow = escrow
		if err = p.A.settleTerminal(ctx, tx, &o); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := settle(true); err == nil {
		t.Fatal("escrow order settled through the pooled wallet")
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 || p.state(p.order) != stateShipped {
		t.Fatalf("escrow branch queued %d payouts or moved the order", n)
	}
	if err := settle(false); err != nil {
		t.Fatal(err)
	}
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-vendor" || amt != 100000 {
		t.Fatalf("pooled release %s %s %d", st, addr, amt)
	}
}
