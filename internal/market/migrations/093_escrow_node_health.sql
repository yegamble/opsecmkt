-- A-451: operator-visible Bitcoin escrow node health. Store fixed reason classes and numeric/allowlisted node state
-- only; credentials, RPC bodies, wallet names and node-provided error text never enter this row.
CREATE TABLE bitcoin_escrow_health (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  enabled boolean NOT NULL DEFAULT false,
  disabled_class text NOT NULL DEFAULT 'not_checked'
    CHECK (disabled_class IN ('not_checked','node_unreachable','unsupported_chain','chain_pin_mismatch',
      'pooled_key_access','arbiter_wallet_unreadable','arbiter_fingerprint_mismatch','arbiter_wallet_not_dedicated',
      'arbiter_wallet_history_unreadable','watch_wallet_unreadable','fee_address_invalid','check_failed')),
  last_pass timestamptz,
  chain text NOT NULL DEFAULT 'unknown'
    CHECK (chain IN ('unknown','regtest','signet','testnet4','main','testnet3')),
  height bigint CHECK (height IS NULL OR height >= 0),
  pin_match boolean,
  keypool_high_water bigint CHECK (keypool_high_water IS NULL OR keypool_high_water >= 0),
  highest_market_index bigint NOT NULL DEFAULT 0 CHECK (highest_market_index >= 0),
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO bitcoin_escrow_health(singleton) VALUES (true) ON CONFLICT (singleton) DO NOTHING;
