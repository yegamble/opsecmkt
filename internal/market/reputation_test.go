package market

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-653: deterministic reputation figures computed on read and a closed label from a fixed Go rule table
// (reputation.go), shown to administrators only and confirmed through /admin/vendor-auto-approve.

var reputationLabels = []string{repHigh, repStandard, repInsufficient, repAtRisk}

// repFixture is a vendor with a physical and a digital listing and a staff account for dispute decisions; seed adds
// orders as their real paths record them.
type repFixture struct {
	e                                              *testEnv
	vendorID, vendorSess, physical, digital, staff string
}

func newRepFixture(e *testEnv, handle string) *repFixture {
	e.t.Helper()
	f := &repFixture{e: e}
	f.vendorID, f.vendorSess = e.user(handle, "vendor")
	f.physical, f.digital = e.product(f.vendorID, "physical"), e.product(f.vendorID, "digital")
	f.staff, _ = e.keylessUser(handle+"_staff", "moderator")
	return f
}

// repPath is how a funded order ends: its final state, its dispute status ("" none) and the moves after paid.
type repPath struct {
	state, dispute string
	digital        bool
	moves          [][3]string // from, to, actor role
}

func repPaths() map[string]repPath {
	shipped := [][3]string{{statePaid, stateShipped, roleVendor}}
	disputed := append(slices.Clone(shipped), [3]string{stateShipped, stateDisputed, roleBuyer})
	return map[string]repPath{
		"confirmed":      {stateCompleted, "", false, append(slices.Clone(shipped), [3]string{stateShipped, stateCompleted, roleBuyer})},
		"deadline":       {stateCompleted, "", false, append(slices.Clone(shipped), [3]string{stateShipped, stateCompleted, roleSystem})},
		"digital":        {stateCompleted, "", true, [][3]string{{statePaid, stateDelivered, roleSystem}, {stateDelivered, stateCompleted, roleSystem}}},
		"noship":         {stateCancelled, "", false, [][3]string{{statePaid, stateCancelled, roleBuyer}}},
		"vendorcancel":   {stateCancelled, "", false, [][3]string{{statePaid, stateCancelled, roleVendor}}},
		"refund":         {stateResolved, disputeOutcomes["refund"], false, append(slices.Clone(disputed), [3]string{stateDisputed, stateResolved, roleModerator})},
		"release":        {stateResolved, disputeOutcomes["release"], false, append(slices.Clone(disputed), [3]string{stateDisputed, stateResolved, roleModerator})},
		"buyerconceded":  {stateCompleted, concededByBuyer, false, append(slices.Clone(disputed), [3]string{stateDisputed, stateCompleted, roleBuyer})},
		"vendorconceded": {stateCancelled, concededByVendor, false, append(slices.Clone(disputed), [3]string{stateDisputed, stateCancelled, roleVendor})},
		"open":           {stateDisputed, "Open", false, disputed},
		"paid":           {statePaid, "", false, nil},
	}
}

// seed inserts one order of buyerID's that ended as how, with the order_events its path writes; "expired" is a payment
// window that ran out (never funded).
func (f *repFixture) seed(buyerID, how string) string {
	e := f.e
	e.t.Helper()
	p, ok := repPaths()[how]
	moves := [][3]string{{stateDraft, stateAwaitingPayment, roleBuyer}, {stateAwaitingPayment, statePaid, roleSystem}}
	switch {
	case how == "expired":
		p, moves = repPath{state: stateCancelled}, [][3]string{{stateDraft, stateAwaitingPayment, roleBuyer}, {stateAwaitingPayment, stateCancelled, roleSystem}}
	case !ok:
		e.t.Fatalf("no reputation path %q", how)
	}
	product := f.physical
	if p.digital {
		product = f.digital
	}
	id := e.order(buyerID, product, "XMR", p.state)
	actors := map[string]any{roleBuyer: buyerID, roleVendor: f.vendorID, roleModerator: f.staff, roleSystem: nil}
	for _, m := range append(moves, p.moves...) {
		agExec(e, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$3,$4,'')", id, m[0], m[1], actors[m[2]])
	}
	if p.dispute != "" {
		outcome := map[string]string{disputeOutcomes["refund"]: "refund", concededByVendor: "refund", disputeOutcomes["release"]: "release", concededByBuyer: "release"}[p.dispute]
		agExec(e, "INSERT INTO disputes(id,order_id,reason,status,outcome) VALUES($1,$2,'The parcel never arrived at all.',$3,$4)", randomToken(), id, p.dispute, outcome)
	}
	return id
}

