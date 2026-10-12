#!/usr/bin/env python3
"""Rehearse the bundled-node Bitcoin escrow operator path on regtest, running the installer's printed commands verbatim
(A-513, SRE60-1..4).

Requires Docker/Compose, Python 3, Bash and OpenSSL. It installs a throwaway project with a local regtest Bitcoin node
(`install.sh init`), takes the escrow commands the installer prints at its end exactly as printed, and runs them in
order in a shell: the set-up script, the bitcoin.conf append, the .env append, the node restart, `--verify
--via-compose app` (must print PASS), the escrow signer's seed and `docker compose up -d`. The only substitutions are
the ones the printed text asks the operator to make: the lines it says to paste (taken from the set-up script's own
output) and the URL it printed. It then checks that the app logs "Bitcoin escrow on" and that the signer is ready.

Regtest only, no public network, no real funds. This rehearses the operator path; it is not payment or deployment
evidence: nothing here proves a signet or testnet4 node, a published port or a Tor onion service. The evidence log
holds commands and exit codes, never a password (the set-up script's output is not logged).
"""
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('test_install_helpers', ROOT / 'scripts' / 'test-install.py')
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
require, run, copy_inputs, free_port, installer = (helpers.require, helpers.run, helpers.copy_inputs, helpers.free_port,
                                                   helpers.installer)


def printed_steps(output):
    """The installer's escrow block as shell steps. The block runs from its '# 1. ' comment to the 'Details: ' line;
    comments are skipped, and a command ending in <<'EOF' takes the lines up to EOF with it. Returns a list of
    (command line, heredoc body lines or None)."""
    lines = output.splitlines()
    start = next((i for i, line in enumerate(lines) if line.startswith('# 1. ')), None)
    require(start is not None, 'The installer printed no escrow set-up commands')
    end = next(i for i in range(start, len(lines)) if lines[i].startswith('Details: '))
    steps, i = [], start
    while i < end:
        line = lines[i]
        i += 1
        if line.startswith('#') or not line.strip():
            continue
        body = None
        if line.endswith("<<'EOF'"):
            body = []
            while lines[i] != 'EOF':
                body.append(lines[i])
                i += 1
            i += 1
        steps.append((line, body))
    return steps


def sh(script, directory, environment, log_path, timeout=600, show=False):
    """Run SCRIPT in bash as an operator pasting it; returns (exit code, stdout, stderr). The log gets the first line of
    the script, the exit code and, only with SHOW (output known to hold no secret), the output."""
    result = subprocess.run(['bash', '-c', script], cwd=directory, env=environment, capture_output=True, text=True,
                            timeout=timeout)
    with log_path.open('a') as log:
        log.write(f'$ {script.splitlines()[0]}\n(exit {result.returncode})\n')
        if show:
            log.write(result.stdout + result.stderr)
    return result.returncode, result.stdout, result.stderr


def wait_for(check, description, seconds):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(3)
    raise RuntimeError(description)


