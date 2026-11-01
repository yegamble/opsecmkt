-- A-717 (PRIV98-1): a finished XMPP receipt keeps its outcome and time. Clear the
-- address and key fingerprint on rows finished before the worker began doing so;
-- nothing reads either value once a row is finished. Pending rows still need them.
UPDATE xmpp_outbox SET destination='',recipient_fingerprint=''
 WHERE finished_at IS NOT NULL AND (destination<>'' OR recipient_fingerprint<>'');
