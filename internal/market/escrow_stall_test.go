package market

import (
	"html"
	"strings"
	"testing"
)

// A-441 (P4-R7 (a)(b)): broadcast honesty, against the fake escrow Core (simulated RPC, not chain evidence;
// TestEscrowRegtestPrivateBroadcast in scripts/test-escrow-regtest.sh is the real-chain leg).

// stallNotices is how many staff notifications and order notes say a settlement of order is queued or stuck, and how
// many order notes say it was seen since.
func (x *escrowEnv) stallNotices(order string) (staff, notes, seen int) {
	x.t.Helper()
	staff = agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0 AND strpos(n.body,$2)>0",
		"Payment review needed for order "+shortID(order), escrowStallMark)
	notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)=1", order, escrowStallMark)
	seen = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)=1", order, escrowSeenMark)
	return
}

func (x *escrowEnv) setBlocks(n int64) {
	x.core.mu.Lock()
	x.core.blocks = n
	x.core.mu.Unlock()
}

// A settlement broadcast but never seen by the watch-only wallet (a -privatebroadcast queue with a dead proxy, say)
// raises one staff notice after escrowStallBlocks blocks: not before, however many passes run at one height (blocks are
// counted, not passes), and not twice. The admin page under Payment providers and the order page as staff see it carry
// the "queued or stuck" line until the wallet sees the transaction; then one order note clears it and the line goes.
// The settlement stays broadcast throughout, and the stored transaction is sent again on every pass as before.
func TestEscrowBroadcastStallAlert(t *testing.T) {
	x := newEscrowEnv(t)
	_, modSess := x.user("stallmod_"+randomToken()[:6], "moderator")
	order, id := x.built(regtestAddr("stall-refund"), regtestAddr("stall-claim"), 100000)
	x.DB.Exec("UPDATE payments SET flagged=true WHERE order_id=$1", order) // so a staff reviewer may open the order page
	x.core.mu.Lock()
	x.core.private = true
	x.core.mu.Unlock()
	x.setBlocks(200)
	x.signAs(order, roleVendor, id)
	x.poll()
	r := x.settleByID(id)
	if r.State != "broadcast" {
		t.Fatalf("after the broadcast pass: %+v", r)
	}
	// The admin line, and the staff line on the order page (the order history also carries the stall note itself).
	const line, staffLine = "queued or stuck: not seen by the network", "staff: queued or stuck: not seen by the network"
	quiet := func(when string) {
		t.Helper()
		if staff, notes, _ := x.stallNotices(order); staff+notes != 0 {
			t.Fatalf("%s: %d staff notices, %d notes", when, staff, notes)
		}
		if page := strings.ToLower(x.adminPage()); strings.Contains(page, line) {
			t.Fatalf("%s: the admin page says queued or stuck", when)
		}
		if page := strings.ToLower(html.UnescapeString(x.page("/order?id="+order, modSess))); strings.Contains(page, line) {
			t.Fatalf("%s: the staff order page says queued or stuck", when)
		}
	}
	sends := x.core.count("sendrawtransaction")
	for range 2 * escrowStallBlocks {
		x.poll()
	}
	if got := x.core.count("sendrawtransaction"); got != sends+2*escrowStallBlocks {
		t.Fatalf("an unseen broadcast was sent again %d times in %d passes", got-sends, 2*escrowStallBlocks)
	}
	quiet("many passes at the broadcast height")
	x.setBlocks(200 + escrowStallBlocks - 1)
	x.poll()
	quiet("one block short")
	x.setBlocks(200 + escrowStallBlocks)
	x.poll()
	staff, notes, _ := x.stallNotices(order)
	if staff != agInt(x.testEnv, "SELECT count(*) FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL") || staff == 0 || notes != 1 {
		t.Fatalf("after %d blocks unseen: %d staff notices, %d notes", escrowStallBlocks, staff, notes)
	}
	if s := x.settleByID(id); s.State != "broadcast" || s.Error != r.Error {
		t.Fatalf("the stall changed the settlement: %+v", s)
	}
	for range 3 {
		x.poll()
	}
	x.setBlocks(200 + 3*escrowStallBlocks)
	x.poll()
	if s2, n2, _ := x.stallNotices(order); s2 != staff || n2 != 1 {
		t.Fatalf("told again: %d staff notices, %d notes", s2, n2)
	}
	admin := html.UnescapeString(x.adminPage())
	if !strings.Contains(strings.ToLower(admin), line) || !strings.Contains(admin, shortID(order)) || !strings.Contains(admin, r.TxID) {
		t.Fatalf("admin page lacks the queued-or-stuck line:\n%s", admin)
	}
	if page := html.UnescapeString(x.page("/order?id="+order, modSess)); !strings.Contains(strings.ToLower(page), staffLine) {
		t.Fatalf("staff order page lacks the queued-or-stuck line:\n%s", page)
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		page := strings.ToLower(html.UnescapeString(x.page("/order?id="+order, sess)))
		if strings.Contains(page, staffLine) || !strings.Contains(page, strings.ToLower(escrowStallMark)) {
			t.Fatal("a party is shown the staff line, or not the order note")
		}
	}

	// The network sees it: the next resend reaches the mempool, and the pass after that finds it in the wallet.
	x.core.mu.Lock()
	x.core.private = false
	x.core.mu.Unlock()
	x.poll()
	x.poll()
	staff2, notes2, seen := x.stallNotices(order)
	if staff2 != staff || notes2 != 1 || seen != 1 {
		t.Fatalf("on sighting: %d staff notices, %d stall notes, %d seen notes", staff2, notes2, seen)
	}
	if page := strings.ToLower(x.adminPage()); strings.Contains(page, line) {
		t.Fatal("the admin line stayed after the wallet saw the transaction")
	}
	if page := strings.ToLower(html.UnescapeString(x.page("/order?id="+order, modSess))); strings.Contains(page, staffLine) {
		t.Fatal("the staff order page line stayed after the wallet saw the transaction")
	}
	x.setBlocks(200 + 5*escrowStallBlocks)
	x.poll()
	x.poll()
	if s3, n3, seen3 := x.stallNotices(order); s3 != staff || n3 != 1 || seen3 != 1 {
		t.Fatalf("after sighting, more blocks: %d staff notices, %d stall notes, %d seen notes", s3, n3, seen3)
	}
	x.core.setTxConfs(r.TxID, 3, "")
	x.poll()
	if s := x.settleByID(id); s.State != "confirmed" {
		t.Fatalf("at 3 confirmations: %s", s.State)
	}
}

