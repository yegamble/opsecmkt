-- A-23: monero-wallet-rpc can answer transfer with a JSON-RPC error after the daemon may already have relayed the
-- transaction (-38 when sendrawtransaction times out, -3/-4 from the daemon's reply, -1 after commit). Until now
-- every such error was recorded as a definite rejection. Reclassify those failures as ambiguous (requeue then
-- needs the administrator's broadcast confirmation), keeping the codes raised before submission definite
-- (payments_watcher.go moneroPreSubmitCodes). Only rows with the old wording match, so re-running changes nothing.
UPDATE payouts
SET send_ambiguous = true,
    error = 'Wallet call failed without a definite answer; the transaction may or may not have been broadcast. Check the wallet before requeueing or paying manually. '
        || substr(error, length('The wallet rejected the send; nothing was broadcast. ') + 1)
WHERE state = 'failed' AND currency = 'XMR' AND NOT send_ambiguous
    AND starts_with(error, 'The wallet rejected the send; nothing was broadcast. ')
    AND error !~ ' error -(2|16|17|18|19|20|37): ';
