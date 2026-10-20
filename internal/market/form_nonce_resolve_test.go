package market

import (
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
