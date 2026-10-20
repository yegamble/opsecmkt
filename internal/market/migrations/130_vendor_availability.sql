ALTER TABLE users
    ADD COLUMN vendor_vacation boolean NOT NULL DEFAULT false,
    ADD COLUMN vendor_deactivated boolean NOT NULL DEFAULT false,
    ADD COLUMN vendor_messages_off_while_away boolean NOT NULL DEFAULT false,
    ADD COLUMN vendor_show_last_seen boolean NOT NULL DEFAULT false,
    ADD COLUMN vendor_last_seen timestamptz;
