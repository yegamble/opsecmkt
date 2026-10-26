package market

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
)

// A-636: TestHistoricalOrderActionMatrix's approach on Monero escrow orders. Every state-changing order action is
// posted, with a complete form, by the buyer, the vendor, an unrelated buyer, an independent moderator and an
// independent administrator on a Monero escrow order in each state it can be in, and the exact status is pinned. A
// refusal must leave the order, its escrow, slot, settlements and dispute, the audit rows, notifications and wallet-rpc
// calls as they were; a success is followed by a fresh order for the next actor. The order page's own status
// (GET /order) is pinned beside the actions: staff read only disputed, resolved and flagged orders, and even then every
// party action answers them 404. The flagged state's flag is set directly (as the watcher sets it on a deposit that
// needs review); a digital order delivered by hand completes at once, so "delivered" is not a resting state of a
// Monero escrow order. The correspondence actions (/orders/message, /orders/delivery-update) are left to
// TestXMREscrowOrderFormsNeverConflict and TestActionRegistryOwnershipSweep. DB integration against the simulated
// Monero wallet-rpc (escrow_xmr_fake_test.go): simulated RPC, not chain evidence.
func TestXMREscrowOrderActionMatrix(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET stock=1000 WHERE id=$1", physical)
	strangerID, stranger := x.user("xm_stranger_"+randomToken()[:6], "buyer")
	x.enableOrderPGP(strangerID)
	_, moderator := x.user("xm_mod_"+randomToken()[:6], "moderator")
	_, admin := x.user("xm_admin_"+randomToken()[:6], "admin")
	agExec(x.testEnv, "UPDATE users SET role='buyer' WHERE id=$1", x.admin) // one independent administrator: admin

	dispute := func(order string) {
		if code, body := x.xmrDispute(order, x.sessOf(order, roleBuyer)); code != 303 {
			t.Fatalf("dispute: %d %s", code, body)
		}
	}
	// Each builder returns a fresh Monero escrow order in its state, on a fresh buyer (the awaiting-payment cap).
	states := []struct {
		name  string
		state string
		build func() string
	}{
		{"draft", stateDraft, func() string {
			buyer, _ := x.user("xm_b_"+randomToken()[:8], "buyer")
			x.enableOrderPGP(buyer)
			return x.order(buyer, physical, "XMR", stateDraft)
		}},
		{"awaiting payment", stateAwaitingPayment, func() string { order, _ := x.freshCheckout(physical); return order }},
		{"awaiting payment with a deposit", stateAwaitingPayment, func() string {
			order, addr := x.freshCheckoutOf(physical, 2) // one unit's price paid: an underpayment
			x.fake.payAddress(addr, hex64("part "+order), xmrListingPrice, 10)
			x.mustPoll()
			return order
		}},
		{"paid digital", statePaid, func() string { order, _ := x.undeliveredXMR(); return order }},
		{"paid digital with a flagged deposit", statePaid, func() string {
			order, _ := x.undeliveredXMR()
			agExec(x.testEnv, "UPDATE payments SET flagged=true WHERE order_id=$1", order)
			return order
		}},
		{"paid physical", statePaid, func() string { order, _ := x.paidXMRPhysical(physical); return order }},
		{"shipped", stateShipped, func() string { order, _, _ := x.shippedXMR(physical); return order }},
		{"completed", stateCompleted, func() string { order, _, _ := x.completed(); return order }},
		{"disputed", stateDisputed, func() string { order, _ := x.undeliveredXMR(); dispute(order); return order }},
		{"resolved for the vendor", stateResolved, func() string {
			order, _ := x.undeliveredXMR()
			x.disputeAndResolve(order, "release")
			return order
		}},
		{"cancelled", stateCancelled, func() string {
			order, _ := x.freshCheckout(physical)
			if code, body := x.xmrPost("/orders/cancel", x.sessOf(order, roleBuyer), url.Values{"order_id": {order}, "from": {stateAwaitingPayment}}); code != 303 {
				t.Fatalf("cancel: %d %s", code, body)
			}
			return order
		}},
	}
	actions := []string{"/orders/escrow-xmr", "/orders/cancel", "/orders/ship", "/orders/deliver", "/orders/complete", "/disputes", "/disputes/reply",
		"/disputes/concede", "/resolve", "/escrow-xmr/ship", "/escrow-xmr/claim", "/escrow-xmr/confirm"}
	actors := []string{"stranger", "moderator", "admin", roleVendor, roleBuyer}
	// want[state][action] is the status for each of actors, in order.
	want := map[string]map[string][5]int{
		"draft": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 303},
			"/orders/cancel":      {404, 404, 404, 403, 303},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 409, 409},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 404, 404},
			"/escrow-xmr/claim":   {404, 404, 404, 404, 404},
			"/escrow-xmr/confirm": {404, 404, 404, 404, 404},
		},
		"awaiting payment": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 303, 303},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 409, 409},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"awaiting payment with a deposit": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 303, 303},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 409, 409},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"paid digital": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 303, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 303, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 303, 303},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"paid digital with a flagged deposit": {
			"GET /order":          {404, 200, 200, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 303, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 303, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 303, 303},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"paid physical": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 303, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 303, 303},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 303, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"shipped": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 409, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 403, 409},
			"/disputes":           {404, 404, 404, 303, 303},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 303},
		},
		"completed": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 409, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 409, 409},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 303, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"disputed": {
			"GET /order":          {404, 200, 200, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 409, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 409, 409},
			"/disputes/reply":     {404, 404, 404, 303, 403},
			"/disputes/concede":   {403, 403, 403, 303, 303},
			"/resolve":            {403, 403, 303, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"resolved for the vendor": {
			"GET /order":          {404, 200, 200, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 409, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 409, 409},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {403, 403, 403, 409, 409},
			"/resolve":            {403, 403, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 303, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
		"cancelled": {
			"GET /order":          {404, 404, 404, 200, 200},
			"/orders/escrow-xmr":  {404, 404, 404, 403, 409},
			"/orders/cancel":      {404, 404, 404, 409, 409},
			"/orders/ship":        {404, 404, 404, 409, 409},
			"/orders/deliver":     {404, 404, 404, 409, 403},
			"/orders/complete":    {404, 404, 404, 409, 409},
			"/disputes":           {404, 404, 404, 409, 409},
			"/disputes/reply":     {404, 404, 404, 409, 409},
			"/disputes/concede":   {404, 404, 404, 404, 404},
			"/resolve":            {403, 409, 409, 403, 403},
			"/escrow-xmr/ship":    {404, 404, 404, 409, 403},
			"/escrow-xmr/claim":   {404, 404, 404, 409, 403},
			"/escrow-xmr/confirm": {404, 404, 404, 403, 409},
		},
	}

	session := func(order, actor string) string {
		switch actor {
		case "stranger":
			return stranger
		case "moderator":
			return moderator
		case "admin":
			return admin
		}
		return x.sessOf(order, actor)
	}
	form := func(order, state, actor string) url.Values {
		id := order
		if d := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order); len(d) == 1 {
			id = d[0]
		}
		outcome := "release" // the buyer's concession; the vendor's is a refund
		if actor == roleVendor {
			outcome = "refund"
		}
		seen := agStrings(t, x.testEnv, "SELECT COALESCE(sum(amount),0)::text FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close", order)[0]
		from := state // a cancel form names the state it was shown in; one from a state that cannot be cancelled is a stale paid one
		if !slices.Contains([]string{stateDraft, stateAwaitingPayment, statePaid}, state) {
			from = statePaid
		}
		return url.Values{"order_id": {order}, "id": {id}, "from": {from}, "refund_address": {testXMRAddrOn(x.refundNet(), "matrix refund "+randomToken(), true)},
			"address": {testXMRAddrOn(x.refundNet(), "matrix claim "+randomToken(), true)}, "reason": {"The parcel never arrived at the address I sent."},
			"reply": {"The parcel was handed to the carrier on time with tracking."}, "content": {"Licence key XC-5678 for this order."},
			"outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"},
			"payout_confirmed": {"confirmed"}, "amount_seen": {seen}, "password": {testPassword}}
	}
	discover := os.Getenv("A636_DISCOVER") != "" // log every outcome and the table it gives, instead of comparing
	var table strings.Builder
	for _, s := range states {
		fresh := func() string {
			x.mustPoll() // keep free slots for the checkouts
			order := s.build()
			if x.state(order) != s.state {
				t.Fatalf("%s: the fixture order is %s", s.name, x.state(order))
			}
			return order
		}
		order, moved := fresh(), false
		for _, action := range append([]string{"GET /order"}, actions...) {
			var got [5]int
			for i, actor := range actors {
				if moved {
					order = fresh() // the previous success moved this one on
				}
				x.A.mu.Lock() // the per-account write limit is not under test: this sweep posts some 600 forms
				x.A.writeLimits = map[string]bucket{}
				x.A.mu.Unlock()
				before := x.footprint(order)
				var code int
				var body string
				if action == "GET /order" {
					r := x.do("GET", "/order?id="+order, session(order, actor), nil)
					code, body = r.Code, ""
				} else {
					code, body = x.xmrPost(action, session(order, actor), form(order, s.state, actor))
				}
				got[i], moved = code, code == 303
				after := x.footprint(order)
				switch {
				case code >= 500:
					t.Errorf("%s, %s as %s: %d %s", s.name, action, actor, code, body)
				case code != 303 && after != before:
					t.Errorf("%s, %s as %s: refused %d but changed the order: %s -> %s", s.name, action, actor, code, before, after)
				case code == 303 && after == before:
					t.Errorf("%s, %s as %s: 303 without a recorded change", s.name, action, actor)
				}
				if discover {
					t.Logf("%s | %s | %s | %d | %.120s", s.name, action, actor, code, body)
				}
			}
			fmt.Fprintf(&table, "\t\t%q: {%d, %d, %d, %d, %d},\n", action, got[0], got[1], got[2], got[3], got[4])
			if w, ok := want[s.name][action]; !discover && (!ok || w != got) {
				t.Errorf("%s, %s: statuses %v for %v, want %v", s.name, action, got, actors, w)
			}
		}
		if discover {
			t.Logf("%q: {\n%s}", s.name, table.String())
			table.Reset()
		}
	}
}
