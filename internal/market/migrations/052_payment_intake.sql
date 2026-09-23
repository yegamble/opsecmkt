-- Intake policy is separate from wallet connectivity and existing settlements.
-- Rows also provide per-currency locks for address issuance versus admin changes.
INSERT INTO settings(key,value) VALUES
 ('payment_intake_BTC','true'), ('payment_intake_XMR','true')
ON CONFLICT (key) DO NOTHING;