func (f *repFixture) review(orderID, buyerID string, rating int) {
	f.e.t.Helper()
	agExec(f.e, "INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) SELECT $1,o.id,o.product_id,$2,$3,'Fixture review' FROM orders o WHERE o.id=$4", randomToken(), buyerID, rating, orderID)
}

// buyers inserts n buyer accounts.
func (f *repFixture) buyers(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i], _ = f.e.keylessUser(prefix+strconv.Itoa(i), "buyer")
	}
	return ids
}

// seedStrong gives the vendor a HIGH_REPUTATION record: 26 completions (two per buyer, of all three routes) by 13
// buyers, each reviewed with five stars.
func (f *repFixture) seedStrong(prefix string) []string {
	ids := f.buyers(prefix, 13)
	for i, b := range ids {
		for _, how := range []string{"confirmed", []string{"confirmed", "deadline", "digital"}[i%3]} {
			f.review(f.seed(b, how), b, 5)
		}
	}
	return ids
}

func (f *repFixture) assess() *ReputationView {
	f.e.t.Helper()
	v, err := vendorReputation(context.Background(), f.e.DB, f.vendorID)
	if err != nil {
		f.e.t.Fatal(err)
	}
	return v
}

func strongFigures() ReputationFigures {
	return ReputationFigures{Funded: 30, FundedBuyers: 20, CompletedConfirmed: 20, CompletedDeadline: 6, CompletedDigital: 4, CompletedBuyers: 20, Reviews: 20}
}

