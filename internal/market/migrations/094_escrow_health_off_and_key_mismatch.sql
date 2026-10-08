-- A-451/A-452 follow-up: distinguish an intentional configuration-off state and an arbiter account-key substitution.
ALTER TABLE bitcoin_escrow_health DROP CONSTRAINT bitcoin_escrow_health_disabled_class_check;
ALTER TABLE bitcoin_escrow_health ADD CONSTRAINT bitcoin_escrow_health_disabled_class_check
  CHECK (disabled_class IN ('not_checked','off','node_unreachable','unsupported_chain','chain_pin_mismatch',
    'pooled_key_access','arbiter_wallet_unreadable','arbiter_fingerprint_mismatch','arbiter_key_mismatch',
    'arbiter_wallet_not_dedicated','arbiter_wallet_history_unreadable','watch_wallet_unreadable',
    'fee_address_invalid','check_failed'));
