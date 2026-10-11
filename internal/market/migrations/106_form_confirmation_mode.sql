-- A-130: dynamic OwnTx forms remember whether their original successful
-- mutation required password/TOTP confirmation. Existing fixed-mode receipts
-- retain false; their routes continue enforcing confirmation independently.
ALTER TABLE form_submissions
 ADD COLUMN confirmation_required boolean NOT NULL DEFAULT false;
