#!/usr/bin/env python3
"""Start isolated real Bitcoin/Monero chains from operator-verified local binaries.

No downloads, global installation, public peers, production wallet data or real
coins. See docs/local-chain-testing.md for signature verification and usage.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]


def free_port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


def save(path, state):
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(state, indent=2) + '\n')
    temporary.chmod(0o600)
    temporary.replace(path)


def wait_for(action, description, seconds=120):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            return action()
        except (RuntimeError, subprocess.SubprocessError, OSError):
            time.sleep(1)
    raise RuntimeError('Timed out waiting for ' + description)


def stop(state):
    """Refuse to signal a reused PID or a process outside this setup's registry."""
    remaining = []
    for process in reversed(state.get('processes', [])):
        pid = process['pid']

        def matches():
            result = subprocess.run(['ps', '-p', str(pid), '-o', 'args='], capture_output=True, text=True)
            return result.returncode == 0 and all(part in result.stdout for part in (process['binary'], process['marker']))

        if not matches():
            continue
        try:
            os.killpg(pid, signal.SIGTERM)
            deadline = time.monotonic() + 30
            while matches() and time.monotonic() < deadline:
                time.sleep(0.2)
            if matches():
                os.killpg(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        except OSError:
            remaining.append(pid)
    if remaining:
        raise RuntimeError('Could not stop registered process IDs: ' + str(remaining))


def setup(args):
    bitcoin = Path(args.bitcoin_bin).expanduser().resolve()
    monero = Path(args.monero_bin).expanduser().resolve()
    for binary in (bitcoin / 'bitcoind', monero / 'monerod', monero / 'monero-wallet-rpc'):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            raise RuntimeError('Missing executable: ' + str(binary))
    directory = Path(args.state_dir).expanduser().resolve()
    if directory.exists() and any(directory.iterdir()):
        raise RuntimeError('State directory is not empty; choose a new directory or stop the existing chains')
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    directory.chmod(0o700)
    runtime = directory / 'runtime.json'
    state = {'bitcoin': {}, 'monero': {}, 'processes': []}
    save(runtime, state)
    os.environ['LOCAL_CHAIN_RUNTIME'] = str(runtime)
    spec = importlib.util.spec_from_file_location('local_chain', ROOT / 'tests/e2e/fixtures/local-chain.py')
    rpc = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(rpc)
    rpc.STATE = state

    def start(binary, arguments, name, marker):
        with (directory / (name + '.log')).open('w') as log:
            process = subprocess.Popen([str(binary), *arguments], stdin=subprocess.DEVNULL,
                                       stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        state['processes'].append(dict(pid=process.pid, binary=str(binary), marker=marker))
        save(runtime, state)
        return process.pid

    def mine(currency, count):
        subprocess.run([sys.executable, str(ROOT / 'tests/e2e/fixtures/local-chain.py'), 'mine', currency, str(count)],
                       env=os.environ, stdout=subprocess.DEVNULL, check=True, timeout=240)

    try:
        btc_data = directory / 'bitcoin-data'
        btc_data.mkdir()
        node = state['bitcoin']
        node.update(port=free_port(), password=secrets.token_hex(24))
        config = btc_data / 'bitcoin.conf'
        config.write_text('regtest=1\nserver=1\nlisten=0\nnetworkactive=0\nfallbackfee=0.0002\n'
                          'rpcuser=review\nrpcpassword=' + node['password'] + '\n[regtest]\n'
                          'rpcbind=127.0.0.1\nrpcport=' + str(node['port']) + '\n')
        node['pid'] = start(bitcoin / 'bitcoind', ['-datadir=' + str(btc_data)], 'bitcoin', str(btc_data))
        wait_for(lambda: rpc.guard('BTC'), 'Bitcoin regtest')
        for name in ('buyer', 'opsecmkt'):
            rpc.btc('createwallet', {'wallet_name': name})
        node['mining_address'] = rpc.btc('getnewaddress', [], 'buyer')
        save(runtime, state)
        mine('BTC', 120)

        monero_data = directory / 'monero-data'
        monero_data.mkdir()
        state['monero']['daemon_port'] = free_port()
        state['monero']['daemon_pid'] = start(monero / 'monerod', [
            '--stagenet', '--offline', '--fixed-difficulty', '1', '--data-dir', str(monero_data),
            '--rpc-bind-ip', '127.0.0.1', '--rpc-bind-port', str(state['monero']['daemon_port']),
            '--p2p-bind-ip', '127.0.0.1', '--p2p-bind-port', str(free_port()), '--no-igd', '--no-zmq',
            '--non-interactive', '--check-updates', 'disabled', '--max-concurrency', '2'], 'monero', str(monero_data))
        wait_for(lambda: rpc.guard('XMR'), 'offline Monero stagenet')
        for name in ('buyer', 'opsecmkt'):
            wallet_dir = directory / ('monero-wallet-' + name)
            wallet_dir.mkdir()
            wallet = dict(port=free_port(), password=secrets.token_hex(24))
            state['monero'][name] = wallet
            config = wallet_dir / 'rpc.conf'
            config.write_text('stagenet=1\ndaemon-address=127.0.0.1:' + str(state['monero']['daemon_port']) + '\n'
                              'wallet-dir=' + str(wallet_dir) + '\nrpc-bind-ip=127.0.0.1\nrpc-bind-port=' + str(wallet['port']) + '\n'
                              'rpc-login=review:' + wallet['password'] + '\ntrusted-daemon=1\nlog-file=' + str(wallet_dir / 'wallet.log') + '\n')
            wallet['pid'] = start(monero / 'monero-wallet-rpc', ['--config-file', str(config)], 'wallet-' + name, str(config))
            wait_for(lambda: rpc.wallet('create_wallet', {'filename': name, 'password': '', 'language': 'English'}, name=name), name + ' Monero wallet')
            wallet['address'] = rpc.wallet('get_address', {'account_index': 0}, name=name)['address']
            save(runtime, state)
        mine('XMR', 120)
        reserve_address = rpc.btc('getnewaddress', [], 'opsecmkt')
        rpc.btc('sendtoaddress', [reserve_address, 2], 'buyer')
        rpc.wallet('transfer', {'destinations': [{'address': state['monero']['opsecmkt']['address'], 'amount': 50 * 10**12}], 'account_index': 0})
        mine('BTC', 3)
        mine('XMR', 20)
        balance = rpc.wallet('get_balance', {'account_index': 0}, name='opsecmkt')
        if balance['unlocked_balance'] < 50 * 10**12:
            raise RuntimeError('Monero market fee reserve is not unlocked')
        evidence = {'bitcoin': rpc.btc('getblockchaininfo')['chain'],
                    'bitcoin_height': rpc.btc('getblockcount'),
                    'monero': rpc.daemon('get_info')['nettype'], 'monero_offline': True,
                    'monero_height': rpc.daemon('get_info')['height'],
                    'monero_market_unlocked_atomic': balance['unlocked_balance']}
        (directory / 'setup-result.json').write_text(json.dumps(evidence, indent=2) + '\n')
        print('Real isolated chains are ready. Protected runtime: ' + str(runtime))
        print('Keep these processes running during tests. Stop with:')
        print('python3 scripts/setup-local-chains.py --stop ' + str(directory))
    except BaseException:
        stop(state)
        raise


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bitcoin-bin', help='Directory containing verified bitcoind')
    parser.add_argument('--monero-bin', help='Directory containing verified monerod and monero-wallet-rpc')
    parser.add_argument('--state-dir', default='artifacts/local-chains', help='New empty directory for disposable chain data')
    parser.add_argument('--stop', metavar='STATE_DIR', help='Stop registered processes without deleting their data')
    args = parser.parse_args()
    if args.stop:
        state = json.loads((Path(args.stop).expanduser().resolve() / 'runtime.json').read_text())
        stop(state)
        print('Registered local chain processes stopped; data preserved.')
    elif args.bitcoin_bin and args.monero_bin:
        setup(args)
    else:
        parser.error('Provide --bitcoin-bin and --monero-bin, or --stop STATE_DIR')


def interrupted(_signal, _frame):
    raise KeyboardInterrupt('Local-chain setup interrupted')


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupted)
    main()
