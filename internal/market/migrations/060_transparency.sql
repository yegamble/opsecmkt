-- P6 Transparency: the single operator-pasted, PGP-clearsigned warrant canary. The operator's public key lives in
-- settings (operator_pgp_key, operator_pgp_fingerprint); the audit-export signing key is env-only, never stored here.
CREATE TABLE IF NOT EXISTS canary (
 id integer PRIMARY KEY CHECK (id = 1),
 clearsigned text NOT NULL CHECK (length(clearsigned) BETWEEN 1 AND 16384),
 posted timestamptz NOT NULL DEFAULT now()
);
