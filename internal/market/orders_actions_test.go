package market

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// stubProvider is the P3 test double for a payment provider (P5 owns the real fake).
type stubProvider struct {
	currency string
	broken   bool
	created  []string
}

func (s *stubProvider) Currency() string   { return s.currency }
func (s *stubProvider) Network() string    { return "regtest" }
func (s *stubProvider) Confirmations() int { return 1 }
func (s *stubProvider) NewAddress(_ context.Context, orderID string) (string, error) {
	if s.broken {
		return "", errors.New("wallet offline")
	}
	s.created = append(s.created, orderID)
	return "stub-" + orderID[:8], nil
}
func (s *stubProvider) Incoming(context.Context, []string) ([]Incoming, error) { return nil, nil }
func (s *stubProvider) Send(context.Context, string, int64) (string, error) {
	return "", errors.New("stub cannot send")
}
func (s *stubProvider) ValidAddress(addr string) bool { return strings.HasPrefix(addr, "stub-") }

func targets(ts []Transition) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.To)
	}
	return out
}

func TestViewerTransitionsFollowTheStateTable(t *testing.T) {
	buyer, vendor := &User{ID: "b", Role: "buyer"}, &User{ID: "v", Role: "vendor"}
	mod, stranger := &User{ID: "m", Role: "moderator"}, &User{ID: "x", Role: "buyer"}
	withBTC := map[string]PaymentProvider{"BTC": &stubProvider{currency: "BTC"}}
	for _, tc := range []struct {
		state, kind string
		u           *User
		payments    map[string]PaymentProvider
		want        []string
	}{
		{stateDraft, "physical", buyer, withBTC, []string{stateAwaitingPayment, stateCancelled}},
		{stateDraft, "physical", vendor, withBTC, nil},
		{stateAwaitingPayment, "physical", buyer, withBTC, []string{stateCancelled}},
		{stateAwaitingPayment, "physical", vendor, withBTC, []string{stateCancelled}},
		{statePaid, "physical", vendor, nil, []string{stateShipped, stateDisputed, stateCancelled}},
		{statePaid, "digital", vendor, nil, []string{stateDelivered, stateDisputed, stateCancelled}},
		{statePaid, "service", vendor, nil, []string{stateDisputed, stateCancelled}},
		{statePaid, "physical", buyer, nil, []string{stateDisputed}},
		{stateShipped, "physical", buyer, nil, []string{stateCompleted, stateDisputed}},
		{stateShipped, "physical", vendor, nil, []string{stateDisputed}},
		{stateDelivered, "digital", buyer, nil, []string{stateCompleted, stateDisputed}},
		{stateDisputed, "physical", mod, nil, nil}, // resolved on the moderator desk, not the order page
		{stateDisputed, "physical", buyer, nil, nil},
		{statePaid, "physical", stranger, nil, nil},
		{stateCompleted, "physical", buyer, nil, nil},
		{stateCancelled, "physical", vendor, nil, nil},
		{stateResolved, "physical", buyer, nil, nil},
	} {
		o := &Order{State: tc.state, Kind: tc.kind, BuyerID: "b", VendorID: "v", Currency: "BTC"}
		got := viewerTransitions(tc.payments, o, tc.u)
		if !slices.Equal(targets(got), tc.want) {
			t.Errorf("%s/%s as %s: got %v want %v", tc.state, tc.kind, tc.u.ID, targets(got), tc.want)
		}
		for _, tr := range got {
			allowed := transitions[tc.state][tr.To]
			if !slices.ContainsFunc(actorRoles(o, tc.u), func(r string) bool { return slices.Contains(allowed, r) }) {
				t.Errorf("%s -> %s offered to %s but the table forbids it", tc.state, tr.To, tc.u.ID)
			}
			if tr.Action == "" || tr.Label == "" {
				t.Errorf("%s -> %s has no action/label", tc.state, tr.To)
			}
		}
	}
	// Without a provider the payment step is shown, but as unavailable (no form action).
	got := viewerTransitions(nil, &Order{State: stateDraft, Kind: "physical", BuyerID: "b", VendorID: "v", Currency: "XMR"}, buyer)
	if len(got) != 2 || got[0].To != stateAwaitingPayment || got[0].Action != "" || !strings.Contains(got[0].Label, "Payment unavailable for XMR") {
		t.Fatalf("unavailable payment step: %+v", got)
	}
	for to, path := range transitionActions {
		if _, ok := actions[path]; !ok {
			t.Errorf("transition to %s points at unregistered action %s", to, path)
		}
	}
}

func TestReviewStarsAndHelpers(t *testing.T) {
	for rating, want := range map[int]string{1: "★☆☆☆☆", 4: "★★★★☆", 5: "★★★★★", 0: "☆☆☆☆☆", 9: "★★★★★"} {
		if got := (Review{Rating: rating}).Stars(); got != want {
			t.Errorf("Stars(%d) = %s", rating, got)
		}
	}
	if shortID("abc") != "abc" || shortID(strings.Repeat("a", 64)) != "aaaaaaaa" {
		t.Error("shortID")
	}
	if len(disputeOutcomes) != 2 || disputeOutcomes["release"] == "" || disputeOutcomes["refund"] == "" {
		t.Error("dispute outcomes must be exactly release and refund (matches the disputes.outcome CHECK)")
	}
}
