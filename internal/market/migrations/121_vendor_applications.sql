-- A-544: vendor onboarding. An account becomes a vendor through an application decided under the administrator's
-- vendor policy (settings vendor_bond_xmr, in atomic piconero, and vendor_approval). The bond is a non-refundable
-- fee. Each application captures the bond and approval mode in force when it was created, so a policy change
-- applies to new applications only. An administrator records the bond as received (how 'manual') or waives it
-- ('waived'); 'xmr' is reserved for automatic Monero deposit detection, which this release does not ship.
CREATE TABLE vendor_applications (
  id text PRIMARY KEY,
  user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  state text NOT NULL CHECK (state IN ('awaiting_bond','awaiting_approval','approved','rejected','withdrawn')),
  approval text NOT NULL CHECK (approval IN ('manual','automatic')),
  bond_piconero bigint NOT NULL CHECK (bond_piconero >= 0),
  bond_received_at timestamptz,
  bond_received_by text REFERENCES users(id) ON DELETE SET NULL,
  bond_received_how text CHECK (bond_received_how IN ('manual','xmr','waived')),
  decided_by text REFERENCES users(id) ON DELETE SET NULL,
  decided_at timestamptz,
  note text NOT NULL DEFAULT '' CHECK (char_length(note) <= 500),
  created timestamptz NOT NULL DEFAULT now(),
  CHECK ((bond_received_at IS NULL) = (bond_received_how IS NULL)),
  -- Waiting for a bond means none was recorded; a bond application leaves that state only once it is.
  CHECK (state <> 'awaiting_bond' OR (bond_piconero > 0 AND bond_received_at IS NULL)),
  CHECK (state NOT IN ('awaiting_approval','approved') OR bond_piconero = 0 OR bond_received_at IS NOT NULL),
  CHECK ((state IN ('approved','rejected','withdrawn')) = (decided_at IS NOT NULL))
);
-- At most one open application per account.
CREATE UNIQUE INDEX vendor_applications_one_open ON vendor_applications(user_id) WHERE state IN ('awaiting_bond','awaiting_approval');
CREATE INDEX vendor_applications_user ON vendor_applications(user_id, created DESC);
CREATE INDEX vendor_applications_queue ON vendor_applications(created, id) WHERE state IN ('awaiting_bond','awaiting_approval');

-- An administrator's "verified vendor" mark: the staff's judgment, not an identity check. set_by/set_at record the
-- latest change either way; the audit trail keeps the history.
ALTER TABLE users ADD COLUMN vendor_verified boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN vendor_verified_set_by text REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN vendor_verified_set_at timestamptz;

INSERT INTO settings(key,value) VALUES ('vendor_bond_xmr','0'),('vendor_approval','manual'),('vendor_bond_instructions','')
  ON CONFLICT (key) DO NOTHING;

-- Existing vendors are grandfathered with an approved application (no bond, decided by no one) and are not verified.
INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero,decided_at,note,created)
  SELECT md5(u.id || ':vendor-application-121'), u.id, 'approved', 'manual', 0, now(),
    'Vendor before vendor applications (migration 121)', u.created
  FROM users u WHERE u.role = 'vendor';
