package market

const escrowRestoringText = "Bitcoin escrow is paused while this market restores its records from a backup. Nothing was charged and your order is still a draft; try again later."

// watchInfoCalls is the params of every getaddressinfo call on the watch-only wallet, in order.
func (x *escrowEnv) watchInfoCalls() []string {
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	var out []string
	for _, c := range x.core.calls {
		if c.Method == "getaddressinfo" && c.Path == fakeWatchWallet {
			out = append(out, string(c.Params))
		}
	}
	return out
}

// escrowKeys is the market index and each party's derivation suffix of order's escrow.
func (x *escrowEnv) escrowKeys(order string) (int64, []string) {
	x.t.Helper()
	var idx int64
	if err := x.DB.QueryRow("SELECT market_index FROM escrows WHERE order_id=$1", order).Scan(&idx); err != nil {
		x.t.Fatal(err)
	}
	return idx, agStrings(x.t, x.testEnv, "SELECT party||' '||derivation_suffix FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 ORDER BY party", order)
}
