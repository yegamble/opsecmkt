-- A-85: stop retaining the unused profile contact. Keep the column for compatibility.
UPDATE users SET xmpp='' WHERE xmpp<>'';