func TestReputationLabelTable(t *testing.T) {
	with := func(change func(*ReputationFigures)) ReputationFigures {
		f := strongFigures()
		change(&f)
		return f
	}
	for _, c := range []struct {
		name    string
		f       ReputationFigures
		label   string
		reasons []string
	}{
		{"no history (the default)", ReputationFigures{}, repInsufficient, []string{repFewCompleted}},
		{"strong record", strongFigures(), repHigh, []string{repStrongRecord}},
		{"four completed orders", ReputationFigures{Funded: 4, FundedBuyers: 4, CompletedConfirmed: 4, CompletedBuyers: 4}, repInsufficient, []string{repFewCompleted}},
		{"below the HIGH volume floor", with(func(f *ReputationFigures) { f.CompletedConfirmed, f.CompletedDeadline, f.CompletedDigital = 24, 0, 0 }), repStandard, []string{repFewCompleted}},
		{"below the distinct-buyer floor", with(func(f *ReputationFigures) { f.CompletedBuyers = 9 }), repStandard, []string{repFewBuyers, repConcentration}},
		{"repeat buyers below half", with(func(f *ReputationFigures) { f.CompletedBuyers = 14 }), repStandard, []string{repConcentration}},
		{"concentrated buyers", with(func(f *ReputationFigures) { f.CompletedBuyers = 7 }), repAtRisk, []string{repConcentration}},
		{"one non-delivery refund", with(func(f *ReputationFigures) { f.NoShipCancels = 1 }), repHigh, []string{repStrongRecord}},
		{"some non-delivery refunds", with(func(f *ReputationFigures) { f.NoShipCancels, f.VendorCancels = 1, 1 }), repStandard, []string{repNondelivery}},
		{"many non-delivery refunds", with(func(f *ReputationFigures) { f.NoShipCancels, f.VendorCancels = 3, 5 }), repAtRisk, []string{repNondelivery}},
		{"refunds before any completion", ReputationFigures{Funded: 3, FundedBuyers: 3, VendorCancels: 3}, repAtRisk, []string{repNondelivery}},
		{"two refunds before any completion", ReputationFigures{Funded: 2, FundedBuyers: 2, VendorCancels: 2}, repInsufficient, []string{repFewCompleted}},
		{"a dispute lost", with(func(f *ReputationFigures) { f.DecidedRefund = 1 }), repStandard, []string{repDisputesLost}},
		{"disputes won", with(func(f *ReputationFigures) { f.DecidedRelease, f.BuyerConceded = 5, 5 }), repHigh, []string{repStrongRecord}},
		{"disputes lost", with(func(f *ReputationFigures) { f.DecidedRefund, f.VendorConceded = 2, 2 }), repAtRisk, []string{repDisputesLost}},
		{"a low rating", with(func(f *ReputationFigures) { f.LowReviews = 1 }), repHigh, []string{repStrongRecord}},
		{"some low ratings", with(func(f *ReputationFigures) { f.LowReviews = 2 }), repStandard, []string{repLowRatings}},
		{"many low ratings", with(func(f *ReputationFigures) { f.LowReviews = 6 }), repAtRisk, []string{repLowRatings}},
		{"a listing taken down", with(func(f *ReputationFigures) { f.Takedowns = 1 }), repStandard, []string{repRemovals}},
		{"listings removed", with(func(f *ReputationFigures) { f.Takedowns, f.Rejections = 1, 2 }), repAtRisk, []string{repRemovals}},
		{"several reasons", with(func(f *ReputationFigures) { f.Takedowns, f.DecidedRefund, f.CompletedBuyers = 3, 4, 5 }), repAtRisk, []string{repConcentration, repDisputesLost, repRemovals}},
	} {
		v := assessReputation("vendor-id", c.f)
		if v.Label != c.label || !slices.Equal(v.Reasons, c.reasons) {
			t.Errorf("%s: %s %v, want %s %v", c.name, v.Label, v.Reasons, c.label, c.reasons)
		}
	}
	// The table is fixed: the same rows in the same order, each with its figure and both thresholds, and an effect
	// that is one of the labels.
	codes := []string{repFewCompleted, repFewBuyers, repConcentration, repNondelivery, repDisputesLost, repLowRatings, repRemovals}
	v := assessReputation("vendor-id", with(func(f *ReputationFigures) { f.NoShipCancels, f.Takedowns = 2, 1 }))
	var got []string
	for _, r := range v.Rules {
		got = append(got, r.Code)
		if r.Figure == "" || r.High == "" || (r.Effect != "" && !slices.Contains(reputationLabels, r.Effect)) {
			t.Errorf("rule %+v", r)
		}
	}
	if !slices.Equal(got, codes) {
		t.Fatalf("rule rows %v, want %v", got, codes)
	}
	for i, want := range []string{"30 completed (buyer confirmed 20, release deadline 6, digital on funding 4) of 30 funded", "20 distinct buyers of completed orders (20 of funded orders)",
		"20 distinct buyers for 30 completed: 66%", "2 non-delivery refunds (buyer no-ship cancels 2, vendor cancellations of paid orders 0) against 30 completed: 6% of 32",
		"0 disputes lost (decided refund 0, vendor conceded 0; won: decided release 0, buyer conceded 0) against 30 completed: 0% of 30",
		"0 of 20 counted reviews at 2 stars or fewer: 0%", "1 listings now (taken down 1, rejected 0)"} {
		if v.Rules[i].Figure != want {
			t.Errorf("%s figure %q, want %q", v.Rules[i].Code, v.Rules[i].Figure, want)
		}
	}
	if v.Rules[3].High != "below 5%" || v.Rules[3].Lower != "AT_RISK at 3 or more and 20% or more" || v.Rules[0].Lower != "INSUFFICIENT_HISTORY below 5" {
		t.Fatalf("thresholds %+v %+v", v.Rules[3], v.Rules[0])
	}
	// The digest names the vendor and every figure.
	base := strongFigures()
	if reputationDigest("a", base) == reputationDigest("b", base) || len(reputationDigest("a", base)) != 32 {
		t.Fatal("the digest does not name the vendor")
	}
	for i := range 16 {
		changed := base
		*figureField(&changed, i)++
		if reputationDigest("a", changed) == reputationDigest("a", base) {
			t.Fatalf("the digest does not cover figure %d", i)
		}
	}
}

// figureField is the i-th of the 16 figures.
func figureField(f *ReputationFigures, i int) *int {
	return []*int{&f.Funded, &f.FundedBuyers, &f.CompletedConfirmed, &f.CompletedDeadline, &f.CompletedDigital, &f.CompletedBuyers, &f.NoShipCancels,
		&f.VendorCancels, &f.DecidedRefund, &f.DecidedRelease, &f.BuyerConceded, &f.VendorConceded, &f.Reviews, &f.LowReviews, &f.Takedowns, &f.Rejections}[i]
}

