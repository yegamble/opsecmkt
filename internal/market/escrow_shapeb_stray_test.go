package market

import (
	"slices"
)

// siblingKeysGo is Go's derivation of the three keys of order's sibling /branch/k: each party's account key's child
// <i>/branch/k and the signer's <m>/branch/k, by party.
func (x *escrowEnv) siblingKeysGo(order string, signer bip32Key, branch, k uint32) map[string][]byte {
	x.t.Helper()
	rows := x.escrowRows(order)
	out := map[string][]byte{}
	for party, expr := range map[string]string{"buyer": x.buyerKey, "vendor": x.vendorKey} {
		steps, _ := parseOriginPath(rows[party].suffix)
		child, _ := perOrderChild(x.t, accountKey(x.t, expr), steps[0])
		leaf := siblingKey(x.t, child, branch, k)
		out[party] = slices.Clone(leaf.PubKey[:])
	}
	child, _ := perOrderChild(x.t, signer, uint32(x.marketIndex(order)))
	leaf := siblingKey(x.t, child, branch, k)
	out["market"] = slices.Clone(leaf.PubKey[:])
	return out
}

// strayRefunds is order's stray refunds: id and state, oldest first.
func (x *escrowEnv) strayRefunds(order string) (ids []int64, states []string) {
	x.t.Helper()
	rows, err := x.DB.Query("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='stray_refund' ORDER BY id", order)
	if err != nil {
		x.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s string
		rows.Scan(&id, &s)
		ids, states = append(ids, id), append(states, s)
	}
	return ids, states
}
