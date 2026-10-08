-- A-86: preserve the send-time count so later recipient-key changes do not rewrite message history.
-- NULL means the recipient IDs were hidden or the recipient had no usable key to compare.
ALTER TABLE messages ADD COLUMN recipient_extra_keys integer;
ALTER TABLE messages ADD CONSTRAINT messages_recipient_extra_keys_check
 CHECK (recipient_extra_keys IS NULL OR recipient_extra_keys >= 0);