// TestReputationMonotonic: over generated consistent figures, worsening any adverse figure (one more non-delivery
// refund, lost dispute, low rating or removed listing, with or without the order that carries it, or one fewer distinct
// buyer) never improves the label, and a low-volume or concentrated record is never HIGH_REPUTATION.
func TestReputationMonotonic(t *testing.T) {
	r := rand.New(rand.NewPCG(653, 87))
	worsen := map[string]func(f *ReputationFigures){
		"no-ship cancel":           func(f *ReputationFigures) { f.NoShipCancels++ },
		"vendor cancellation":      func(f *ReputationFigures) { f.VendorCancels++ },
		"decided refund":           func(f *ReputationFigures) { f.DecidedRefund++ },
		"vendor concession":        func(f *ReputationFigures) { f.VendorConceded++ },
		"new low review":           func(f *ReputationFigures) { f.LowReviews++; f.Reviews++ },
		"review edited low":        func(f *ReputationFigures) { f.LowReviews = min(f.LowReviews+1, f.Reviews) },
		"takedown":                 func(f *ReputationFigures) { f.Takedowns++ },
		"rejection":                func(f *ReputationFigures) { f.Rejections++ },
		"one fewer distinct buyer": func(f *ReputationFigures) { f.CompletedBuyers = max(f.CompletedBuyers-1, min(f.Completed(), 1)) },
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range worsen {
			if !yield(k) {
				return
			}
		}
	})
	checked := map[string]int{}
	for range 20000 {
		n := func(hi int) int { return r.IntN(hi + 1) }
		var f ReputationFigures
		f.CompletedConfirmed, f.CompletedDeadline, f.CompletedDigital = n(40), n(10), n(10)
		if c := f.Completed(); c > 0 {
			f.CompletedBuyers = 1 + n(c-1)
		}
		f.NoShipCancels, f.VendorCancels, f.DecidedRefund, f.VendorConceded, f.DecidedRelease, f.BuyerConceded = n(6), n(6), n(6), n(6), n(4), n(4)
		f.Reviews = n(f.Completed())
		f.LowReviews = n(f.Reviews)
		f.Takedowns, f.Rejections = n(3), n(3)
		f.Funded = f.Completed() + f.Nondelivery() + f.DisputesLost() + f.DecidedRelease + n(5)
		f.FundedBuyers = f.CompletedBuyers + n(5)
		base := assessReputation("v", f)
		if base.Label == repHigh && (f.Completed() < repHighCompleted || f.CompletedBuyers < repHighBuyers || f.CompletedBuyers*100 < f.Completed()*repHighSpreadPct) {
			t.Fatalf("HIGH_REPUTATION below a floor: %+v", f)
		}
		for _, name := range names {
			for _, carried := range []bool{false, true} {
				g := f
				worsen[name](&g)
				if carried { // with the funded order, by a new buyer, that carries the outcome
					g.Funded++
					g.FundedBuyers++
				}
				got := assessReputation("v", g)
				if reputationRank(got.Label) > reputationRank(base.Label) {
					t.Fatalf("%s improved %s to %s: %+v -> %+v", name, base.Label, got.Label, f, g)
				}
				if got.Label != base.Label {
					checked[name]++
				}
			}
		}
	}
	for _, name := range names {
		if checked[name] == 0 {
			t.Errorf("no generated case where %q changed the label: the property is not exercised", name)
		}
	}
	if reputationRank(repHigh) <= reputationRank(repStandard) || reputationRank(repStandard) <= reputationRank(repInsufficient) || reputationRank(repInsufficient) <= reputationRank(repAtRisk) {
		t.Fatal("label order")
	}
}