def exercise(repo, environment, log_path):
    # The Bitcoin image the Compose file defaults to (the installer asks for one when it is not given an option).
    image = re.search(r'\$\{BITCOIN_IMAGE:-([^}]+)\}', (repo / 'compose.nodes.yaml').read_text())[1]
    code, output = installer(repo, environment, log_path,
                             ['init', '--clearnet', '--behind-proxy', 'no', '--bitcoin', 'local', '--bitcoin-image', image,
                              '--bitcoin-network', 'regtest', '--bitcoin-prune', 'yes', '--monero', 'disabled'], 1500)
    require(code == 0, 'install.sh init with a local regtest node failed')
    steps = printed_steps(output)
    setup, conf_append, env_append, restart, verify, signer_init, up = (
        [step for step in steps if step[0].startswith(prefix)][0] for prefix in (
            './scripts/escrow-node-setup.sh --chain regtest', 'docker compose exec -T -u bitcoin bitcoin sh -c',
            'cat >> .env', 'docker compose restart bitcoin', "ESCROW_BTC_RPC_URL='<the URL it printed>' ",
            'docker compose run --rm --no-deps escrow-signer init', 'docker compose up -d'))
    require(len(steps) == 7, f'expected 7 printed commands, found {len(steps)}')

    # "once the local Bitcoin node has synced": a regtest node is ready as soon as bitcoin-cli answers.
    probe = setup[0].split(" --cli '")[1][:-1] + ' -chain=regtest getblockchaininfo'
    wait_for(lambda: sh(probe, repo, environment, log_path, 60)[0] == 0, 'the regtest node never answered bitcoin-cli', 180)

    code, printed_lines, _ = sh(setup[0], repo, environment, log_path, 300)
    require(code == 0, 'the printed set-up command failed')
    lines = printed_lines.splitlines()
    conf_lines = [line for line in lines if re.match(r'(rpcauth|rpcwhitelistdefault|rpcwhitelist)=', line)]
    env_lines = [line for line in lines if line.startswith('ESCROW_')]
    require(len(conf_lines) == 5 and len(env_lines) == 5, f'unexpected printed lines: {len(conf_lines)}, {len(env_lines)}')
    url = next(line.split('=', 1)[1] for line in env_lines if line.startswith('ESCROW_BTC_RPC_URL='))
    require(url.endswith('@bitcoin:8332'), 'the printed escrow URL is not the bundled node at its pinned port')

    def pasted(step, replacements):
        """The printed command and its heredoc with the placeholder comment replaced by the lines it asks for."""
        command, body = step
        body = [line for line in body if not line.startswith('# paste')] + replacements
        return '\n'.join([command, *body, 'EOF']) + '\n'

    require(sh(pasted(conf_append, conf_lines), repo, environment, log_path)[0] == 0, 'the bitcoin.conf append failed')
    code, conf, _ = sh('docker compose exec -T bitcoin cat /data/bitcoin.conf', repo, environment, log_path)
    require(code == 0 and all(line in conf.splitlines() for line in conf_lines),
            'bitcoin.conf does not hold the printed lines exactly (is the $ in rpcauth intact?)')
    require(sh(pasted(env_append, env_lines), repo, environment, log_path)[0] == 0, 'the .env append failed')
    require(sh(restart[0], repo, environment, log_path, 300)[0] == 0, 'the node restart failed')

    verify_environment = dict(environment)
    verify_command = verify[0].replace("ESCROW_BTC_RPC_URL='<the URL it printed>' ", '', 1)
    verify_environment['ESCROW_BTC_RPC_URL'] = url
    code, out, err = sh(verify_command, repo, verify_environment, log_path, 600, show=True)
    require(code == 0 and re.search(r'^PASS: ', out, re.M), 'the printed --verify did not print PASS: ' + out + err)
    require(url.split('@')[0].split(':')[-1] not in out + err, '--verify printed the escrow password')

    code, out, err = sh(signer_init[0], repo, environment, log_path, 900, show=True)
    require(code == 0 and re.search(r'[0-9a-f]{8}', out + err), 'the escrow signer init failed: ' + out + err)
    require(sh(up[0], repo, environment, log_path, 900)[0] == 0, 'docker compose up -d failed after the escrow settings')

    def logs():
        return sh('docker compose logs --no-color app escrow-signer', repo, environment, log_path)[1]
    wait_for(lambda: 'escrow: Bitcoin escrow on' in logs(), 'the app never logged Bitcoin escrow on', 180)
    wait_for(lambda: 'escrow signer ready' in logs(), 'the app never logged that the escrow signer is ready', 180)
    final = logs()
    for secret in re.findall(r'://[^/@\s:]+:([0-9a-f]{64})@', ' '.join(env_lines)):
        require(secret not in final, 'a generated RPC password appears in the app or signer logs')
    code, out, _ = sh('./scripts/install.sh check', repo, environment, log_path, 300, show=True)
    require(code == 0, 'install.sh check refused the configuration after the escrow settings: ' + out)
    return {'installer': 'passed', 'printed_commands': len(steps), 'verify': 'PASS', 'app_log': 'escrow on',
            'signer': 'ready', 'chain': 'regtest'}


def main():
    for executable in ('docker', 'bash', 'openssl'):
        require(shutil.which(executable), 'Missing executable: ' + executable)
    project = 'opsecmkt-rehearsal-' + uuid.uuid4().hex[:12]
    logs = ROOT / 'artifacts' / 'escrow-node-rehearsal' / project
    logs.mkdir(parents=True, mode=0o700)
    log_path = logs / 'rehearsal.log'
    environment = {key: value for key, value in os.environ.items()
                   if key in ('PATH', 'HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG')}
    environment.update(COMPOSE_PROJECT_NAME=project, APP_PORT=str(free_port()))
    failure, result, cleanup_failures = None, None, []
    with tempfile.TemporaryDirectory(prefix=project + '-') as temporary:
        repo = Path(temporary) / 'repo'
        copy_inputs(repo)
        try:
            result = exercise(repo, environment, log_path)
        except BaseException as error:
            failure = error
        finally:
            with log_path.open('a') as log:
                for command in (['docker', 'compose', 'down', '--volumes', '--remove-orphans'],
                                ['docker', 'image', 'rm', project + '-app', project + '-escrow-signer']):
                    if command[1] == 'compose' and not (repo / '.env').exists():
                        continue
                    try:
                        code = run(command, repo, environment, log, 180)
                        if code and command[1] == 'compose':
                            cleanup_failures.append('Compose cleanup failed')
                    except BaseException as error:
                        cleanup_failures.append(type(error).__name__ + ' during cleanup')
    print('Escrow node rehearsal log: ' + str(log_path))
    if failure:
        raise RuntimeError('Escrow node rehearsal failed: ' + str(failure)) from failure
    require(not cleanup_failures, '; '.join(cleanup_failures))
    (logs / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Installer-printed escrow commands (setup, conf append, .env append, restart, --verify PASS, signer init, up) '
          'passed on regtest.')


def interrupted(_signal, _frame):
    raise KeyboardInterrupt('Escrow node rehearsal interrupted')


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupted)
    main()
