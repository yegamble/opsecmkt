-- A-520 (PRIV60-4). The saved pooled-payout addresses (users.payout_btc/payout_xmr, migration 050) and the times they
-- were last saved or removed (payout_btc_changed/payout_xmr_changed, migration 057) are read by nothing: escrow
-- settlements pay the claim and refund addresses given per order, no page shows or edits the saved ones (A-508), and
-- pooledRemovedPatterns refuses any reader in the application. Keeping them only kept identifying wallet history, so
-- they are cleared for every account. The columns stay, since migrations are immutable and retention's
-- retention_erase_legacy_profile (112) still names them; nothing writes them again. Backups made before this migration
-- keep the old values until they are restored and upgraded or removed.
UPDATE users SET payout_btc='',payout_xmr='',payout_btc_changed=NULL,payout_xmr_changed=NULL
 WHERE payout_btc<>'' OR payout_xmr<>'' OR payout_btc_changed IS NOT NULL OR payout_xmr_changed IS NOT NULL;
