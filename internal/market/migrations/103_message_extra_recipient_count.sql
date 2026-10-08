-- A-86: preserve the send-time count so later recipient-key changes do not rewrite message history.
-- Count distinct visible additional recipient IDs only; NULL means the recipient key was unknown or the row predates this migration.
ALTER TABLE messages ADD COLUMN recipient_extra_keys integer;
ALTER TABLE messages ADD CONSTRAINT messages_recipient_extra_keys_check
 CHECK (recipient_extra_keys IS NULL OR recipient_extra_keys >= 0);
