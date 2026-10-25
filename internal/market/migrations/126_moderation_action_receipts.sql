-- An offline signature grants a bounded review annotation, never target changes.
CREATE TABLE moderation_action_receipts (
 nonce text PRIMARY KEY CHECK(nonce ~ '^[0-9a-f]{64}$'),
 token_hash text NOT NULL UNIQUE CHECK(token_hash ~ '^[0-9a-f]{64}$'),
 suggestion_id text NOT NULL UNIQUE REFERENCES moderation_suggestions(id),
 signer_fingerprint text NOT NULL CHECK(signer_fingerprint ~ '^[0-9a-f]{64}$'),
 action text NOT NULL CHECK(action IN ('accept','dismiss','hold')),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER moderation_action_receipts_immutable BEFORE UPDATE OR DELETE ON moderation_action_receipts FOR EACH ROW EXECUTE FUNCTION moderation_immutable();
