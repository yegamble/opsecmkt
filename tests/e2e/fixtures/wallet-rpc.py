#!/usr/bin/env python3
"""Loopback-only deterministic RPC simulator. No chain, real coins, or production mode."""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

addresses = {}
deposits = []
payouts = []
checks = 0
fail_next_send = set()

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
        method, params = data['method'], data.get('params') or {}
        if method == 'getblockchaininfo':
            checks += 1
            result = {'chain':'regtest','initialblockdownload':False}
        elif method == 'getwalletinfo': result = {'walletname':'opsecmkt'}
        elif method == 'get_address': result = {'address':'5'+'a'*94}
        elif method == 'get_info': result = {'nettype':'stagenet','stagenet':True,'height':100,'target_height':100}
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
                return self.reply({'id':data.get('id'),'error':{'code':-6,'message':'Fixture rejected send before broadcast'}})
            destination = {'address':params[0], 'amount':round(params[1]*100000000)} if currency == 'BTC' else params['destinations'][0]
            txid = format(1000+len(payouts), '064x')
            payouts.append({'currency':currency, **destination, 'txid':txid})
            result = txid if currency == 'BTC' else {'tx_hash':txid}
        else:
            return self.reply({'id':data.get('id'),'error':{'code':-32601,'message':'Unsupported fixture method: '+method}})
        self.reply({'jsonrpc':'2.0','id':data.get('id'),'result':result,'error':None})

if __name__ == '__main__':
    HTTPServer(('127.0.0.1',18083), Handler).serve_forever()
