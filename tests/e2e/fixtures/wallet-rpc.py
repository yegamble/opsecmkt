#!/usr/bin/env python3
"""Loopback-only deterministic RPC simulator. No chain, real coins, or production mode."""
import json
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

addresses = {}
deposits = []
payouts = []
checks = 0
fail_next_send = set()
# monero-wallet-rpc get_version: v0.18.5.1 reports RPC version 1.31 ((1 << 16) | 31) from a release build, the
# version the Monero send-error table was verified against (payments_watcher.go moneroVerifiedWalletRPC).
wallet_version = {'version': (1 << 16) | 31, 'release': True}

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, result, status=200):
        body = json.dumps(result).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.reply({'payouts': payouts, 'deposits': deposits, 'checks': checks})

    def do_POST(self):
        global checks
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path == '/test/deposit':
            if data['address'] not in addresses or data['amount'] <= 0:
                return self.reply({'error': 'unknown address or invalid amount'}, 400)
            deposits.append({**data, 'txid': format(len(deposits)+1, '064x'), 'confirmations': 0})
            return self.reply(deposits[-1])
        if self.path == '/test/confirm':
            for d in deposits:
                if d['address'] == data['address']:
                    d['confirmations'] = 3
            return self.reply({'ok': True})
        if self.path == '/test/fail-next-send':
            if data.get('currency') not in ('BTC', 'XMR'):
                return self.reply({'error': 'unknown currency'}, 400)
            fail_next_send.add(data['currency'])
            return self.reply({'ok': True})
        if self.path == '/test/wallet-version':
            if not isinstance(data.get('version'), int) or not isinstance(data.get('release'), bool):
                return self.reply({'error': 'version (int) and release (bool) required'}, 400)
            wallet_version.update(version=data['version'], release=data['release'])
            return self.reply({'ok': True})
        method, params = data['method'], data.get('params') or {}
        if method in ('listdescriptors', 'gethdkeys', 'walletprocesspsbt'):
            # Not on the pooled user's rpcwhitelist (docs/operator-guide.md, "The pooled RPC user"): Core refuses them with
            # HTTP 403 and no body. Bitcoin escrow starts only after every probe is refused (A-440's self-check and
            # QA4W3-1's gethdkeys and walletprocesspsbt probes, pooledKeyProbes).
            self.send_response(403)
            self.send_header('Content-Length', '0')
            self.end_headers()
            return
        if method == 'getblockchaininfo':
            checks += 1
            result = {'chain':'regtest','initialblockdownload':False}
        elif method == 'getwalletinfo': result = {'walletname':'opsecmkt'}
        elif method == 'getnetworkinfo': result = {'version':310100,'subversion':'/Satoshi:31.1.0/'}
        elif method == 'get_version': result = dict(wallet_version)
        elif method == 'get_address': result = {'address':'5'+'a'*94}
        elif method == 'get_height': result = {'height':100}
        # A synced stagenet daemon with peers and a fresh top block (the pooled Monero sync guard, A-449).
        elif method == 'get_info': result = {'nettype':'stagenet','stagenet':True,'height':100,'target_height':100,'synchronized':True,'outgoing_connections_count':8,'incoming_connections_count':0}
        elif method == 'get_last_block_header': result = {'block_header':{'height':99,'timestamp':int(time.time())},'status':'OK'}
        elif method in ('getnewaddress','create_address'):
            index = len(addresses)+1
            address = ('bcrt1qfixture' + str(index).zfill(20)) if method == 'getnewaddress' else ('7' + str(index).zfill(94))
            addresses[address] = index
            result = address if method == 'getnewaddress' else {'address':address,'address_index':index}
        elif method == 'get_address_index': result = {'index':{'major':0,'minor':addresses[params['address']]}}
        elif method == 'validateaddress': result = {'isvalid':params[0].startswith('bcrt1')}
        elif method == 'validate_address': result = {'valid':params['address'].startswith(('5', '7')),'nettype':'stagenet'}
        elif method == 'listreceivedbyaddress':
            result = [{'address':a,'txids':[d['txid'] for d in deposits if d['address']==a]} for a in addresses if a.startswith('bcrt1')]
        elif method == 'gettransaction':
            d = next(d for d in deposits if d['txid']==params[0])
            result = {'confirmations':d['confirmations'],'details':[{'address':d['address'],'category':'receive','amount':f"{d['amount']/100000000:.8f}",'vout':0}]}
        elif method == 'get_transfers':
            result = {'in':[], 'pool':[]}
            for d in deposits:
                if not d['address'].startswith('7'): continue
                result['in' if d['confirmations'] else 'pool'].append({'txid':d['txid'],'amount':d['amount'],'confirmations':d['confirmations'],'unlock_time':0,'subaddr_index':{'major':0,'minor':addresses[d['address']]}})
        elif method in ('sendtoaddress','transfer'):
            currency = 'BTC' if method == 'sendtoaddress' else 'XMR'
            if currency in fail_next_send:
                fail_next_send.remove(currency)
                # A definite pre-broadcast refusal: Bitcoin Core -6 (insufficient funds); monero-wallet-rpc -17
                # (not enough money), one of the codes raised before commit_tx submits (payments_watcher.go).
                code = -6 if currency == 'BTC' else -17
                return self.reply({'id':data.get('id'),'error':{'code':code,'message':'Fixture rejected send before broadcast'}})
            if currency == 'BTC':
                destination = {'address':params[0], 'amount':round(params[1]*100000000)}
                fee_from_amount = len(params) > 4 and params[4] is True  # sendtoaddress subtractfeefromamount
            else:
                destination = params['destinations'][0]
                fee_from_amount = params.get('subtract_fee_from_outputs') == [0]
            # A fixed simulated network fee: taken from the amount when the request asks for that, else paid on top.
            fee = 141 if currency == 'BTC' else 30000000
            received = destination['amount'] - fee if fee_from_amount else destination['amount']
            txid = format(1000+len(payouts), '064x')
            payouts.append({'currency':currency, **destination, 'txid':txid, 'fee':fee, 'fee_from_amount':fee_from_amount, 'received':received})
            result = txid if currency == 'BTC' else {'tx_hash':txid}
        else:
            return self.reply({'id':data.get('id'),'error':{'code':-32601,'message':'Unsupported fixture method: '+method}})
        self.reply({'jsonrpc':'2.0','id':data.get('id'),'result':result,'error':None})

