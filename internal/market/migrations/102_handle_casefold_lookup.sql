-- Deliberately non-unique: legacy installations may have handles differing only by case.
-- New account creation rejects these collisions under advisory lock 782493.
CREATE INDEX users_handle_casefold ON users (lower(handle));
