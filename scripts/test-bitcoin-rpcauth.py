#!/usr/bin/env python3
"""Manual A-144 proof: private Core regtest RPC auth through the real Compose bitcoin command.

Uses the cached pinned image only: no pulls, builds, public-network peers, wallets or funds. Each run owns a
random Compose project with internal-only networks and removes its container, network and volume in finally.
The password lives only in process memory and a mode-0600 temporary env file; output never includes credentials.
Run from the repository root with `python3 scripts/test-bitcoin-rpcauth.py` on a Docker host.
"""
import hashlib
import hmac
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
IMAGE = 'bitcoin/bitcoin:31.1'
CORE_VERSION = 310100


def call(args, env, input_text=None, timeout=60):
    return subprocess.run(args, cwd=ROOT, env=env, input=input_text, text=True,
                          capture_output=True, timeout=timeout, check=False)


def main():
    project = 'a144rpcauth' + secrets.token_hex(4)
    password = secrets.token_hex(24)
    salt = secrets.token_hex(16)
    digest = hmac.new(salt.encode(), password.encode(), hashlib.sha256).hexdigest()
    verifier = f'marketplace:{salt}${digest}'
    # Shell values must never override the private test file or affect profile selection.
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(('BITCOIN_', 'MONERO_', 'COMPOSE_')) and key != 'DATABASE_URL'}
    with tempfile.TemporaryDirectory(prefix='a144-rpcauth-') as temporary:
        directory = Path(temporary)
        dotenv = directory / '.env'
        dotenv.write_text("DATABASE_URL='postgres://unused@db/unused'\n"
                          "BITCOIN_CHAIN='regtest'\n"
                          f"BITCOIN_RPC_AUTH='{verifier}'\n")
        dotenv.chmod(0o600)
        overlay = directory / 'isolated.yaml'
        overlay.write_text('networks:\n  backend:\n    internal: true\n  node_egress:\n    internal: true\n')
        compose = ['docker', 'compose', '--env-file', str(dotenv), '-p', project]
        for path in (ROOT / 'compose.yaml', ROOT / 'compose.clearnet.yaml',
                     ROOT / 'compose.nodes.yaml', overlay):
            compose.extend(('-f', str(path)))
        compose.extend(('--profile', 'bitcoin'))
        try:
            image = call(['docker', 'image', 'inspect', IMAGE], env)
            if image.returncode:
                raise RuntimeError('the pinned Bitcoin Core image is not cached; no image was pulled')
            rendered = call(compose + ['config', '--format', 'json'], env)
            if rendered.returncode:
                raise RuntimeError('Compose did not render the isolated configuration')
            config = json.loads(rendered.stdout)
            if not all(network['internal'] for network in config['networks'].values()):
                raise RuntimeError('an isolated network is not internal')
            if config['services']['bitcoin']['image'] != IMAGE:
                raise RuntimeError('the Bitcoin Core image differs from the pinned version')
            started = call(compose + ['up', '-d', '--no-build', '--pull', 'never', '--no-deps', 'bitcoin'], env, timeout=120)
            if started.returncode:
                raise RuntimeError('the isolated regtest node did not start')
            result = call(compose + ['ps', '-q', 'bitcoin'], env)
            cid = result.stdout.strip()
            if result.returncode or not cid:
                raise RuntimeError('the isolated node has no running container')
            result = call(['docker', 'inspect', cid], env)
            if result.returncode:
                raise RuntimeError('could not inspect the isolated node')
            container = json.loads(result.stdout)[0]
            command = container['Config']['Cmd']
            if [arg for arg in command if arg.startswith('-rpcauth=')] != ['-rpcauth=' + verifier]:
                raise RuntimeError('the runtime verifier is not the one-dollar Core format')
            if any(password in arg or arg.startswith(('-rpcpassword=', '-rpcuser=')) for arg in command):
                raise RuntimeError('the node argv contains a plaintext password or legacy RPC credential option')
            if container['HostConfig'].get('PortBindings'):
                raise RuntimeError('the isolated node publishes a host port')
            for network in container['NetworkSettings']['Networks'].values():
                inspected = call(['docker', 'network', 'inspect', network['NetworkID']], env)
                if inspected.returncode or not json.loads(inspected.stdout)[0]['Internal']:
                    raise RuntimeError('the running node joined a non-internal network')
            cli = ['docker', 'exec', '-i', cid, 'bitcoin-cli', '-regtest', '-rpcconnect=127.0.0.1',
                   '-rpcport=8332', '-rpcuser=marketplace', '-stdinrpcpass']
            good = None
            for _ in range(25):
                good = call(cli + ['getblockchaininfo'], env, password + '\n', timeout=10)
                if good.returncode == 0:
                    break
                time.sleep(1)
            if good.returncode or json.loads(good.stdout).get('chain') != 'regtest':
                raise RuntimeError('the correct private RPC password did not authenticate on regtest')
            version = call(cli + ['getnetworkinfo'], env, password + '\n', timeout=10)
            if version.returncode or json.loads(version.stdout).get('version') != CORE_VERSION:
                raise RuntimeError('the node is not the pinned Bitcoin Core version')
            wrong = call(cli + ['getblockchaininfo'], env, secrets.token_hex(24) + '\n', timeout=10)
            rejected = 'authorization failed' in (wrong.stderr + wrong.stdout).lower()
            if wrong.returncode == 0 or not rejected:
                raise RuntimeError('a wrong RPC password did not receive Core authentication rejection')
            still_good = call(cli + ['getblockchaininfo'], env, password + '\n', timeout=10)
            if still_good.returncode or json.loads(still_good.stdout).get('chain') != 'regtest':
                raise RuntimeError('the correct RPC password failed after the rejected attempt')
        finally:
            removed = call(compose + ['down', '-v'], env, timeout=120)
            if removed.returncode:
                # Name only our uniquely owned project; never run a broad prune or kill.
                raise RuntimeError(f'owned disposable Compose project {project} and its volume were not removed')
    print('PASS: Core 31.1 regtest, internal-only networks, one literal $ verifier in argv, '
          'no password in argv, correct RPC auth accepted, wrong RPC auth rejected, node stayed healthy')
    print('PASS: owned disposable Compose project and volume removed')


if __name__ == '__main__':
    main()
