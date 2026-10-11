package market

import (
	"slices"
	"strings"
	"testing"
)

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

	for _, tc := range []struct {
		state, kind string
		u           *User
		want        []string
	}{
		{stateDraft, "physical", buyer, []string{stateCancelled}},
		{stateDraft, "physical", vendor, nil},
		{stateAwaitingPayment, "physical", buyer, nil},
		{stateAwaitingPayment, "physical", vendor, nil},
		{statePaid, "physical", vendor, nil},
		{statePaid, "digital", vendor, nil},
		{statePaid, "service", vendor, nil},
		{statePaid, "physical", buyer, nil},
		{stateShipped, "physical", buyer, nil},
		{stateShipped, "physical", vendor, nil},
		{stateDelivered, "digital", buyer, nil},
		{stateDisputed, "physical", mod, nil}, // resolved on the moderator desk, not the order page
		{stateDisputed, "physical", buyer, nil},
		{statePaid, "physical", stranger, nil},
		{stateCompleted, "physical", buyer, nil},
		{stateCancelled, "physical", vendor, nil},
		{stateResolved, "physical", buyer, nil},
	} {
		o := &Order{State: tc.state, Kind: tc.kind, BuyerID: "b", VendorID: "v", Currency: "BTC"}
		got := viewerTransitions(o, tc.u)
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