// TestReputationSelfPurchaseConcentration: completions bought by one or a few accounts (sockpuppets) never reach
// HIGH_REPUTATION, whatever their reviews say.
func TestReputationSelfPurchaseConcentration(t *testing.T) {
	e := newTestApp(t)
	f := newRepFixture(e, "rep_puppet_vendor")
	puppet, _ := e.keylessUser("rep_puppet", "buyer")
	for range 30 {
		f.review(f.seed(puppet, "confirmed"), puppet, 5)
	}
	v := f.assess()
	if v.Figures.CompletedConfirmed != 30 || v.Figures.CompletedBuyers != 1 || v.Figures.Reviews != 30 || v.Label != repAtRisk || !slices.Equal(v.Reasons, []string{repConcentration}) {
		t.Fatalf("one buyer's 30 completions: %+v %s %v", v.Figures, v.Label, v.Reasons)
	}
	// A small ring of nine accounts with 30 completions between them is still never HIGH_REPUTATION.
	for i, b := range f.buyers("rep_ring_", 8) {
		for range 1 + i%2 {
			f.review(f.seed(b, "deadline"), b, 5)
		}
	}
	v = f.assess()
	if v.Figures.CompletedBuyers != 9 || v.Label == repHigh || !slices.Contains(v.Reasons, repConcentration) {
		t.Fatalf("a ring of nine buyers: %+v %s %v", v.Figures, v.Label, v.Reasons)
	}
	// Forty more distinct buyers dilute the concentration: the rule reads distinct buyers, not ratings.
	for _, b := range f.buyers("rep_real_", 40) {
		f.seed(b, "confirmed")
	}
	if v = f.assess(); v.Label != repHigh {
		t.Fatalf("49 buyers of 82 completions: %+v %s %v", v.Figures, v.Label, v.Reasons)
	}
}

// TestReputationIgnoresSettlementMechanics: each figure counts its own route over funded orders only; payment-window
// expiries, opened disputes, same-state settlement and decision notes and the settlements themselves (stray refunds,
// failed or superseded ones) change no figure.
func TestReputationIgnoresSettlementMechanics(t *testing.T) {
	e := newTestApp(t)
	f := newRepFixture(e, "rep_mech_vendor")
	buyers := f.buyers("rep_mech_", 4)
	orders := map[string]string{}
	for i, how := range []string{"confirmed", "deadline", "digital", "noship", "vendorcancel", "refund", "release", "buyerconceded", "vendorconceded", "open", "paid", "expired"} {
		orders[how] = f.seed(buyers[i%3], how)
	}
	f.review(orders["confirmed"], buyers[0], 1)
	f.review(orders["deadline"], buyers[1], 5)
	f.review(orders["digital"], buyers[2], 2)
	agExec(e, "UPDATE reviews SET hidden_at=now() WHERE order_id=$1", orders["digital"]) // hidden: not counted
	f.review(orders["buyerconceded"], buyers[0], 4)
	// A banned buyer's review is not counted (A-649's reviewCountedSQL).
	banned := buyers[3]
	f.review(f.seed(banned, "confirmed"), banned, 1)
	agExec(e, "UPDATE users SET banned_at=now(),suspended_at=now(),ban_reason='Groundless disputes on many orders' WHERE id=$1", banned)
	// Listings: one taken down, one rejected, one pending and one archived by the vendor (neither counts).
	for _, set := range []string{"archived=true,admin_archive_reason='Prohibited item listed here'", "review_state='rejected',review_rejection_reason='Rejected by staff'",
		"review_state='pending'", "archived=true"} {
		agExec(e, "UPDATE products SET "+set+" WHERE id=$1", e.product(f.vendorID, "physical"))
	}
	want := ReputationFigures{Funded: 12, FundedBuyers: 4, CompletedConfirmed: 2, CompletedDeadline: 1, CompletedDigital: 1, CompletedBuyers: 4,
		NoShipCancels: 1, VendorCancels: 1, DecidedRefund: 1, DecidedRelease: 1, BuyerConceded: 1, VendorConceded: 1, Reviews: 3, LowReviews: 1, Takedowns: 1, Rejections: 1}
	got := f.assess()
	if got.Figures != want {
		t.Fatalf("figures\n got %+v\nwant %+v", got.Figures, want)
	}

	// Settlement mechanics on the same orders: escrows with stray refunds and failed or superseded settlements, the
	// same-state notes they and a changed decision write, and a second expired payment window.
	wallet := xmrWallet(t, e, 1, "active")
	for i, how := range []string{"confirmed", "noship", "paid", "deadline"} {
		xmrEscrowOf(t, e, wallet, int64(i+1), orders[how])
		agExec(e, "INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1", orders[how])
	}
	agExec(e, "INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'refund',refund_address FROM escrows WHERE order_id=$1", orders["noship"])
	agExec(e, "UPDATE xmr_settlements SET state='failed' WHERE order_id=$1 AND kind='refund'", orders["noship"])
	agExec(e, "INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2)", orders["deadline"], xmrAddr("claim-rep"))
	agExec(e, "UPDATE xmr_settlements SET state='superseded' WHERE order_id=$1 AND kind='release'", orders["deadline"])
	for how, note := range map[string]string{"confirmed": "A stray deposit was refunded to the buyer", "noship": "The refund failed and waits for an administrator",
		"refund": "An administrator changed the dispute decision from refund to buyer to release to vendor: test", "paid": "Payment seen"} {
		var state string
		if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", orders[how]).Scan(&state); err != nil {
			t.Fatal(err)
		}
		agExec(e, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orders[how], state, note)
	}
	f.seed(buyers[0], "expired")
	if after := f.assess(); after.Figures != want || after.Digest != got.Digest {
		t.Fatalf("settlement mechanics changed the figures\n got %+v\nwant %+v", after.Figures, want)
	}
	// The figures read no settlement, payment or escrow table at all.
	for _, table := range []string{"settlements", "payments", "escrows", "payouts", "deposits"} {
		if regexp.MustCompile(`\b` + table + `\b`).MatchString(reputationSQL) {
			t.Fatalf("reputationSQL reads %s", table)
		}
	}
}

