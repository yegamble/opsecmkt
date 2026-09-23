package market

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"testing"
)

func TestTransitionTableShape(t *testing.T) {
	for _, s := range orderStates {
		if _, ok := transitions[s]; !ok {
			t.Errorf("state %s missing from transitions", s)
		}
		if stateLabel(s) == s {
			t.Errorf("state %s has no label", s)
		}
	}
	for _, s := range []string{stateCompleted, stateResolved, stateCancelled} {
		if len(transitions[s]) != 0 {
			t.Errorf("terminal state %s has exits", s)
		}
	}
	reached := map[string]bool{stateDraft: true}
	queue := []string{stateDraft}
	for len(queue) > 0 {
		for to := range transitions[queue[0]] {
			if !slices.Contains(orderStates, to) {
				t.Errorf("unknown target state %s", to)
			}
			if !reached[to] {
				reached[to] = true
				queue = append(queue, to)
			}
		}
		queue = queue[1:]
	}
	if len(reached) != len(orderStates) {
		t.Errorf("unreachable states: reached %v", reached)
	}
	for from, tos := range transitions {
		for to, roles := range tos {
			if to == statePaid && !slices.Equal(roles, []string{roleSystem}) {
				t.Errorf("%s -> paid allowed for %v; only the payment watcher (system) may mark orders paid", from, roles)
			}
		}
	}
	o := &Order{BuyerID: "b", VendorID: "v"}
	for _, tc := range []struct {
		u    *User
		want []string
	}{
		{nil, []string{roleSystem}},
		{&User{ID: "b", Role: "buyer"}, []string{roleBuyer}},
		{&User{ID: "v", Role: "admin"}, []string{roleVendor}},
		{&User{ID: "m", Role: "moderator"}, []string{roleModerator}},
		{&User{ID: "x", Role: "buyer"}, nil},
	} {
		if got := actorRoles(o, tc.u); !slices.Equal(got, tc.want) {
			t.Errorf("actorRoles(%+v) = %v want %v", tc.u, got, tc.want)
		}
	}
}

type orderFixture struct {
	e                    *testEnv
	buyer, vendor, other *User
	order                string
}

func newOrderFixture(t *testing.T, kind, state string) *orderFixture {
	e := newTestApp(t)
	b, _ := e.user("buyer_"+randomToken()[:6], "buyer")
	v, _ := e.user("vendor_"+randomToken()[:6], "vendor")
	x, _ := e.user("other_"+randomToken()[:6], "buyer")
	f := &orderFixture{e: e, buyer: e.loadUser(b), vendor: e.loadUser(v), other: e.loadUser(x)}
	f.order = e.order(b, e.product(v, kind), "BTC", state)
	return f
}

func (f *orderFixture) try(from, to string, actor *User) error {
	ctx := context.Background()
	tx, err := f.e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = f.e.A.transition(ctx, tx, f.order, from, to, actor, "test"); err != nil {
		return err
	}
	return tx.Commit()
}

func TestTransitionRulesAndSideEffects(t *testing.T) {
	f := newOrderFixture(t, "physical", stateDraft)
	if err := f.try(stateDraft, stateCancelled, f.vendor); !errors.Is(err, errForbidden) {
		t.Fatalf("vendor cancelled a draft: %v", err)
	}
	if err := f.try(stateDraft, stateCancelled, f.other); !errors.Is(err, errForbidden) {
		t.Fatalf("stranger cancelled a draft: %v", err)
	}
	if err := f.try(stateDraft, stateAwaitingPayment, f.buyer); err == nil || err.Error() != "Payment unavailable for BTC" {
		t.Fatalf("payment step without provider: %v", err)
	}
	if err := f.try(stateAwaitingPayment, statePaid, nil); !errors.Is(err, errConflict) {
		t.Fatalf("stale from-state: %v", err)
	}
	if err := f.try(stateDraft, stateCompleted, f.buyer); !errors.Is(err, errForbidden) {
		t.Fatalf("move outside the table: %v", err)
	}
	var hooked []string
	transitionHooks = append(transitionHooks, func(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
		hooked = append(hooked, from+">"+to+":"+o.State)
		return nil
	})
	t.Cleanup(func() { transitionHooks = transitionHooks[:len(transitionHooks)-1] })
	if err := f.try(stateDraft, stateCancelled, f.buyer); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hooked, []string{"draft>cancelled:cancelled"}) {
		t.Fatalf("hooks saw %v", hooked)
	}
	var state, actor, from string
	var notes int
	f.e.DB.QueryRow("SELECT o.state,e.actor_id,e.from_state FROM orders o JOIN order_events e ON e.order_id=o.id WHERE o.id=$1", f.order).Scan(&state, &actor, &from)
	f.e.DB.QueryRow("SELECT count(*) FROM notifications WHERE user_id=$1", f.vendor.ID).Scan(&notes)
	if state != stateCancelled || actor != f.buyer.ID || from != stateDraft || notes != 1 {
		t.Fatalf("state=%s actor=%s from=%s vendor notifications=%d", state, actor, from, notes)
	}
	if err := f.try(stateCancelled, stateDraft, f.buyer); !errors.Is(err, errForbidden) {
		t.Fatalf("terminal state left: %v", err)
	}

	g := newOrderFixture(t, "digital", statePaid)
	if err := g.try(statePaid, stateShipped, g.vendor); err == nil || errors.Is(err, errConflict) {
		t.Fatalf("digital order shipped: %v", err)
	}
	if err := g.try(statePaid, stateDelivered, nil); err != nil {
		t.Fatalf("system delivery: %v", err)
	}
	if err := g.try(stateDelivered, stateCompleted, g.vendor); !errors.Is(err, errForbidden) {
		t.Fatalf("vendor completed: %v", err)
	}
	if err := g.try(stateDelivered, stateCompleted, g.buyer); err != nil {
		t.Fatal(err)
	}
}

// Many concurrent identical transitions: exactly one wins, one event row is written.
func TestTransitionCompareAndSetIsExclusive(t *testing.T) {
	f := newOrderFixture(t, "physical", stateDraft)
	f.e.DB.SetMaxOpenConns(20)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.try(stateDraft, stateCancelled, f.buyer)
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, errConflict):
			t.Errorf("unexpected error %v", err)
		}
	}
	var events int
	f.e.DB.QueryRow("SELECT count(*) FROM order_events WHERE order_id=$1", f.order).Scan(&events)
	if ok != 1 || events != 1 {
		t.Fatalf("successes=%d events=%d", ok, events)
	}
}
