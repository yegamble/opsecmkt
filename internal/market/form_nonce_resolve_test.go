package market

import (
	"net/url"

	"testing"
)

func TestReceiptAuthorizeRequiresConfirmedFormNonce(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unguarded receipt authorization was registered")
		}
		if _, exists := actions["/invalid-receipt-authorize"]; exists {
			t.Fatal("invalid action was registered")
		}
	}()
	registerAction("/invalid-receipt-authorize", actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ReceiptAuthorize: func(*actionCtx) error { return nil }, Run: func(*actionCtx) (actionResult, error) { return actionResult{}, nil }})
}

func resolveNonceForm(dispute, outcome, nonce string) url.Values {
	return resolveConfirmed(url.Values{
		"id": {dispute}, "outcome": {outcome},
		"resolution": {"The evidence supports this decision and the payment disposition."},
		"reply_seen": {"0"}, formNonceField: {nonce},
	}, 100000)
}
