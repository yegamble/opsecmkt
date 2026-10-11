package market

import (
	"html"
	"strings"
	"testing"
)

// Historical payout rows remain visible to their recipient after the send
// routes have been retired. No test wallet or runtime payout writer is needed.
func insertHistoricalPayout(t *testing.T, p *payEnv, orderState, kind, userID, state, address, txid, errText string) string {
	t.Helper()
	order, _ := p.newOrder(orderState)
	if _, err := p.DB.Exec(`INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'btc-testnet3')`, order, "legacy-deposit-"+order[:12]); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid,error)
        VALUES($1,$2,$3,'BTC',100000,$4,$5,$6,$7)`, order, kind, userID, address, state, txid, errText); err != nil {
		t.Fatal(err)
	}
	return order
}

func TestHistoricalPayoutRecipientAndPrivacy(t *testing.T) {
	p := newPayEnv(t)
	secret := "RAW-WALLET-SECRET"
	pending := insertHistoricalPayout(t, p, stateCompleted, "release", p.vendor.ID, "failed", "legacy-vendor-destination", "", secret)
	sentTx := strings.Repeat("ab", 32)
	sent := insertHistoricalPayout(t, p, stateCompleted, "release", p.vendor.ID, "sent", "legacy-vendor-sent", sentTx, "")
	refund := insertHistoricalPayout(t, p, stateCancelled, "refund", p.buyer.ID, "pending", "legacy-buyer-destination", "", "")

	vendorAccount := html.UnescapeString(p.page("/account", p.vendorSess))
	mustContain(t, vendorAccount, "Historical payout records", pending[:8], "Failed — not retried")
	mustNotContain(t, vendorAccount, refund[:8], "legacy-buyer-destination", secret)
	buyerAccount := html.UnescapeString(p.page("/account", p.buyerSess))
	mustContain(t, buyerAccount, refund[:8], "Queued")
	mustNotContain(t, buyerAccount, pending[:8], "legacy-vendor-destination", secret)

	vendorOrder := html.UnescapeString(p.page("/order?id="+pending, p.vendorSess))
	mustContain(t, vendorOrder, "legacy-vendor-destination", "historical record")
	mustNotContain(t, vendorOrder, secret)
	buyerOrder := html.UnescapeString(p.page("/order?id="+pending, p.buyerSess))
	mustNotContain(t, buyerOrder, "legacy-vendor-destination", secret)
	mustContain(t, buyerOrder, "Failed — not retried")

	recipientSent := html.UnescapeString(p.page("/order?id="+sent, p.vendorSess))
	mustContain(t, recipientSent, sentTx, "legacy-vendor-sent")
	otherSent := html.UnescapeString(p.page("/order?id="+sent, p.buyerSess))
	mustNotContain(t, otherSent, sentTx, "legacy-vendor-sent")
}

func TestHistoricalPayoutTxidInStoredNotesIsRedactedFromOtherParty(t *testing.T) {
	p := newPayEnv(t)
	txid := strings.Repeat("cd", 32)
	order := insertHistoricalPayout(t, p, stateResolved, "refund", p.buyer.ID, "sent", "legacy-buyer-destination", txid, "")
	agExec(p.testEnv, "INSERT INTO order_events(order_id,from_state,to_state,note) VALUES($1,'resolved','resolved',$2)", order, "Legacy payout transaction "+txid+" and "+truncate(txid, 20))
	agExec(p.testEnv, "INSERT INTO disputes(id,order_id,reason,status,outcome,resolution) VALUES($1,$2,'Nothing arrived at all','Resolved — refund to buyer','refund',$3)", randomToken(), order, "Recorded refund "+txid)
	buyer := html.UnescapeString(p.page("/order?id="+order, p.buyerSess))
	vendor := html.UnescapeString(p.page("/order?id="+order, p.vendorSess))
	mustContain(t, buyer, txid)
	mustNotContain(t, vendor, txid, truncate(txid, 20), "legacy-buyer-destination")
	mustContain(t, vendor, payoutTxidHidden)
}