// The count starts again for a settlement first followed at a later height (a restart, say), and a settlement the
// wallet sees from the start is never reported.
func TestEscrowBroadcastStallSeenNeverAlerts(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("stall-seen-refund"), regtestAddr("stall-seen-claim"), 100000)
	x.setBlocks(300)
	x.signAs(order, roleVendor, id)
	x.poll()
	x.setBlocks(300 + 4*escrowStallBlocks)
	x.poll()
	if staff, notes, seen := x.stallNotices(order); staff+notes+seen != 0 || x.settleByID(id).State != "broadcast" {
		t.Fatalf("a broadcast the wallet saw: %d staff notices, %d notes, %d seen notes", staff, notes, seen)
	}
}

// QA4W4A-3 (from QA's scratch probe): the stall count is in memory, so it starts again with the process: a restart
// halfway reports the stall escrowStallBlocks blocks after the restart's first pass, not after the broadcast ("later",
// never early); and an alert raised before a restart is not raised again after it, however many blocks pass ("never
// twice": the open alert is read from the database, not from memory).
func TestEscrowBroadcastStallAcrossRestart(t *testing.T) {
	x := newEscrowEnv(t)
	x.user("stallrestartmod_"+randomToken()[:6], "moderator")
	order, id := x.built(regtestAddr("stall-restart-refund"), regtestAddr("stall-restart-claim"), 100000)
	x.core.mu.Lock()
	x.core.private = true
	x.core.mu.Unlock()
	const base, half = 200, escrowStallBlocks / 2
	x.setBlocks(base)
	x.signAs(order, roleVendor, id)
	x.poll()
	if s := x.settleByID(id); s.State != "broadcast" {
		t.Fatalf("not broadcast: %+v", s)
	}
	x.setBlocks(base + half)
	x.poll()
	x.restart()
	x.poll() // the restart's first pass, half the count after the broadcast
	x.setBlocks(base + escrowStallBlocks)
	x.poll()
	if _, notes, _ := x.stallNotices(order); notes != 0 {
		t.Fatalf("alert %d blocks after the broadcast despite the restart in between: %d notes", escrowStallBlocks, notes)
	}
	x.setBlocks(base + half + escrowStallBlocks)
	x.poll()
	staff, notes, _ := x.stallNotices(order)
	if notes != 1 || staff == 0 {
		t.Fatalf("no alert %d blocks after the restart's first pass: %d notes, %d staff notices", escrowStallBlocks, notes, staff)
	}
	x.restart()
	x.poll()
	x.setBlocks(base + half + 4*escrowStallBlocks)
	x.poll()
	x.poll()
	if s2, n2, _ := x.stallNotices(order); n2 != 1 || s2 != staff {
		t.Fatalf("told again after a restart: %d notes, %d staff notices (before %d)", n2, s2, staff)
	}
}