// TestReputationIndependentOfModerationCapture: the figures come only from marketplace rows. With every moderation_*
// table locked against reads by another transaction they are still computed, unchanged, and the reputation source
// neither names those tables nor imports package moderation.
func TestReputationIndependentOfModerationCapture(t *testing.T) {
	src, err := os.ReadFile("internal/market/reputation.go") // the tests run from the repository root (TestMain)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "reputation.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		if strings.Contains(imp.Path.Value, "moderation") {
			t.Fatalf("reputation.go imports %s", imp.Path.Value)
		}
	}
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(src), "") // comments may explain the boundary
	if strings.Contains(strings.ToLower(code), "moderation") {
		t.Fatal("reputation.go code names moderation")
	}
	e := newTestApp(t)
	f := newRepFixture(e, "rep_capture_vendor")
	f.seedStrong("rep_capture_")
	before := f.assess()
	tables := agStrings(t, e, "SELECT format('%I',tablename) FROM pg_tables WHERE schemaname=current_schema() AND tablename LIKE 'moderation%' ORDER BY 1")
	if len(tables) < 5 {
		t.Fatalf("moderation tables %v", tables)
	}
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("LOCK TABLE " + strings.Join(tables, ",") + " IN ACCESS EXCLUSIVE MODE NOWAIT"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	after, err := vendorReputation(ctx, e.DB, f.vendorID)
	if err != nil {
		t.Fatalf("the figures waited on a moderation table: %v", err)
	}
	if after.Figures != before.Figures || after.Label != repHigh {
		t.Fatalf("figures %+v %s, want %+v", after.Figures, after.Label, before.Figures)
	}
}

var reputationLeak = regexp.MustCompile(repHigh + `|` + repStandard + `|` + repInsufficient + `|` + repAtRisk + `|` + repStrongRecord + `|user-reputation|over retained history`)

