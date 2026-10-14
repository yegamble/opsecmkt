package market

import "context"

// bitcoinAddressUsed keeps infrastructure addresses global, and external
// destination history private to its recipient account. A vendor must still not
// claim to this order's buyer refund, even though that address belongs to another
// account. Migration 111 atomically reserves each accepted external destination.
func bitcoinAddressUsed(ctx context.Context, q rowQuerier, addr, feeAddr, userID, orderID string) (bool, error) {
	var used bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM escrows WHERE address=$1 OR fee_destination=$1) OR $1=$2
		OR EXISTS(SELECT 1 FROM escrow_siblings WHERE address=$1)
		OR EXISTS(SELECT 1 FROM retention_escrow_retired_addresses WHERE coin='BTC' AND address_hash=encode(sha256(convert_to($1,'UTF8')),'hex'))
		OR EXISTS(SELECT 1 FROM escrows WHERE order_id=$4 AND refund_address=$1)
		OR EXISTS(SELECT 1 FROM escrow_external_addresses WHERE user_id=$3 AND coin='BTC' AND address=$1)`, addr, feeAddr, userID, orderID).Scan(&used)
	return used, err
}