// torNode sets what the fake node's getnetworkinfo reports: each network's proxy, whether onion is reachable and the
// version.
func torNode(c *fakeEscrowCore, ipv4, ipv6, onion string, onionReachable bool, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.netProxy = map[string]string{"ipv4": ipv4, "ipv6": ipv6, "onion": onion}
	c.onionReachable, c.netVersion = onionReachable, version
}

// In APP_MODE=tor a settlement is broadcast only while the escrow node's getnetworkinfo, read once per pass, shows a
// proxy for both IPv4 and IPv6 and Bitcoin Core 31.1 or later. Otherwise no sendrawtransaction is issued and the
// settlement stays signed (co-signing, which stores the final transaction and sends nothing, still happens), one log
// line names the change and /admin says what to configure; once the node is set up the next pass broadcasts. A failed
// getnetworkinfo waits too. In clearnet mode none of this applies.
func TestEscrowTorBroadcastGate(t *testing.T) {
	const socks = "127.0.0.1:9050"
	for _, tc := range []struct {
		name string
		mode string
		set  func(c *fakeEscrowCore)
		why  string // the reason the gate holds; "" = it sends
	}{
		{"tor: proxy for ipv4 and ipv6 on 31.1", "tor", func(c *fakeEscrowCore) { torNode(c, socks, socks, socks, true, 310100) }, ""},
		{"tor: proxy for ipv4 and ipv6 on a later Core", "tor", func(c *fakeEscrowCore) { torNode(c, socks, socks, socks, true, 320000) }, ""},
		{"tor: no proxy", "tor", func(c *fakeEscrowCore) { torNode(c, "", "", "", false, 310100) }, "no proxy for IPv4 or IPv6"},
		{"tor: ipv4 only", "tor", func(c *fakeEscrowCore) { torNode(c, socks, "", socks, true, 310100) }, "no proxy for IPv6"},
		{"tor: ipv6 only", "tor", func(c *fakeEscrowCore) { torNode(c, "", socks, socks, true, 310100) }, "no proxy for IPv4"},
		{"tor: -onion only, onion reachable", "tor", func(c *fakeEscrowCore) { torNode(c, "", "", socks, true, 310100) }, "no proxy for IPv4 or IPv6"},
		{"tor: Core 31.0", "tor", func(c *fakeEscrowCore) { torNode(c, socks, socks, socks, true, 310000) }, "Bitcoin Core older than 31.1"},
		{"tor: getnetworkinfo fails", "tor", func(c *fakeEscrowCore) { torNode(c, socks, socks, socks, true, 310100); c.netInfoErr = -32601 }, "getnetworkinfo failed: getnetworkinfo rpc:-32601"},
		{"tor: getnetworkinfo forbidden", "tor", func(c *fakeEscrowCore) { torNode(c, socks, socks, socks, true, 310100); c.netInfoForbidden = true }, "getnetworkinfo failed: getnetworkinfo forbidden"},
		{"clearnet: no proxy", "clearnet", func(c *fakeEscrowCore) { torNode(c, "", "", "", false, 310100) }, ""},
		{"clearnet: getnetworkinfo fails", "clearnet", func(c *fakeEscrowCore) { c.netInfoErr = -32601 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newEscrowEnv(t)
			x.A.mode = tc.mode
			logs := captureLog(t)
			tc.set(x.core)
			order, id := x.built(regtestAddr("tor-refund"), regtestAddr("tor-claim"), 100000)
			x.signAs(order, roleVendor, id)
			infos, sends := x.core.count("getnetworkinfo"), x.core.count("sendrawtransaction")
			x.poll()
			first := x.settleByID(id)
			x.poll()
			hold := "escrow: APP_MODE=tor: escrow broadcasts wait (" + tc.why + ")"
			if tc.why == "" {
				if s := x.settleByID(id); s.State != "broadcast" || x.core.count("sendrawtransaction") == sends {
					t.Fatalf("not broadcast: %+v", s)
				}
				if tc.mode == "clearnet" {
					if n := x.core.count("getnetworkinfo"); n != infos {
						t.Fatalf("clearnet on regtest called getnetworkinfo %d times", n-infos)
					}
				} else if n := x.core.count("getnetworkinfo"); n != infos+2 {
					t.Fatalf("Tor mode called getnetworkinfo %d times in 2 passes, want once per pass", n-infos)
				}
				if got := logs.String(); strings.Contains(got, "escrow broadcasts wait") {
					t.Fatalf("log:\n%s", got)
				}
				if page := x.adminPage(); strings.Contains(page, "Bitcoin escrow broadcasts wait") {
					t.Fatal("admin page says broadcasts wait")
				}
				return
			}
			if n := x.core.count("sendrawtransaction"); n != sends {
				t.Fatalf("%d sendrawtransaction calls while the gate holds", n-sends)
			}
			if s := x.settleByID(id); s != first || s.State != "signed" || s.Raw == "" || s.Error != "" {
				t.Fatalf("the settlement while the gate holds: %+v, then %+v", first, s)
			}
			if tc.mode == "clearnet" {
				if n := x.core.count("getnetworkinfo"); n != infos {
					t.Fatalf("clearnet on regtest called getnetworkinfo %d times", n-infos)
				}
			} else if n := x.core.count("getnetworkinfo"); n != infos+2 && tc.why != torGateFailedAs+"getnetworkinfo forbidden" {
				t.Fatalf("Tor mode called getnetworkinfo %d times in 2 passes, want once per pass (a call refused with HTTP 403 never reaches the fake)", n-infos)
			}
			if got := logs.String(); strings.Count(got, hold) != 1 || strings.Contains(got, "escrow watcher:") {
				t.Fatalf("log while the gate holds, want one %q:\n%s", hold, got)
			}
			page := x.adminPage()
			for _, want := range []string{"Bitcoin escrow broadcasts wait (APP_MODE=tor): " + tc.why + ".", "-proxy=", "never -connect or -onlynet", "Bitcoin Core 31.1 or later"} {
				if !strings.Contains(page, want) {
					t.Fatalf("admin page lacks %q:\n%s", want, page)
				}
			}
			// Configured: the next pass broadcasts, with one log line; the admin line goes.
			x.core.mu.Lock()
			x.core.netInfoErr, x.core.netInfoForbidden = 0, false
			x.core.mu.Unlock()
			torNode(x.core, socks, socks, "", true, 310100)
			x.poll()
			if s := x.settleByID(id); s.State != "broadcast" || x.core.count("sendrawtransaction") != sends+1 {
				t.Fatalf("not broadcast once the node sends through the proxy: %+v", s)
			}
			resumed := "escrow: APP_MODE=tor: the escrow node sends through a proxy for IPv4 and IPv6 on Bitcoin Core 31.1 or later; escrow broadcasts resumed"
			if got := logs.String(); strings.Count(got, resumed) != 1 {
				t.Fatalf("log once configured:\n%s", got)
			}
			if page := x.adminPage(); strings.Contains(page, "Bitcoin escrow broadcasts wait") {
				t.Fatal("admin line stayed once configured")
			}
			// A broadcast settlement is not sent again while the gate holds (trackBroadcast's resend).
			tc.set(x.core)
			sends = x.core.count("sendrawtransaction")
			x.poll()
			x.poll()
			if n := x.core.count("sendrawtransaction"); n != sends {
				t.Fatalf("%d resends while the gate holds", n-sends)
			}
			if s := x.settleByID(id); s.State != "broadcast" {
				t.Fatalf("the broadcast settlement moved while the gate holds: %+v", s)
			}
			if got := logs.String(); strings.Count(got, hold) != 2 {
				t.Fatalf("want a second hold line after the change back:\n%s", got)
			}
		})
	}
}