// TestReputationAdminOnly: an administrator sees the figures, rule table and label on the vendor's account page; the
// vendor, a buyer, a moderator and a guest never see a label or figure there or on their own pages.
func TestReputationAdminOnly(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("rep_only_admin", "admin")
	f := newRepFixture(e, "rep_only_vendor")
	f.seedStrong("rep_only_")
	_, buyer := e.user("rep_only_buyer", "buyer")
	_, moderator := e.user("rep_only_mod", "moderator")
	page := e.body("GET", "/admin/users?handle=rep_only_vendor", admin, nil, 200)
	mustContain(t, page, `id="user-reputation"`, "over retained history", "nothing is stored", repHigh, repStrongRecord, repFewCompleted,
		"26 completed (buyer confirmed 18, release deadline 4, digital on funding 4) of 26 funded", "below 5%", "AT_RISK at 3 or more and 20% or more")
	// A buyer's account page has no reputation panel.
	mustNotContain(t, e.body("GET", "/admin/users?handle=rep_only_buyer", admin, nil, 200), `id="user-reputation"`)
	for who, s := range map[string]string{"vendor": f.vendorSess, "buyer": buyer, "moderator": moderator, "guest": randomToken()} {
		for _, path := range []string{"/admin/users?handle=rep_only_vendor", "/admin/vendors", "/vendor?id=" + f.vendorID, "/", "/account", "/notifications", "/moderator"} {
			w := e.do("GET", path, s, nil)
			if m := reputationLeak.FindString(w.Body.String()); m != "" {
				t.Fatalf("%s sees %q on %s (status %d)", who, m, path, w.Code)
			}
		}
	}
}

// TestAutoApproveConfirmRecomputesLabel: confirming the reputation when turning on auto-approval recomputes the label
// inside the action's transaction; a label that is not HIGH_REPUTATION or figures that changed since the page was
// shown are refused (409) and nothing changes. The administrator's audit row names the label only; the vendor's audit
// and notification never do. Turning auto-approval on without the choice keeps working.
func TestAutoApproveConfirmRecomputesLabel(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("rep_conf_admin", "admin")
	f := newRepFixture(e, "rep_conf_vendor")
	buyers := f.seedStrong("rep_conf_")
	userPage := func() string { return e.body("GET", "/admin/users?handle=rep_conf_vendor", admin, nil, 200) }
	confirmForm := func() url.Values {
		t.Helper()
		v := formOn(t, userPage(), "/admin/vendor-auto-approve", `name="confirm_reputation"`)
		v.Set("password", testPassword)
		v.Del(formNonceField)
		return v
	}
	flag := func() string { return agStr(e, "SELECT listing_auto_approve::text FROM users WHERE id=$1", f.vendorID) }
	form := confirmForm()
	if form.Get("action") != "on" || form.Get("reputation_digest") != f.assess().Digest {
		t.Fatalf("confirmation form %v", form)
	}
	// Stale: one more vendor cancellation leaves the label HIGH_REPUTATION but changes the figures.
	f.seed(buyers[0], "vendorcancel")
	if v := f.assess(); v.Label != repHigh {
		t.Fatalf("label %s", v.Label)
	}
	agExpect(e, "/admin/vendor-auto-approve", admin, form, 409, "figures changed since the page was shown")
	if flag() != "false" {
		t.Fatal("a stale confirmation set the flag")
	}
	// A fresh form confirms.
	form = confirmForm()
	w := e.do("POST", "/admin/vendor-auto-approve", admin, form)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/admin/users?handle=rep_conf_vendor#vendors" || flag() != "true" {
		t.Fatalf("redirect %q, flag %s", loc, flag())
	}
	if !e.auditExact(adminID, "Let rep_conf_vendor publish listings without review; reputation confirmed as HIGH_REPUTATION (confirmed with password)") ||
		!e.auditExact(f.vendorID, "Listings publish without review: set by administrator rep_conf_admin") {
		t.Fatal("audit rows")
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action ~ '[A-Z]{2,}_'", f.vendorID) +
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body ~ '[A-Z]{2,}_'", f.vendorID); n != 0 {
		t.Fatal("the vendor's audit trail or notifications name a reputation label")
	}
	if m := reputationLeak.FindString(e.body("GET", "/account", f.vendorSess, nil, 200)); m != "" {
		t.Fatalf("the vendor's account page shows %q", m)
	}
	// With auto-approval on, the page offers no confirmation; turn it off again through the existing form.
	mustNotContain(t, userPage(), `name="confirm_reputation"`)
	off := formOn(t, userPage(), "/admin/vendor-auto-approve", `name="action" value="off"`)
	off.Set("password", testPassword)
	e.check(e.do("POST", "/admin/vendor-auto-approve", admin, off), 303)

	// Not HIGH_REPUTATION: a listing taken down makes it STANDARD. The page offers no confirmation, and a confirmation
	// with the current digest is refused.
	agExec(e, "UPDATE products SET archived=true,admin_archive_reason='Prohibited item listed here' WHERE id=$1", f.physical)
	v := f.assess()
	if v.Label != repStandard {
		t.Fatalf("label %s %v", v.Label, v.Reasons)
	}
	page := userPage()
	mustContain(t, page, repStandard, repRemovals)
	mustNotContain(t, page, `name="confirm_reputation"`)
	form.Set("reputation_digest", v.Digest)
	agExpect(e, "/admin/vendor-auto-approve", admin, form, 409, "rep_conf_vendor's reputation label is STANDARD, not HIGH_REPUTATION. Nothing was changed.")
	if flag() != "false" {
		t.Fatal("a not-HIGH confirmation set the flag")
	}
	// The choice applies only to turning auto-approval on, and needs the digest.
	agExpect(e, "/admin/vendor-auto-approve", admin, withField(form, "action", "off"), 400, "Nothing was changed")
	agExpect(e, "/admin/vendor-auto-approve", admin, withField(form, "reputation_digest", ""), 400, "Nothing was changed")
	// Without the choice, auto-approval turns on as before, whatever the label.
	plain := withField(form, "action", "on")
	plain.Del("confirm_reputation")
	plain.Del("reputation_digest")
	e.check(e.do("POST", "/admin/vendor-auto-approve", admin, plain), 303)
	if flag() != "true" || !e.auditExact(adminID, "Let rep_conf_vendor publish listings without review (confirmed with password)") {
		t.Fatal("auto-approval without the reputation choice")
	}
	// No bulk confirmation: the vendors page's handle form has no reputation choice.
	mustNotContain(t, e.body("GET", "/admin/vendors", admin, nil, 200), "confirm_reputation")
}

