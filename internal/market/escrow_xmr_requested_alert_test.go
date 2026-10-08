package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

func TestXMREscrowRequestedSettlementStaffAlertAfter24Hours(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, mod := x.user("reqalert_"+randomToken()[:6], "moderator")
	old, _, _ := x.completed()
	young, _, _ := x.completed()
	for _, order := range []string{old, young} {
		agExec(x.testEnv, "INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2)", order, testXMRAddr("requested alert "+order, true))
	}
	agExec(x.testEnv, "UPDATE xmr_settlements SET created=now()-interval '25 hours' WHERE order_id=$1", old)
	agExec(x.testEnv, "UPDATE xmr_settlements SET created=now()-interval '23 hours' WHERE order_id=$1", young)
	notices := agInt(x.testEnv, "SELECT count(*) FROM notifications")

	desk := html.UnescapeString(x.page("/moderator", mod))
	oldAlert := deskAlertIn(desk, old)
	if !strings.Contains(oldAlert, "Monero escrow settlement has been requested for more than 24 hours") {
		t.Fatalf("older requested settlement is missing its staff alert: %q", oldAlert)
	}
	if got := deskAlertIn(desk, young); got != "" {
		t.Fatalf("a 23-hour settlement raised an alert: %q", got)
	}
	if got := x.do("GET", "/order?id="+url.QueryEscape(old), mod, nil); got.Code != 200 || !strings.Contains(got.Body.String(), "Monero escrow settlement has been requested for more than 24 hours") {
		t.Fatalf("staff could not read the alerting order (%d): %s", got.Code, got.Body.String())
	}
	if got := x.do("GET", "/order?id="+url.QueryEscape(young), mod, nil); got.Code != 404 {
		t.Fatalf("a sub-24-hour order became readable to staff: %d", got.Code)
	}
	if after := agInt(x.testEnv, "SELECT count(*) FROM notifications"); after != notices {
		t.Fatalf("the read-only alert query sent %d new notifications", after-notices)
	}
}
