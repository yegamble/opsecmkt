package market

// confirmedSettlement is a vendor's claim co-signed, broadcast and confirmed at 3 confirmations: the order and the
// settlement.
func (x *escrowEnv) confirmedSettlement(label string) (string, int64) {
	x.t.Helper()
	order, id := x.built(x.netAddr("refund-"+label), x.netAddr("claim-"+label), 100000)
	x.signAs(order, roleVendor, id)
	x.poll()
	x.core.setTxConfs(x.settleByID(id).TxID, 3, "")
	x.poll()
	if r := x.settleByID(id); r.State != "confirmed" {
		x.t.Fatalf("%s: %+v", label, r)
	}
	return order, id
}