// TestAutoApproveOnlyWriters: vendorAutoApproveAction is the only writer of users.listing_auto_approve apart from the
// ban action clearing it (A-650); no migration after 145 and no database function writes it.
func TestAutoApproveOnlyWriters(t *testing.T) {
	allowed := map[string]bool{"internal/market/listing_review.go:vendorAutoApproveAction": true, "internal/market/admin_ban.go:adminBanAction": true}
	write := regexp.MustCompile(`(?i)listing_auto_approve\s*=|insert\s+into\s+users\s*\([^)]*listing_auto_approve|listing_auto_approve\s*\)\s*=`)
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !write.Match(src) {
				return nil
			}
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				body := src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset]
				if write.Match(body) {
					found[filepath.ToSlash(path)+":"+fn.Name.Name] = true
				}
			}
			for _, decl := range file.Decls {
				if g, ok := decl.(*ast.GenDecl); ok && write.Match(src[fset.Position(g.Pos()).Offset:fset.Position(g.End()).Offset]) {
					found[filepath.ToSlash(path)+":"+g.Tok.String()] = true // a package-level SQL constant naming the write
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for w := range found {
		if !allowed[w] {
			t.Errorf("%s writes listing_auto_approve: only vendorAutoApproveAction and the ban action may (A-653)", w)
		}
	}
	for w := range allowed {
		if !found[w] {
			t.Errorf("the guard no longer finds %s's write: update it", w)
		}
	}
	migrations, err := filepath.Glob("internal/market/migrations/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("no migrations", err)
	}
	for _, m := range migrations {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "listing_auto_approve") && filepath.Base(m) != "145_listing_review.sql" {
			t.Errorf("%s names listing_auto_approve", m)
		}
	}
	// A probe: the pattern sees an UPDATE ... SET written either way.
	for _, probe := range []string{"UPDATE users SET listing_auto_approve=true", "UPDATE users SET role='x', listing_auto_approve = $2",
		"UPDATE users SET (role,listing_auto_approve)=('vendor',true)", "INSERT INTO users(id,handle,listing_auto_approve) VALUES($1,$2,true)"} {
		if !write.MatchString(probe) {
			t.Fatalf("the guard misses %q", probe)
		}
	}
	e := newTestApp(t) // skips without TEST_DATABASE_URL, after the source checks above
	if n := agInt(e, "SELECT count(*) FROM pg_proc WHERE prosrc LIKE '%listing_auto_approve%'"); n != 0 {
		t.Fatalf("%d database functions or triggers name listing_auto_approve", n)
	}
}
