package market

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-652a (PAY87-1): a ban never takes a refund away from a buyer. The source half: the ban actions (admin_ban*.go)
// never call the address checks or the settlement writers directly, and their SQL never writes a settlement, its
// outputs, or anything xmrAddressUse reads to classify an address. The A-652b ban refund reaches a refund only through
// the state machine (transition), which requests it to the pinned refund address (escrowSettleTerminal).

var (
	// Settlement writers and address classifiers the ban code must not call itself.
	banForbiddenIdents = []string{"xmrAddressUse", "checkXMRRefund", "xmrClaimAddress", "escrowSettleTerminal", "insertXMRRelease", "xmrRequestStray", "xmrReRequest"}
	// A write to a settlement, its outputs, or the address records xmrAddressUse reads.
	banPayeeWrite = regexp.MustCompile(`(?is)\b(?:update|insert\s+into|delete\s+from)\s+(?:only\s+)?"?(?:xmr_settlements|xmr_settlement_outputs|settlements|settlement_outputs|xmr_escrow_slots|xmr_escrow_wallets|escrow_external_addresses|retention_escrow_retired_addresses)\b` +
		`|\bdelete\s+from\s+"?escrows\b|\binsert\s+into\s+"?escrows\b|\bupdate\s+"?escrows"?\s+set\b.*\b(?:address|refund_address|fee_destination|xmr_slot_id)\s*=`)
)

func banPayeeFindings(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("internal/market/admin_ban*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found []string
	scanned := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if slices.Contains(banForbiddenIdents, n.Name) {
					found = append(found, fset.Position(n.Pos()).String()+": "+n.Name)
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				if err != nil {
					t.Fatalf("%s: %v", fset.Position(n.Pos()), err)
				}
				if m := banPayeeWrite.FindString(s); m != "" {
					found = append(found, fset.Position(n.Pos()).String()+": "+sqlSnippet(m))
				}
			}
			return true
		})
	}
	if scanned < 2 {
		t.Fatalf("scanned %d ban files: want admin_ban.go and admin_ban_orders.go", scanned)
	}
	return found
}

// A buyer banned while the refund of their cancelled order, and of their order decided for them, is still to be built
// gets each refund exactly once, to the refund address they pinned at checkout. The ban changes neither refund
// settlement nor how the market classifies that address (xmrAddressUse). DB integration against the simulated Monero
// wallet RPC, not chain evidence.
func TestBannedBuyerRefundStillPaid(t *testing.T) {
	if found := banPayeeFindings(t); len(found) > 0 {
		t.Fatalf("ban code reaches a settlement or the address records directly:\n%s", strings.Join(found, "\n"))
	}
	x := newXMREscrowEnv(t) // skips without TEST_DATABASE_URL, after the source checks above
	x.mustPoll()
	_, admin := x.user("bbr_admin", "admin")
	phys := x.physical(x.vendor)
	cancelled, _ := x.paidXMRPhysical(phys)
	// Both orders are funded (by a pass) before either refund is requested, so no pass builds a refund before the bans.
	refunded, _ := x.paidXMRPhysical(phys)
	x.forceDeadline(cancelled, "ship_by=now()-interval '1 minute'") // the vendor did not ship in time
	if code, body := x.xmrBuyerCancel(cancelled, x.sessOf(cancelled, roleBuyer)); code != 303 || x.state(cancelled) != stateCancelled {
		t.Fatalf("buyer's cancellation: %d %s (%s)", code, body, x.state(cancelled))
	}
	x.disputeAndResolve(refunded, "refund")

	ctx := context.Background()
	type orderView struct {
		buyer, refund string
		rows          []string // every settlement of the order, as stored
		use           [2]string
	}
	view := func(order string) orderView {
		t.Helper()
		v := orderView{buyer: agStr(x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order), refund: agStr(x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)}
		v.rows = agStrings(t, x.testEnv, `SELECT id||' '||kind||' '||state||' '||destination||' '||COALESCE(requested_by,'-')||' '||COALESCE(supersedes::text,'-')||' '||COALESCE(tx_hash,'-')||' '||send_error
			FROM xmr_settlements WHERE order_id=$1 ORDER BY id`, order)
		for i, o := range []string{order, ""} {
			use, err := xmrAddressUse(ctx, x.DB, v.refund, x.A.escrowXMR.feeAddress, o, v.buyer)
			if err != nil {
				t.Fatal(err)
			}
			v.use[i] = use
		}
		return v
	}
	before := map[string]orderView{}
	for _, order := range []string{cancelled, refunded} {
		v := view(order)
		if len(v.rows) != 1 || !strings.Contains(v.rows[0], " refund requested "+v.refund+" ") || v.use[0] != "refund" {
			t.Fatalf("order %s before the ban: settlements %q, address use %q", shortID(order), v.rows, v.use)
		}
		before[order] = v
	}

	for _, order := range []string{cancelled, refunded} {
		handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", before[order].buyer)
		agExpect(x.testEnv, "/admin/ban", admin, banForm("ban", handle, testBanReason), 303, "")
		if n := agInt(x.testEnv, "SELECT count(*) FROM users WHERE id=$1 AND banned_at IS NOT NULL", before[order].buyer); n != 1 {
			t.Fatalf("buyer of %s not banned", shortID(order))
		}
	}
	for _, order := range []string{cancelled, refunded} {
		if v := view(order); !slices.Equal(v.rows, before[order].rows) || v.refund != before[order].refund || v.use != before[order].use {
			t.Fatalf("the ban changed order %s: settlements %q (was %q), refund address %s (was %s), address use %q (was %q)",
				shortID(order), v.rows, before[order].rows, v.refund, before[order].refund, v.use, before[order].use)
		}
	}

	x.mustPoll()
	x.mustPoll()
	x.fake.mine(10)
	x.mustPoll()
	x.mustPoll()
	calls := x.fake.callLog()
	for _, order := range []string{cancelled, refunded} {
		b := before[order]
		ids := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_settlements WHERE order_id=$1", order)
		if len(ids) != 1 {
			t.Fatalf("order %s has %d settlements after the ban, want its one refund", shortID(order), len(ids))
		}
		id, _ := strconv.ParseInt(ids[0], 10, 64)
		s := x.settlement(id)
		want := []string{"claimant:" + b.refund + ":" + strconv.FormatInt(xmrListingPrice-s.Fee, 10)}
		if s.Kind != "refund" || s.State != "confirmed" || !slices.Equal(x.outputsOf(id), want) {
			t.Fatalf("refund of %s: %+v outputs %v, want confirmed paying %v", shortID(order), s, x.outputsOf(id), want)
		}
		metadata := agStr(x.testEnv, "SELECT tx_metadata FROM xmr_settlements WHERE id=$1", id)
		built, relayed := 0, 0
		for _, c := range calls {
			switch {
			case c.Method == "transfer" && strings.Contains(c.Params, b.refund):
				built++
			case c.Method == "relay_tx" && strings.Contains(c.Params, metadata):
				relayed++
			}
		}
		if built != 1 || relayed != 1 {
			t.Fatalf("refund of %s: %d builds paying the refund address and %d relays, want one of each", shortID(order), built, relayed)
		}
		if v := view(order); v.refund != b.refund || v.use != b.use {
			t.Fatalf("order %s after its refund: refund address %s (was %s), address use %q (was %q)", shortID(order), v.refund, b.refund, v.use, b.use)
		}
	}
}
