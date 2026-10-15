package market

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func staffRequestEffects(e *testEnv, order, actor string) [4]int {
	return [4]int{agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1", order),
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", actor),
		agInt(e, "SELECT count(*) FROM notifications"), agInt(e, "SELECT count(*) FROM form_submissions")}
}

func assertStaffRequestLostAck(t *testing.T, status int, retry, body string, pending bool) {
	t.Helper()
	if pending || status != 500 || retry != "" || !strings.Contains(body, "Escrow administration outcome unknown") || !strings.Contains(body, "may have been saved") || !strings.Contains(body, "resubmit it unchanged") {
		t.Fatalf("lost commit acknowledgement status=%d retry=%q pending=%v body=%s", status, retry, pending, body)
	}
}

func TestXMRSettlementRequestReceiptsDoNotRepeatWatcherRequests(t *testing.T) {
	for _, kind := range []string{"rerequest", "retry-build"} {
		t.Run(kind, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			var order string
			var id int64
			if kind == "rerequest" {
				order, id = x.failedRelease()
			} else {
				order, _, _ = x.completed()
				var claim string
				id, claim = x.release(order)
				x.fake.edit(func(f *fakeXMREscrow) { f.crash[claim] = true })
				for range 2 {
					if err := x.poll(); err == nil {
						t.Fatal("build did not hit injected wallet crash")
					}
				}
				if x.waitClass(id) != xmrBuildSetAside {
					t.Fatal("failed builds did not set settlement aside")
				}
			}
			adminID, admin := x.user("xmr_request_receipt_admin", "admin")
			path := "/admin/escrow-xmr-" + kind
			page := x.page("/order?id="+order, admin)
			f := url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page, path)}}
			before := submissionCount(t, x.testEnv)
			x.page("/order?id="+order, admin)
			if submissionCount(t, x.testEnv) != before {
				t.Fatal("XMR settlement GET wrote receipt")
			}
			lost := loseNextActionCommit(t, x.testEnv)
			w := x.do("POST", path, admin, f)
			assertStaffRequestLostAck(t, w.Code, w.Header().Get("Retry-After"), w.Body.String(), lost.Load())
			x.check(x.do("POST", path, admin, f), 303)
			if kind == "rerequest" {
				if len(x.reRequests(id)) != 1 {
					t.Fatal("receipt repeated re-request before watcher")
				}
				x.mustPoll()
				if x.settlement(id).State != "superseded" {
					t.Fatalf("watcher did not supersede settlement: %s", x.settlement(id).State)
				}
			} else {
				fresh := copyFormValues(f)
				fresh.Set(formNonceField, randomToken()[:32])
				x.check(x.do("POST", path, admin, fresh), 409) // cleared wait class retains original guard
				// A later failed build is a distinct staff task; an old receipt must not clear it.
				for range 2 {
					if err := x.poll(); err == nil {
						t.Fatal("retried build did not hit injected wallet crash")
					}
				}
				if x.waitClass(id) != xmrBuildSetAside {
					t.Fatal("later build failure did not set settlement aside")
				}
			}
			savedUpdated := agStr(x.testEnv, "SELECT updated::text FROM xmr_settlements WHERE id=$1", id)
			effects := staffRequestEffects(x.testEnv, order, adminID)
			transferCalls := x.fake.countOf("transfer")
			relayCalls := x.fake.countOf("relay_tx")
			requests := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_rerequests WHERE settlement_id=$1", id)
			settlements := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order)
			x.check(x.do("POST", path, admin, f), 303)
			if staffRequestEffects(x.testEnv, order, adminID) != effects || agStr(x.testEnv, "SELECT updated::text FROM xmr_settlements WHERE id=$1", id) != savedUpdated || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_rerequests WHERE settlement_id=$1", id) != requests || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order) != settlements || x.fake.countOf("transfer") != transferCalls || x.fake.countOf("relay_tx") != relayCalls {
				t.Fatal("XMR saved receipt repeated request, RPC or state effects")
			}
			if kind == "retry-build" && x.waitClass(id) != xmrBuildSetAside {
				t.Fatal("old retry receipt cleared new failure")
			}
			changed := copyFormValues(f)
			changed.Set("order_id", randomToken())
			x.check(x.do("POST", path, admin, changed), 409)
			x.check(x.do("POST", path, agSession(x.testEnv, adminID), f), 409)
			wrong := copyFormValues(f)
			wrong.Set("password", "wrong password")
			x.check(x.do("POST", path, admin, wrong), 401)
			agExec(x.testEnv, "UPDATE orders SET buyer_id=$2 WHERE id=$1", order, adminID)
			x.check(x.do("POST", path, admin, f), 403)
			if staffRequestEffects(x.testEnv, order, adminID) != effects {
				t.Fatal("XMR saved request refusals repeated effects")
			}
		})
	}
}
