#!/usr/bin/env python3
"""Real, isolated local-chain controls for browser tests; never contact public chains.

LOCAL_CHAIN_RUNTIME names a protected runtime file created by the local node
setup. `env` is for programmatic consumption only: its JSON contains RPC secrets.
All other commands output public addresses/transaction IDs and atomic amounts.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from decimal import Decimal
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[3]
STATE = json.loads(Path(os.environ.get('LOCAL_CHAIN_RUNTIME', ROOT / 'artifacts/real-chain/runtime.json')).read_text())


def call(url, method=None, params=None, password=None, digest=False, path=None):
    payload = params if path and path != '/json_rpc' else {'jsonrpc': '2.0', 'id': 'local-chain', 'method': method, 'params': params if params is not None else {}}
    config = 'url = "' + url + (path or '/json_rpc' if digest else path or '') + '"\n'
    if password:
        config += 'user = "review:' + password + '"\n'
    command = ['curl', '--silent', '--show-error', '--fail', '--noproxy', '*', '--max-time', '90',
               '--config', '-', '-H', 'Content-Type: application/json', '--data', json.dumps(payload)]
    if digest:
        command.append('--digest')
    process = subprocess.run(command, input=config, text=True, capture_output=True, timeout=100)
    if process.returncode:
        raise RuntimeError('Local node HTTP request failed (curl status ' + str(process.returncode) + ')')
    result = json.loads(process.stdout)
    if result.get('error'):
        raise RuntimeError('Local node RPC error: ' + str(result['error']))
    value = result.get('result', result)
    if isinstance(value, dict) and value.get('status', 'OK') != 'OK':
        raise RuntimeError('Local node RPC status: ' + str(value['status']))
    return value


def btc(method, params=None, wallet=None):
    node = STATE['bitcoin']
    url = 'http://127.0.0.1:' + str(node['port'])
    if wallet:
        url += '/wallet/' + quote(wallet, safe='')
    return call(url, method, params, node['password'])


def daemon(method, params=None, path=None):
    return call('http://127.0.0.1:' + str(STATE['monero']['daemon_port']),
                method, params, path=path or '/json_rpc')


def wallet(method, params=None, name='buyer'):
    node = STATE['monero'][name]
    return call('http://127.0.0.1:' + str(node['port']), method, params, node['password'], digest=True)


def guard(currency):
    if currency == 'BTC':
        if btc('getblockchaininfo')['chain'] != 'regtest' or btc('getnetworkinfo')['networkactive']:
            raise RuntimeError('Only Bitcoin regtest with networking disabled is allowed')
    elif currency == 'XMR':
        info = daemon('get_info')
        if info.get('nettype') != 'stagenet' or not info.get('offline'):
            raise RuntimeError('Only an offline Monero stagenet chain is allowed')
    else:
        raise ValueError('Currency must be BTC or XMR')


def main():
    command = sys.argv[1]
    if command == 'env':
        currencies = os.environ.get('LOCAL_CHAIN_CURRENCIES', 'BTC,XMR').split(',')
        result = {'BITCOIN_RPC_URL': '', 'MONERO_RPC_URL': '', 'MONERO_WALLET_RPC_URL': ''}
        if 'BTC' in currencies:
            guard('BTC')
            node = STATE['bitcoin']
            result.update(BITCOIN_RPC_URL='http://review:' + node['password'] + '@127.0.0.1:' + str(node['port']), BITCOIN_CHAIN='regtest', BITCOIN_WALLET='opsecmkt')
        if 'XMR' in currencies:
            guard('XMR')
            node = STATE['monero']['opsecmkt']
            result.update(MONERO_RPC_URL='http://127.0.0.1:' + str(STATE['monero']['daemon_port']), MONERO_WALLET_RPC_URL='http://review:' + node['password'] + '@127.0.0.1:' + str(node['port']), MONERO_NETWORK='stagenet')
        print(json.dumps(result))
        return
    currency = sys.argv[2]
    guard(currency)
    if command == 'address':
        result = {'address': btc('getnewaddress', [], 'buyer') if currency == 'BTC' else wallet('create_address', {'account_index': 0})['address']}
    elif command == 'deposit':
        address, amount = sys.argv[3], Decimal(sys.argv[4])
        if amount <= 0:
            raise ValueError('Deposit must be positive')
        if currency == 'BTC':
            result = {'txid': btc('sendtoaddress', [address, float(amount)], 'buyer')}
        else:
            wallet('refresh')
            result = {'txid': wallet('transfer', {'destinations': [{'address': address, 'amount': int(amount * 10**12)}], 'account_index': 0})['tx_hash']}
    elif command == 'mine':
        count = int(sys.argv[3]) if len(sys.argv) > 3 else (3 if currency == 'BTC' else 12)
        if not 1 <= count <= 1000:
            raise ValueError('Mine between 1 and 1000 blocks')
        if currency == 'BTC':
            btc('generatetoaddress', [count, STATE['bitcoin']['mining_address']])
            result = {'height': btc('getblockcount')}
        else:
            target = daemon('get_info')['height'] + count
            daemon(None, {'miner_address': STATE['monero']['buyer']['address'], 'threads_count': 1, 'do_background_mining': False, 'ignore_battery': True}, path='/start_mining')
            try:
                deadline = time.monotonic() + 180
                while daemon('get_info')['height'] < target:
                    if time.monotonic() > deadline:
                        raise RuntimeError('Monero isolated mining timed out')
                    time.sleep(0.2)
            finally:
                daemon(None, {}, path='/stop_mining')
            wallet('refresh')
            wallet('refresh', name='opsecmkt')
            result = {'height': daemon('get_info')['height']}
    elif command == 'received':
        address = sys.argv[3]
        if currency == 'BTC':
            amount = Decimal(str(btc('getreceivedbyaddress', [address, 0], 'buyer')))
            rows = btc('listreceivedbyaddress', [0, True, True, address], 'buyer')
            txids = {txid for row in rows for txid in row.get('txids', [])}
            fee = sum(abs(Decimal(str(btc('gettransaction', [txid], 'opsecmkt').get('fee', 0)))) for txid in txids)
            result = {'amount': int(amount * 10**8), 'fee': int(fee * 10**8), 'transactions': len(txids)}
        else:
            wallet('refresh')
            index = wallet('get_address_index', {'address': address})['index']['minor']
            transfers = wallet('get_transfers', {'in': True, 'pool': True, 'account_index': 0, 'subaddr_indices': [index]})
            rows = [row for kind in ('in', 'pool') for row in transfers.get(kind, [])]
            result = {'amount': sum(row['amount'] for row in rows), 'fee': 0, 'transactions': len({row['txid'] for row in rows})}
    else:
        raise ValueError('Use env, address, deposit, mine or received')
    print(json.dumps(result))


if __name__ == '__main__':
    main()
