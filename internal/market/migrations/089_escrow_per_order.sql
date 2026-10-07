-- A-456a (shape (b) slice 4, first half; rulings P4-R2, P4-R17, P4-R18; ledger ADDENDUM QA4W5K-6). Per-order (shape (b))
-- Bitcoin escrows: each party's key is the first receiving key of an order wallet built on the party's account key's
-- child <i> (derivation suffix /<i>/0/0), and the market's is the escrow signer's account key's child <m>/0/0. Nothing
-- writes a shape-(b) row while the application's per-order switch is off, which it is until A-456b; shape (a) rows
-- (/0/<i>) are not affected. See UPGRADING.md section 44.

-- 1. Every step of every escrow key's derivation suffix is unhardened and below 2^31, for every party, and a per-order
--    derivation suffix (three or more steps) is exactly /<i>/0/0 with 2 <= i < 2^31-1. Migration 074 allows steps of up
--    to ten digits, so /2147483648/0/0 and /2147483649/0/0 (which name the hardened children /0h/0/0 and /1h/0/0) passed
--    088's checks, as did a shape-(a) /0/2147483648 or /0/9999999999 (QA5W3A-11); i >= 2 because account/0 and
--    account/1, a wallet's receiving and change branches, served as an order wallet's key would be ancestors of other
--    keys (P4-R2); and the practice index 2^31-1, refused for a buyer's or a vendor's key since 088, is refused for the
--    market's per-order key too. The application writes only /0/<i> (shape (a)) and /<i>/0/0 (shape (b)). A row that
--    breaks the rule stops this migration with a message naming it, before anything changes; no release writes one.
--    One expression serves the refusal and the constraint (each CASE arm casts only what its pattern admits).
DO $$
DECLARE
  n bigint;
  example text;
BEGIN
  SELECT count(*), min(derivation_suffix) INTO n, example FROM escrow_pubkeys
  WHERE NOT CASE
    WHEN derivation_suffix ~ '^/[1-9][0-9]{0,9}/0/0$' THEN split_part(derivation_suffix, '/', 2)::bigint BETWEEN 2 AND 2147483646
    WHEN derivation_suffix ~ '^(/[^/]+){3,}$' THEN false
    WHEN derivation_suffix ~ '^/[0-9]{1,10}$' THEN split_part(derivation_suffix, '/', 2)::bigint <= 2147483647
    WHEN derivation_suffix ~ '^/[0-9]{1,10}/[0-9]{1,10}$' THEN split_part(derivation_suffix, '/', 2)::bigint <= 2147483647
      AND split_part(derivation_suffix, '/', 3)::bigint <= 2147483647
    ELSE false END;
  IF n > 0 THEN
    RAISE EXCEPTION 'migration 089: % escrow key row(s) in escrow_pubkeys have a derivation suffix (such as %) with a hardened step or a step of 2^31 or more (which names a hardened key), or a per-order suffix that is not /<i>/0/0 with 2 <= i < 2^31-1. No release writes such a row: restore the pre-upgrade backup or ask the project. See UPGRADING.md section 44. Nothing was changed.', n, example
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE escrow_pubkeys ADD CONSTRAINT escrow_pubkeys_suffix_steps CHECK (CASE
    WHEN derivation_suffix ~ '^/[1-9][0-9]{0,9}/0/0$' THEN split_part(derivation_suffix, '/', 2)::bigint BETWEEN 2 AND 2147483646
    WHEN derivation_suffix ~ '^(/[^/]+){3,}$' THEN false
    WHEN derivation_suffix ~ '^/[0-9]{1,10}$' THEN split_part(derivation_suffix, '/', 2)::bigint <= 2147483647
    WHEN derivation_suffix ~ '^/[0-9]{1,10}/[0-9]{1,10}$' THEN split_part(derivation_suffix, '/', 2)::bigint <= 2147483647
      AND split_part(derivation_suffix, '/', 3)::bigint <= 2147483647
    ELSE false END) NOT VALID;
ALTER TABLE escrow_pubkeys VALIDATE CONSTRAINT escrow_pubkeys_suffix_steps;

-- 2. The market's key in shape (b) is the escrow signer's rooted account key (depth 0, its own fingerprint), so its
--    origin has a fingerprint and no path: origin_path '' is allowed for the market's per-order key only. Every other
--    row keeps 074's rule (one to ten canonical steps).
ALTER TABLE escrow_pubkeys DROP CONSTRAINT escrow_pubkeys_origin_path_check;
ALTER TABLE escrow_pubkeys ADD CONSTRAINT escrow_pubkeys_origin_path CHECK (origin_path ~ '^(/(0|[1-9][0-9]{0,9})h?){1,10}$'
  OR party = 'market' AND origin_path = '' AND derivation_suffix ~ '^(/[^/]+){3}$') NOT VALID;
ALTER TABLE escrow_pubkeys VALIDATE CONSTRAINT escrow_pubkeys_origin_path;

-- 3. The order wallet's sibling addresses of each shape-(b) escrow: receiving /0/1../0/19 and change /1/0../1/9 (/0/0 is
--    the escrow's own address). The escrow watcher reads them with the escrow's, so a deposit to one is recorded (left
--    out, never counted) and returned by the stray refund instead of being stranded (P4-R2). Checkout writes them with
--    the escrow, after Bitcoin Core derived the same addresses as the application.
CREATE TABLE escrow_siblings (
 escrow_id bigint NOT NULL REFERENCES escrows(id),
 branch smallint NOT NULL CHECK (branch IN (0, 1)),
 idx smallint NOT NULL CHECK (branch = 0 AND idx BETWEEN 1 AND 19 OR branch = 1 AND idx BETWEEN 0 AND 9),
 address text NOT NULL UNIQUE CHECK (address ~ '^[a-z0-9]{14,90}$'),
 PRIMARY KEY (escrow_id, branch, idx)
);

-- 4. per_order_xpub (074's optional per-order extended key, NULL until now) holds, for the buyer's and the vendor's key
--    of a shape-(b) escrow only, the party's order wallet key: the account key's child <i>, serialized as a tpub
--    (public data the market already holds, derived at checkout and cross-checked with Core with the escrow's ranged
--    descriptors), so the watch-only wallet's repair and the signing rebuild the order wallet whatever key the party has
--    registered since (QA5W4A-5). The market's per-order key comes from the escrow signer and every other row stays NULL.
ALTER TABLE escrow_pubkeys ADD CONSTRAINT escrow_pubkeys_per_order_xpub CHECK (per_order_xpub IS NULL
  OR party <> 'market' AND derivation_suffix ~ '^/[1-9][0-9]{0,9}/0/0$' AND per_order_xpub ~ '^tpub[1-9A-HJ-NP-Za-km-z]{107}$') NOT VALID;
ALTER TABLE escrow_pubkeys VALIDATE CONSTRAINT escrow_pubkeys_per_order_xpub;
