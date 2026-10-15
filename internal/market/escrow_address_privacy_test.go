package market

import (
	"testing"
)

// Older migration tests explicitly reconstruct the pre-upgrade schema.
func restoreSchema110(t *testing.T, e *testEnv) {
	t.Helper()
	if _, err := e.DB.Exec(`DELETE FROM schema_migrations WHERE version=111;
		DROP TRIGGER escrow_refund_address_reserve ON escrows;
		DROP TRIGGER escrow_claim_address_reserve ON settlements;
		DROP TRIGGER xmr_escrow_claim_address_reserve ON xmr_settlements;
		DROP FUNCTION escrow_refund_address_reserve(); DROP FUNCTION escrow_claim_address_reserve();
		DROP TABLE escrow_external_addresses;
		ALTER TABLE escrows ADD CONSTRAINT escrows_refund_address_key UNIQUE (refund_address);
		CREATE UNIQUE INDEX settlements_release_destination ON settlements(destination) WHERE kind='release';
		CREATE UNIQUE INDEX xmr_settlements_release_destination ON xmr_settlements(destination) WHERE kind='release' AND supersedes IS NULL`); err != nil {
		t.Fatal(err)
	}
}
