-- Vendor approval is independent of a moderator's staff authority. Administrator
-- selling/verification is derived from the current role, never persisted here.
ALTER TABLE users ADD COLUMN vendor_approved boolean NOT NULL DEFAULT false;
UPDATE users SET vendor_approved=true WHERE role='vendor';
