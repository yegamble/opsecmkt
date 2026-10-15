package market

import (
	"context"

	"testing"
)

// escrowKeyOf is the stored public key of party in order's escrow.
func (e *testEnv) escrowKeyOf(t *testing.T, order, party string) []byte {
	t.Helper()
	keys, err := escrowKeys(context.Background(), e.DB, order)
	if err != nil {
		t.Fatal(err)
	}
	return keys[party].PubKey
}
