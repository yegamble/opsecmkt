package market

import (
	"net/url"
	"strings"
	"testing"
)

// With Monero escrow configured and Bitcoin escrow off, a physical Monero escrow order still snapshots the release
// deadline length and its listing's dispatch window (P3-R7), so the administrator's release-days setting on /admin and
// the vendor's dispatch field are offered as they are with Bitcoin escrow on.
func TestXMREscrowOnlyReleaseDaysAndDispatch(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll() // the watcher creates the month's wallet and its free slots
	if x.A.escrowConfigured {
		t.Fatal("the Monero escrow environment has Bitcoin escrow on")
	}
	_, admin := x.user("xadmin2_"+randomToken()[:6], "admin")
	page := x.page("/admin", admin)
	for _, want := range []string{`action="/admin/escrow-release-days"`, `name="release_days"`, "14 days"} {
		if !strings.Contains(page, want) {
			t.Fatalf("admin page lacks %q with only Monero escrow on", want)
		}
	}
	if w := x.do("POST", "/admin/escrow-release-days", admin, url.Values{"release_days": {"21"}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("saving the release deadline with only Monero escrow on: %d %s", w.Code, w.Body.String())
	}
	if got := agStr(x.testEnv, "SELECT action FROM audit_events WHERE action LIKE 'Changed the % escrow release deadline%' ORDER BY id DESC LIMIT 1"); !strings.Contains(got, "Changed the Monero escrow release deadline (fakechain)") || strings.Contains(got, "Bitcoin") {
		t.Fatalf("Monero release deadline audit named the wrong currency: %q", got)
	}
	if days, err := escrowReleaseDays(t.Context(), x.DB); err != nil || days != 21 {
		t.Fatalf("release deadline %d (%v), want 21", days, err)
	}
	product := x.physical(x.vendor)
	for _, path := range []string{"/vendor-dashboard", "/listing-edit?id=" + product} {
		body := x.do("GET", path, x.vendorSess, nil).Body.String()
		for _, want := range []string{`name="dispatch_days"`, "Dispatch within (days)", `id="dispatch-help"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s lacks %q with only Monero escrow on", path, want)
			}
		}
	}
	order, _ := x.paidXMRPhysical(product)
	var days string
	if err := x.DB.QueryRow("SELECT release_days::text FROM escrows WHERE order_id=$1", order).Scan(&days); err != nil || days != "21" {
		t.Fatalf("the Monero escrow snapshotted release_days %q (%v), want 21", days, err)
	}
}
