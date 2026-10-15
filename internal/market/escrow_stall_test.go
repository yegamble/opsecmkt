package market

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

// torNode sets what the fake node's getnetworkinfo reports: each network's proxy, whether onion is reachable and the
// version.
func torNode(c *fakeEscrowCore, ipv4, ipv6, onion string, onionReachable bool, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.netProxy = map[string]string{"ipv4": ipv4, "ipv6": ipv6, "onion": onion}
	c.onionReachable, c.netVersion = onionReachable, version
}
