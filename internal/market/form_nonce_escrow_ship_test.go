package market

import (
	"net/url"
)

func btcShipNonceForm(order, address, nonce string) url.Values {
	f := escrowReceiptForm(order, address, nonce)
	f.Set("note", "Sent by tracked post.")
	return f
}
