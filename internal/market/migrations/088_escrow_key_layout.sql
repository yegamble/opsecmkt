-- A-455 (shape (b) slice 3; rulings P4-R2 and P4-R18). The layout of an account's Bitcoin escrow key: 'core' (a
-- Bitcoin Core wallet; each escrow uses the account key's child /0/<i>, shape (a)) or 'per_order' (a BIP39-seed
-- Electrum or Sparrow wallet made for each order from the account key's child <i>, shape (b)). Every existing key
-- is a Core key. A user keeps one key (user_escrow_keys is keyed by user); changing the layout means registering
-- again, which resets active_at, so the practice signature is passed again in the new layout (ES-9).
ALTER TABLE user_escrow_keys ADD COLUMN layout text NOT NULL DEFAULT 'core' CHECK (layout IN ('core','per_order'));

-- An escrow key's derivation_suffix names its layout by its length: shape (a) writes exactly two steps, /0/<i>
-- (checkEscrowOrigin for the parties, /0/<market_index> for the arbiter), and shape (b) three, /<i>/0/0 (the first
-- receiving key of the per-order wallet built on the account key's child <i>). A per-order index of 0 or 1 would
-- serve an ancestor of other keys as a per-order wallet's xpub: account/0 is the parent of every shape-(a) child and
-- account/1 the change branch (SEC-2, P4-R2), so a suffix of three or more steps may not start with 0 or 1, for any
-- party. The reserved per-order practice index 2^31-1 (ES-9) is refused for a buyer's or a vendor's key, as 074
-- refuses /0/0. Both are added NOT VALID and then validated, so a row that breaks them stops the migration.
ALTER TABLE escrow_pubkeys ADD CONSTRAINT escrow_pubkeys_per_order_index
  CHECK (derivation_suffix !~ '^/[01]h?(/[^/]+){2,}$') NOT VALID;
ALTER TABLE escrow_pubkeys VALIDATE CONSTRAINT escrow_pubkeys_per_order_index;
ALTER TABLE escrow_pubkeys ADD CONSTRAINT escrow_pubkeys_per_order_practice
  CHECK (party = 'market' OR derivation_suffix !~ '^/2147483647h?(/[^/]+){2,}$') NOT VALID;
ALTER TABLE escrow_pubkeys VALIDATE CONSTRAINT escrow_pubkeys_per_order_practice;