if __name__ == '__main__':
    # --port moves the pooled-wallet simulator (default 18083). --escrow-port also serves the Bitcoin escrow node
    # (escrow_rpc.py: the escrow adapter's PSBT methods, regtest only) on its own loopback port, for the escrow journey.
    # --xmr-wallet-port and --xmr-daemon-port also serve the Monero escrow wallet-rpc and its fakechain daemon
    # (xmr_escrow_rpc.py, SIMULATED: not chain evidence) on their own loopback ports, for the Monero escrow journey.
    import argparse
    import threading
    from http.server import ThreadingHTTPServer
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=18083)
    parser.add_argument('--escrow-port', type=int)
    parser.add_argument('--xmr-wallet-port', type=int)
    parser.add_argument('--xmr-daemon-port', type=int)
    args = parser.parse_args()
    if (args.xmr_wallet_port is None) != (args.xmr_daemon_port is None):
        parser.error('--xmr-wallet-port and --xmr-daemon-port go together')
    if args.escrow_port:
        import escrow_rpc
        escrow = ThreadingHTTPServer(('127.0.0.1', args.escrow_port), escrow_rpc.make_handler(escrow_rpc.EscrowCore()))
        threading.Thread(target=escrow.serve_forever, daemon=True).start()
    if args.xmr_wallet_port:
        import xmr_escrow_rpc
        xmr_escrow_rpc.serve(args.xmr_wallet_port, args.xmr_daemon_port)
    HTTPServer(('127.0.0.1', args.port), Handler).serve_forever()
