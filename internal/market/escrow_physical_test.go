package market

import (
	"net/url"
	"regexp"

	"sync"
	"testing"
	"time"
)

func physString(t *testing.T, e *testEnv, q string, args ...any) string {
	t.Helper()
	var s string
	if err := e.DB.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// physicalListing is a physical listing of vendor with a dispatch window of days (0 = none set).
func (x *escrowEnv) physicalListing(vendor string, days int) string {
	x.t.Helper()
	id := x.testEnv.product(vendor, "physical")
	if days > 0 {
		agExec(x.testEnv, "UPDATE products SET dispatch_days=$2 WHERE id=$1", id, days)
	}
	return id
}

// physicalDraft is the buyer's BTC draft on a new physical listing with a dispatch window of days, whose vendor can
// receive encrypted messages.
func (x *escrowEnv) physicalDraft(days int) string {
	x.t.Helper()
	_, pub := testPGPKey(x.t, "physvendor"+randomToken()[:6])
	x.setKey(x.vendor, pub)
	return x.testEnv.order(x.buyer, x.physicalListing(x.vendor, days), "BTC", stateDraft)
}

// snapshots is the escrow's release_days, dispatch_days and whether its deadline columns are still unset.
func (x *escrowEnv) snapshots(order string) string {
	x.t.Helper()
	return physString(x.t, x.testEnv, `SELECT coalesce(release_days::text,'-')||' '||coalesce(dispatch_days::text,'-')||' '||(ship_by IS NULL)||' '||(release_at IS NULL)||' '||(reminded_at IS NULL)||' '||release_recheck
		FROM escrows WHERE order_id=$1`, order)
}

// concurrentSaves has six administrators post the same escrow setting at once, each with its own value, on a database
// where it was never saved, and checks that the saves were serialised (A-379): every audit row's old value is the
// previous row's new value, the first is the value in force before any save, and the stored value is the last new one.
func (x *escrowEnv) concurrentSaves(path, auditLike string, chain *regexp.Regexp, forms []url.Values, first string) []string {
	x.t.Helper()
	var sessions []string
	for range forms {
		_, s := x.user("eadm_"+randomToken()[:6], "admin")
		sessions = append(sessions, s)
	}
	var wg sync.WaitGroup
	codes := make([]int, len(forms))
	start := make(chan struct{})
	for i := range forms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// The password check has four slots and refuses a fifth caller with 503 before anything is counted or
			// locked; such a request is sent again, so all six reach the setting's lock.
			for try := 0; try < 100; try++ {
				if codes[i] = x.do("POST", path, sessions[i], forms[i]).Code; codes[i] != 503 {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	close(start)
	wg.Wait()
	for i, c := range codes {
		if c != 303 {
			x.t.Fatalf("admin %d: status %d", i, c)
		}
	}
	rows := agStrings(x.t, x.testEnv, "SELECT action FROM audit_events WHERE action LIKE $1 ORDER BY id", auditLike)
	if len(rows) != len(forms) {
		x.t.Fatalf("%d audit rows, want %d: %q", len(rows), len(forms), rows)
	}
	prev := first
	var news []string
	for _, r := range rows {
		m := chain.FindStringSubmatch(r)
		if m == nil || m[1] != prev {
			x.t.Fatalf("audit row %q: old value is not %q (the previous save's new value); rows %q", r, prev, rows)
		}
		prev = m[2]
		news = append(news, prev)
	}
	return news
}
