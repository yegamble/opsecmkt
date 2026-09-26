"""Exercise installer decisions in disposable directories with no real Docker."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
INSTALLER = REPOSITORY / 'scripts' / 'install.sh'
PRUNED_MONERO_FLAGS = '--prune-blockchain --sync-pruned-blocks'
MAIN_ONION = 'abcd' * 14 + '.onion'
MIRROR_ONION = 'wxyz' * 14 + '.onion'
HOSTNAME = 'cat /var/lib/tor/marketplace/hostname'
# The installer's view of every Compose container on the host (A-135), taken by `check`.
ALL_CONTAINERS = ('ps -a --filter label=com.docker.compose.project --format '
                  '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.project.working_dir"}}')
CHECK = [ALL_CONTAINERS, 'compose config --quiet']
UP = 'compose up -d --build --wait'
STATUS = 'compose exec -T app /app/server -status'
# Keys whose values are secrets: upgrade never invents them and no command prints them.
SECRET_KEYS = ('DATABASE_URL', 'POSTGRES_PASSWORD', 'APP_SECRET_KEY', 'SETUP_TOKEN', 'AUDIT_SIGNING_KEY',
               'BITCOIN_RPC_PASSWORD', 'MONERO_WALLET_RPC_PASSWORD')
PASSWORD = 'correct horse battery staple'


def preflight(project='opsecmkt'):
    """The Docker calls init makes before asking anything: the project's containers, then its volumes."""
    return ['compose version',
            f'ps -a --filter label=com.docker.compose.project={project} '
            '--format dir={{.Label "com.docker.compose.project.working_dir"}}',
            f'volume ls -q --filter label=com.docker.compose.project={project}']


PREFLIGHT = preflight()


def hex_value(length=64, seed='x'):
    """A random-looking hex string that passes the app's secret rules."""
    value = ''
    while len(value) < length:
        value += hashlib.sha256(f'{seed}:{len(value)}'.encode()).hexdigest()
    return value[:length]


def env_text(values):
    return ''.join(f"{key}='{value}'\n" for key, value in values.items())


def installed_env(**overrides):
    """An .env as init writes it for Tor with the internal database and no nodes."""
    password = hex_value(48, 'db')
    values = {
        'DATABASE_URL': f'postgres://opsecmkt:{password}@db:5432/opsecmkt?sslmode=disable',
        'POSTGRES_PASSWORD': password,
        'APP_SECRET_KEY': hex_value(64, 'key'),
        'SETUP_TOKEN': hex_value(64, 'token'),
        'APP_MODE': 'tor',
        'COOKIE_SECURE': 'false',
        'APP_PORT': '8080',
        'COMPOSE_PROJECT_NAME': 'opsecmkt',
        'AUDIT_SIGNING_KEY': hex_value(64, 'audit'),
        'BITCOIN_CHAIN': '',
        'MONERO_NETWORK': '',
        'COMPOSE_FILE': 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml',
        'COMPOSE_PROFILES': 'internal-db',
    }
    values.update(overrides)
    return {key: value for key, value in values.items() if value is not None}


def clearnet_env(**overrides):
    return installed_env(**{'APP_MODE': 'clearnet', 'COOKIE_SECURE': 'true',
                            'COMPOSE_FILE': 'compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml',
                            **overrides})


def alpha_env(**overrides):
    """An .env as the v0.1.0-alpha.1 installer wrote it: Tor, internal database, a local Monero node."""
    password = hex_value(48, 'alpha-db')
    values = {
        'DATABASE_URL': f'postgres://opsecmkt:{password}@db:5432/opsecmkt?sslmode=disable',
        'POSTGRES_PASSWORD': password,
        'SETUP_TOKEN': hex_value(64, 'alpha-token'),
        'APP_MODE': 'tor',
        'COOKIE_SECURE': 'false',
        'MONERO_IMAGE': 'ghcr.io/sethforprivacy/simple-monerod:latest',
        'MONERO_RPC_URL': 'http://monero:18081',
        'COMPOSE_FILE': 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml',
        'COMPOSE_PROFILES': 'internal-db,monero',
    }
    values.update(overrides)
    return {key: value for key, value in values.items() if value is not None}


def example_values():
    """The key=value lines of .env.example, quotes removed."""
    values = {}
    for line in (REPOSITORY / '.env.example').read_text().splitlines():
        if line and not line.startswith('#'):
            key, value = line.split('=', 1)
            values[key] = value[1:-1] if value[:1] == "'" else value
    return values


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='opsecmkt-installer-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / 'scripts').mkdir()
        shutil.copyfile(INSTALLER, self.root / 'scripts' / 'install.sh')
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.state = self.root / 'stub-state'
        self.state.mkdir()
        self.log = self.root / 'docker-calls.log'
        # A Docker stand-in: it records every call (its arguments, and its environment as JSON) and answers the
        # commands the installer makes from INSTALLER_TEST_* variables and files in the stub-state directory.
        self.stub('docker', '''
import json
import os
from pathlib import Path
import sys
args = sys.argv[1:]
log = os.environ['INSTALLER_TEST_LOG']
state = Path(os.environ['INSTALLER_TEST_STATE'])
with Path(log).open('a') as out:
    out.write(' '.join(args) + '\\n')
with Path(log + '.env').open('a') as out:
    out.write(json.dumps(dict(os.environ)) + '\\n')
def flag(name):
    return os.environ.get(name) == '1'
def installed():
    path = state / 'installed'
    return path.read_text() if path.exists() else os.environ.get('INSTALLER_TEST_INSTALLED', 'false')
def print_status():
    if flag('INSTALLER_TEST_STATUS_FAIL'):
        sys.exit('status: database connection failed')
    print(f'installed={installed()} schema=021')
    sys.exit(0)
if args == ['compose', 'version']:
    sys.exit(0)
if args == ['compose', 'config', '--quiet']:
    assert Path('.env').is_file(), 'Configuration must exist before validation'
    sys.exit(42 if flag('INSTALLER_TEST_BAD_CONFIG') else 0)
if args[:2] == ['compose', 'up']:
    sys.exit(1 if flag('INSTALLER_TEST_UNHEALTHY') else 0)
if args[:2] == ['ps', '-a']:
    if flag('INSTALLER_TEST_PS_FAIL'):
        sys.exit('Cannot connect to the Docker daemon')
    project_dirs = os.environ.get('INSTALLER_TEST_CONTAINERS', '').splitlines()
    if '--format' in args:
        template = args[args.index('--format') + 1]
        label = '{{.Label "com.docker.compose.project.working_dir"}}'
        assert label in template, template
        if template.startswith('dir='):
            # One project's containers, one line each (an empty label stays empty).
            for directory in project_dirs:
                print(template.replace(label, directory))
        else:
            assert template == '{{.Label "com.docker.compose.project"}}|' + label, template
            project = os.environ.get('INSTALLER_TEST_PROJECT', 'opsecmkt')
            for directory in project_dirs:
                print(f'{project}|{directory}')
            for line in os.environ.get('INSTALLER_TEST_OTHER_CONTAINERS', '').splitlines():
                print(line)
        sys.exit(0)
    assert '-q' in args and 'label=com.docker.compose.service=tor-mirror' in args, args
    if flag('INSTALLER_TEST_MIRROR_CONTAINER'):
        print('0123456789ab')
    sys.exit(0)
if args[:2] == ['volume', 'ls']:
    print(os.environ.get('INSTALLER_TEST_VOLUMES', ''), end='')
    sys.exit(0)
if args[:3] == ['compose', 'exec', '-T']:
    service, command = args[3], args[4:]
    if service == 'app' and command == ['/app/server', '-status']:
        if flag('INSTALLER_TEST_APP_DOWN'):
            sys.exit('service "app" is not running')
        print_status()
    if service in ('tor', 'tor-mirror') and command == ['cat', '/var/lib/tor/marketplace/hostname']:
        counter = state / (service + '-reads')
        reads = int(counter.read_text()) + 1 if counter.exists() else 1
        counter.write_text(str(reads))
        waits = os.environ.get('INSTALLER_TEST_' + service.upper().replace('-', '_') + '_ONION_WAITS',
                               os.environ.get('INSTALLER_TEST_ONION_WAITS', '0'))
        if flag('INSTALLER_TEST_NO_ONION') or reads <= int(waits):
            sys.exit(1)
        print(os.environ['INSTALLER_TEST_' + service.upper().replace('-', '_') + '_ONION'])
        sys.exit(0)
if args[:4] == ['compose', 'run', '--rm', '-T']:
    rest = args[4:]
    if rest == ['--no-deps', 'app', '-status']:
        print_status()
    if rest[:2] == ['app', '-bootstrap-admin']:
        (state / 'bootstrap-stdin').write_text(sys.stdin.read())
        if flag('INSTALLER_TEST_BOOTSTRAP_FAIL'):
            sys.exit('bootstrap-admin: the market is already set up')
        (state / 'installed').write_text('true')
        print(f'Setup complete: administrator {rest[2]} created. Sign in with that handle and password.')
        sys.exit(0)
if args[:3] == ['compose', 'ps', '-a']:
    print('  app: Up 1 minute (healthy)')
    sys.exit(0)
sys.exit('Unexpected docker command')
''')
        self.stub('openssl', '''
import hashlib
import os
from pathlib import Path
import sys
assert sys.argv[1:3] == ['rand', '-hex']
counter = Path(os.environ['INSTALLER_TEST_LOG'] + '.openssl')
call = int(counter.read_text()) + 1 if counter.exists() else 1
counter.write_text(str(call))
if str(call) == os.environ.get('INSTALLER_TEST_OPENSSL_EMPTY_CALL'):
    sys.exit(1)  # generation failure: no output
# Distinct, random-looking values: the installer's check applies the app's secret rules to them.
value = ''
while len(value) < 2 * int(sys.argv[3]):
    value += hashlib.sha256(f'{call}:{len(value)}'.encode()).hexdigest()
value = value[:2 * int(sys.argv[3])]
with Path(os.environ['INSTALLER_TEST_LOG'] + '.secrets').open('a') as out:
    out.write(value + '\\n')
print(value)
''')
        # The installer fetches nothing itself: these record any call so a test can fail on it.
        for tool in ('curl', 'wget'):
            self.stub(tool, 'echo "$0 $*" >> "$INSTALLER_TEST_LOG.fetch"; exit 99', interpreter='/bin/sh')
        # Onion waits poll up to 90 times; a trivial stub needs no interpreter startup under CI load.
        self.stub('sleep', 'exit 0', interpreter='/bin/sh')
        self.backup_stub()

    def stub(self, name, body, interpreter=None):
        path = self.bin / name
        path.write_text(f'#!{interpreter or sys.executable}\n' + body)
        path.chmod(0o700)

    def backup_stub(self, code=0):
        path = self.root / 'scripts' / 'backup.sh'
        path.write_text('#!/bin/sh\necho "$* project=$COMPOSE_PROJECT_NAME recipient=$AGE_RECIPIENT" >> '
                        f'"$INSTALLER_TEST_LOG.backup"\nexit {code}\n')
        path.chmod(0o700)

    def run_installer(self, answers, bad_config=False, openssl_empty_call=None, arguments=None, extra_env=None, timeout=30):
        # Do not source or copy the user's environment file. Compose is a stub;
        # all writes and generated credentials stay inside this temporary repo.
        env = {
            'PATH': str(self.bin) + os.pathsep + os.defpath,
            'HOME': str(self.root),
            'INSTALLER_TEST_LOG': str(self.log),
            'INSTALLER_TEST_STATE': str(self.state),
            'INSTALLER_TEST_BAD_CONFIG': '1' if bad_config else '0',
            'INSTALLER_TEST_OPENSSL_EMPTY_CALL': str(openssl_empty_call or ''),
            'INSTALLER_TEST_TOR_ONION': MAIN_ONION,
            'INSTALLER_TEST_TOR_MIRROR_ONION': MIRROR_ONION,
            # macOS's Python can redirect bytecode caches beneath the temporary
            # HOME. Stubs must not keep populating it while cleanup removes it.
            'PYTHONDONTWRITEBYTECODE': '1',
            **(extra_env or {}),
        }
        Path(str(self.log) + '.openssl').unlink(missing_ok=True)
        process = subprocess.Popen(
            ['/bin/bash', str(self.root / 'scripts' / 'install.sh'), *(arguments or [])],
            cwd=self.root, env=env, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            start_new_session=True,
        )
        try:
            # No answers means empty input: any question then stops the installer at "end of input".
            stdout, stderr = process.communicate('\n'.join(answers) + '\n' if answers else '', timeout=timeout)
        except subprocess.TimeoutExpired as expired:
            # Killing bash alone leaves Python stubs alive with open pipes and
            # a temporary HOME. Kill only this test's dedicated process group.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            try:
                expired.output, expired.stderr = process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                # Never wait indefinitely even if an unexpected descendant has
                # escaped the process group while retaining an output pipe.
                process.stdout.close()
                process.stderr.close()
            raise expired
        return subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)

    def command(self, *arguments, extra_env=None, answers=None):
        """Run a subcommand on an existing .env, with this checkout's containers present."""
        self.log.unlink(missing_ok=True)
        env = {'INSTALLER_TEST_CONTAINERS': str(self.root), **(extra_env or {})}
        return self.run_installer(answers, arguments=list(arguments), extra_env=env)

    def write_env(self, values_or_text, mode=0o600):
        path = self.root / '.env'
        path.write_bytes(values_or_text if isinstance(values_or_text, bytes) else
                         (values_or_text if isinstance(values_or_text, str) else env_text(values_or_text)).encode())
        path.chmod(mode)
        return path.read_bytes()

    def set_installed(self, value):
        (self.state / 'installed').write_text('true' if value else 'false')

    def generated_secrets(self):
        path = Path(str(self.log) + '.secrets')
        return path.read_text().split() if path.exists() else []

    def pre_upgrade_copies(self):
        return sorted(self.root.glob('.env.pre-upgrade-*'))

    def assert_no_secret_printed(self, result, config, allow_token=False):
        output = result.stdout + result.stderr
        for key in SECRET_KEYS + ('BITCOIN_RPC_URL', 'MONERO_WALLET_RPC_URL'):
            value = config.get(key, '')
            if not value or (allow_token and key == 'SETUP_TOKEN'):
                continue
            self.assertNotIn(value, output, f'{key} was printed')
        for value in self.generated_secrets():
            if not (allow_token and value == config.get('SETUP_TOKEN')):
                self.assertNotIn(value, output, 'a generated secret was printed')

    def assert_token_only_in_final_block(self, result, token):
        """SETUP_TOKEN appears once, on the 'Setup token:' line after 'Ready.', near the end of stdout."""
        self.assertNotIn(token, result.stderr)
        self.assertEqual(result.stdout.count(token), 1)
        lines = result.stdout.splitlines()
        index = next(i for i, line in enumerate(lines) if token in line)
        self.assertEqual(lines[index], 'Setup token: ' + token)
        self.assertTrue(lines[index - 1].startswith('Ready. Open '), lines[index - 1])
        self.assertGreaterEqual(index, len(lines) - 8)

    def test_timeout_stops_stub_descendants_without_bytecode_writes(self):
        self.stub('docker', '''
import os
from pathlib import Path
import subprocess
import sys
import time
assert sys.dont_write_bytecode
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
Path(os.environ['INSTALLER_TEST_LOG'] + '.child').write_text(str(child.pid))
time.sleep(60)
''')
        with self.assertRaises(subprocess.TimeoutExpired):
            self.run_installer([], timeout=2)
        child = int(Path(str(self.log) + '.child').read_text())
        # A killed orphan may briefly await reaping; a zombie cannot write to
        # the temporary directory or keep captured output pipes open.
        state = subprocess.run(
            ['/bin/ps', '-o', 'stat=', '-p', str(child)],
            capture_output=True, text=True, timeout=5, check=False,
        ).stdout.strip()
        self.assertTrue(not state or state.startswith('Z'), state)
        self.assertEqual(list(self.root.rglob('*.pyc')), [])

    def config(self):
        path = self.root / '.env'
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        return {key: value[1:-1] for key, value in (
            line.split('=', 1) for line in path.read_text().splitlines()
        )}

    def calls(self):
        return self.log.read_text().splitlines() if self.log.exists() else []

    def assert_started(self, result, preflight_calls=PREFLIGHT):
        self.assertEqual(result.returncode, 0, 'Installer unexpectedly failed: ' + result.stderr)
        onion = [f'compose exec -T tor {HOSTNAME}'] if self.config()['APP_MODE'] == 'tor' else []
        self.assertEqual(self.calls(), [*preflight_calls, *CHECK, UP, *onion])
        self.assertEqual(list(self.root.glob('.env.install.*')), [])
        self.assertEqual(list(self.root.glob('.env.edit.*')), [])

    def assert_refused_before_writing(self, result, calls):
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.calls(), calls)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual(list(self.root.glob('.env.install.*')), [])
        # Nothing was asked: the refusal comes before the first question.
        self.assertEqual(result.stdout, '')

    # ---- init: questions, answers and the .env it writes ------------------------------------------------

    def test_internal_clearnet_defaults(self):
        # Every question answered with Enter: mode, database, local HTTP, then for each coin the node
        # choice, image, network and pruning. The defaults are pruned local test-network nodes.
        result = self.run_installer([''] * 11)
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['APP_MODE'], 'clearnet')
        self.assertEqual(config['COOKIE_SECURE'], 'true')
        # Existing installs have no COMPOSE_PROJECT_NAME and use compose.yaml's name; new ones keep that default.
        self.assertEqual(config['COMPOSE_PROJECT_NAME'], 'opsecmkt')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero,monero-wallet')
        self.assertEqual(config['COMPOSE_FILE'],
                         'compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml')
        self.assertIn('@db:5432/opsecmkt', config['DATABASE_URL'])
        self.assertEqual(len(config['POSTGRES_PASSWORD']), 48)
        self.assertEqual(len(config['SETUP_TOKEN']), 64)
        self.assertEqual(len(config['AUDIT_SIGNING_KEY']), 64)
        self.assert_token_only_in_final_block(result, config['SETUP_TOKEN'])
        self.assert_no_secret_printed(result, config, allow_token=True)
        self.assertEqual(config['BITCOIN_CHAIN'], 'testnet4')
        self.assertEqual(config['MONERO_NETWORK'], 'stagenet')
        images = self.compose_image_defaults()
        self.assertEqual(config['BITCOIN_IMAGE'], images['BITCOIN_IMAGE'])
        self.assertEqual(config['MONERO_IMAGE'], images['MONERO_IMAGE'])
        self.assertEqual(config['MONERO_WALLET_IMAGE'], images['MONERO_WALLET_IMAGE'])
        # Pruned by default, written explicitly so the operator can see and change it in .env.
        self.assertEqual(config['BITCOIN_PRUNE_MB'], self.compose_bitcoin_prune_default())
        self.assertEqual(config['MONERO_PRUNE_FLAGS'], PRUNED_MONERO_FLAGS)
        self.assertEqual(config['BITCOIN_RPC_URL'],
                         'http://marketplace:' + config['BITCOIN_RPC_PASSWORD'] + '@bitcoin:8332')
        self.assertEqual(config['MONERO_WALLET_RPC_URL'],
                         'http://marketplace:' + config['MONERO_WALLET_RPC_PASSWORD'] + '@monero-wallet:18083')
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])
        # The node question states the disk and first-sync cost before the operator answers.
        self.assertIn('pruned Bitcoin Core node', result.stdout)
        self.assertIn('pruned monerod', result.stdout)
        self.assertIn('first sync', result.stdout)
        self.assertNotIn('Full node', result.stdout)
        self.assertIn('docs/testnet-runbook.md', result.stdout)
        self.assertIn('Secure cookies require HTTPS', result.stdout)
        # Behind a proxy: the snippets name the chosen loopback port and forward Host.
        self.assertIn('reverse_proxy 127.0.0.1:8080', result.stdout)
        self.assertIn('proxy_set_header Host $host;', result.stdout)
        self.assertIn('https://YOUR-DOMAIN/setup', result.stdout)

    def compose_bitcoin_prune_default(self):
        # The installer writes the same default the Compose file falls back to for an .env without it.
        text = (REPOSITORY / 'compose.nodes.yaml').read_text()
        match = re.search(r'-prune=\$\{BITCOIN_PRUNE_MB:-([0-9]+)\}', text)
        self.assertIsNotNone(match, 'compose.nodes.yaml must run bitcoind with -prune=${BITCOIN_PRUNE_MB:-N}')
        self.assertGreaterEqual(int(match.group(1)), 550)
        self.assertIn('${MONERO_PRUNE_FLAGS-' + PRUNED_MONERO_FLAGS + '}', text)
        return match.group(1)

    def compose_image_defaults(self):
        # The installer and .env.example write the image each compose.nodes.yaml service falls back to.
        text = (REPOSITORY / 'compose.nodes.yaml').read_text()
        defaults = dict(re.findall(r'^    image: \$\{([A-Z_]+_IMAGE):-([^}]+)\}$', text, re.MULTILINE))
        self.assertEqual(sorted(defaults), ['BITCOIN_IMAGE', 'MONERO_IMAGE', 'MONERO_WALLET_IMAGE'])
        return defaults

    def test_node_images_default_to_the_verified_release_tags(self):
        # War-room A-100: walletRejected's error tables are verified against these releases; a floating tag
        # could upgrade the wallet under them and make a post-broadcast error look definite.
        defaults = self.compose_image_defaults()
        tags = {}
        for variable, image in defaults.items():
            tag = image.partition('@')[0].rpartition('/')[2].partition(':')[2]  # a digest may follow the tag
            self.assertNotIn(tag, ('', 'latest'), f'{variable} defaults to {image!r}, not a pinned release tag')
            tags[variable] = tag
        # Bumping a tag means re-verifying the code tables first (docs/operator-guide.md#node-image-versions);
        # the verified version named in payments_watcher.go must change in the same commit.
        watcher = (REPOSITORY / 'internal/market/payments_watcher.go').read_text()
        for verified in (f'Bitcoin Core (v{tags["BITCOIN_IMAGE"]} ', f'(monero {tags["MONERO_WALLET_IMAGE"]}: '):
            self.assertTrue(verified in watcher, f'payments_watcher.go does not name {verified!r} as verified')
        self.assertEqual(tags['MONERO_IMAGE'], tags['MONERO_WALLET_IMAGE'])
        example = dict(line.split('=', 1) for line in (REPOSITORY / '.env.example').read_text().splitlines()
                       if line.split('=', 1)[0] in defaults)
        self.assertEqual(example, defaults)

    def test_internal_clearnet_without_nodes(self):
        result = self.run_installer(['', '', '', 'disabled', 'disabled'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        # No node chosen: nothing forces a chain, so a node added later may use any test network.
        self.assertEqual(config['BITCOIN_CHAIN'], '')
        self.assertEqual(config['MONERO_NETWORK'], '')
        self.assertNotIn('BITCOIN_RPC_URL', config)
        self.assertNotIn('MONERO_WALLET_RPC_URL', config)
        self.assertNotIn('BITCOIN_PRUNE_MB', config)
        self.assertNotIn('MONERO_PRUNE_FLAGS', config)
        self.assertNotIn('docs/testnet-runbook.md', result.stdout)

    def test_local_nodes_can_opt_out_of_pruning(self):
        result = self.run_installer(['tor', '', 'local', '', 'signet', 'no', 'local', '', 'testnet', 'no'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero,monero-wallet')
        # -prune=0 disables pruning in bitcoind; a blank flag list drops --prune-blockchain for monerod.
        self.assertEqual(config['BITCOIN_PRUNE_MB'], '0')
        self.assertEqual(config['MONERO_PRUNE_FLAGS'], '')
        self.assertEqual(config['BITCOIN_CHAIN'], 'signet')
        self.assertEqual(config['MONERO_NETWORK'], 'testnet')
        self.assertEqual(result.stdout.count('Full node'), 2)
        # Compose reads an unquoted blank followed by a comment ("MONERO_PRUNE_FLAGS= # x") as the value "# x"
        # (A-149): every line the installer writes is a single-quoted value with nothing after it.
        for line in (self.root / '.env').read_text().splitlines():
            self.assertRegex(line, r"^[A-Z0-9_]+='[^'\n]*'$")
        self.assertIn("\nMONERO_PRUNE_FLAGS=''\n", (self.root / '.env').read_text())

    def test_env_example_value_lines_have_no_inline_comments(self):
        # Same Compose rule for the template operators copy and hand-edit (A-149).
        for line in (REPOSITORY / '.env.example').read_text().splitlines():
            if line and not line.startswith('#'):
                self.assertRegex(line, r'^[A-Z0-9_]+=[^#]*$')

    def test_local_node_prune_answer_yes_is_pruned(self):
        result = self.run_installer(['tor', '', 'local', '', '', 'yes', 'local', '', '', 'yes'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['BITCOIN_PRUNE_MB'], self.compose_bitcoin_prune_default())
        self.assertEqual(config['MONERO_PRUNE_FLAGS'], PRUNED_MONERO_FLAGS)
        self.assertNotIn('Full node', result.stdout)

    def test_local_shortcut_needs_no_input_and_disables_inherited_wallets(self):
        result = self.run_installer([], arguments=['--local'], extra_env={
            'APP_PORT': '18085', 'DATABASE_URL': 'postgres://unrelated/existing',
            'BITCOIN_RPC_URL': 'http://unrelated-wallet', 'COMPOSE_PROFILES': 'bitcoin',
        })
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COOKIE_SECURE'], 'false')
        self.assertEqual(config['APP_PORT'], '18085')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('@db:5432/opsecmkt', config['DATABASE_URL'])
        for key in ('BITCOIN_RPC_URL', 'MONERO_RPC_URL', 'MONERO_WALLET_RPC_URL'):
            self.assertEqual(config[key], '')
        self.assertIn('Ready. Open http://127.0.0.1:18085/setup', result.stdout)
        self.assertNotIn('docs/testnet-runbook.md', result.stdout)
        # Developer mode never starts nodes, so it writes no node images or pruning settings.
        for key in ('BITCOIN_IMAGE', 'MONERO_IMAGE', 'BITCOIN_PRUNE_MB', 'MONERO_PRUNE_FLAGS'):
            self.assertNotIn(key, config)
        self.assertEqual(config['BITCOIN_CHAIN'], '')
        self.assertEqual(config['MONERO_NETWORK'], '')
        self.assertNotIn('pruned', result.stdout)
        self.assert_token_only_in_final_block(result, config['SETUP_TOKEN'])
        # Compose sees .env alone: the shell's values never reach a docker command.
        for line in Path(str(self.log) + '.env').read_text().splitlines():
            environment = json.loads(line)
            for key in ('DATABASE_URL', 'BITCOIN_RPC_URL', 'COMPOSE_PROFILES', 'APP_PORT'):
                self.assertNotIn(key, environment)

    def test_unhealthy_local_app_does_not_report_ready(self):
        result = self.run_installer([], arguments=['--local'], extra_env={'INSTALLER_TEST_UNHEALTHY': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('did not become healthy', result.stderr)
        self.assertNotIn('Ready.', result.stdout)
        self.assertNotIn(self.config()['SETUP_TOKEN'], result.stdout + result.stderr)  # kept for a restart

    def test_invalid_local_port_starts_nothing(self):
        result = self.run_installer([], arguments=['--local'], extra_env={'APP_PORT': '65536'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('APP_PORT must', result.stderr)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual(self.calls(), PREFLIGHT)

    def test_external_clearnet_local_http(self):
        url = 'postgresql://test:local-only@database:5432/scratch?sslmode=require'
        result = self.run_installer(['clearnet', url, 'yes', 'disabled', 'disabled'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['DATABASE_URL'], url)
        self.assertEqual(config['COOKIE_SECURE'], 'false')
        self.assertEqual(config['COMPOSE_PROFILES'], '')
        self.assertEqual(config['COMPOSE_FILE'], 'compose.yaml:compose.clearnet.yaml:compose.nodes.yaml')
        self.assertNotIn(url, result.stdout + result.stderr)

    def test_internal_tor_stays_without_direct_egress(self):
        result = self.run_installer(['tor', '', 'disabled', 'disabled'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COOKIE_SECURE'], 'false')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('compose.tor.yaml', config['COMPOSE_FILE'])
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_external_tor_explicitly_enables_egress(self):
        result = self.run_installer(['tor', 'postgres://test@database/scratch', 'disabled', 'disabled'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], '')
        self.assertNotIn('compose.internal-db.yaml', config['COMPOSE_FILE'])
        self.assertTrue(config['COMPOSE_FILE'].endswith(':compose.external-egress.yaml'))
        self.assertIn('direct network egress is enabled', result.stdout)

    def test_tor_profiles_match_the_mirror_command_check(self):
        # A-140: scripts/test-mirror-commands.sh (run by CI) checks the guide's onion-mirror commands against .env
        # files holding COMPOSE_FILE and COMPOSE_PROFILES exactly as the installer writes them for Tor. If either
        # side drifts, this fails.
        script = (REPOSITORY / 'scripts' / 'test-mirror-commands.sh').read_text()
        external = 'postgres://test@database/scratch'
        cases = [
            ('internal', ['tor', ''] + [''] * 8, 'internal-db,bitcoin,monero,monero-wallet'),
            ('internal', ['tor', '', 'disabled', 'disabled'], 'internal-db'),
            ('external', ['tor', external] + [''] * 8, 'bitcoin,monero,monero-wallet'),
            ('external', ['tor', external, 'disabled', 'disabled'], ''),
        ]
        for database, answers, profiles in cases:
            with self.subTest(database=database, profiles=profiles):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                self.assert_started(self.run_installer(answers))
                config = self.config()
                self.assertEqual(config['COMPOSE_PROFILES'], profiles)
                self.assertIn(f"'{database}|{config['COMPOSE_FILE']}|{profiles}'", script)

    def test_documented_mirror_commands_keep_the_installers_profiles(self):
        # `docker compose --profile X` replaces COMPOSE_PROFILES for that command instead of adding to it, so on
        # an internal-db install it drops the database ("app depends on undefined service db"). The documents
        # add `mirror` to COMPOSE_PROFILES in .env instead (install.sh add-mirror does it), and CI runs the
        # marked guide block.
        for name in ('README.md', 'UPGRADING.md', '.env.example', 'docs/operator-guide.md', 'docs/testnet-runbook.md',
                     'scripts/install.sh'):
            self.assertNotIn('docker compose --profile', (REPOSITORY / name).read_text(), name)
        self.assertNotIn('--profile', INSTALLER.read_text())
        guide = (REPOSITORY / 'docs' / 'operator-guide.md').read_text()
        self.assertEqual(guide.count('<!-- ops-cmd: onion-mirror -->'), 1)
        self.assertIn("COMPOSE_PROFILES='internal-db,bitcoin,monero,monero-wallet,mirror'", guide)
        self.assertIn('./scripts/install.sh add-mirror', guide)

    def test_external_rpc_also_enables_tor_egress(self):
        result = self.run_installer(['tor', '', 'external', 'https://rpc.example.test', 'disabled'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['BITCOIN_RPC_URL'], 'https://rpc.example.test')
        # External nodes are not forced onto one chain; the app accepts any test chain the node reports.
        self.assertEqual(config['BITCOIN_CHAIN'], '')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('compose.external-egress.yaml', config['COMPOSE_FILE'])
        self.assertIn('docs/testnet-runbook.md', result.stdout)

    def test_local_node_profiles_and_images(self):
        result = self.run_installer([
            'tor', '', 'local', 'reviewed-bitcoin:test', 'testnet4', 'yes',
            'local', 'reviewed-monero:test', 'stagenet', 'yes',
        ])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero,monero-wallet')
        self.assertEqual(config['BITCOIN_IMAGE'], 'reviewed-bitcoin:test')
        self.assertEqual(config['MONERO_IMAGE'], 'reviewed-monero:test')
        self.assertEqual(config['MONERO_WALLET_IMAGE'], self.compose_image_defaults()['MONERO_WALLET_IMAGE'])
        self.assertEqual(len(config['BITCOIN_RPC_PASSWORD']), 48)
        self.assertEqual(config['BITCOIN_RPC_URL'],
                         'http://marketplace:' + config['BITCOIN_RPC_PASSWORD'] + '@bitcoin:8332')
        self.assertEqual(config['BITCOIN_CHAIN'], 'testnet4')
        self.assertEqual(config['MONERO_NETWORK'], 'stagenet')
        self.assertEqual(config['MONERO_RPC_URL'], 'http://monero:18081')
        # monero-wallet-rpc runs with --rpc-login; the app authenticates with the same generated password.
        self.assertEqual(len(config['MONERO_WALLET_RPC_PASSWORD']), 48)
        self.assertEqual(config['MONERO_WALLET_RPC_URL'],
                         'http://marketplace:' + config['MONERO_WALLET_RPC_PASSWORD'] + '@monero-wallet:18083')
        self.assert_no_secret_printed(result, config, allow_token=True)
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_local_node_rejects_incomplete_or_malformed_image_before_compose(self):
        for image in ('@sha256', 'vendor/node@sha256:deadbeef', 'vendor/node:'):
            with self.subTest(image=image):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(['clearnet', '', 'yes', 'local', image])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Invalid', result.stderr)
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertFalse((self.root / '.env').exists())

    def test_local_node_accepts_a_complete_digest_pinned_reference(self):
        digest_image = 'registry.example:5000/bitcoin/core@sha256:' + ('a' * 64)
        result = self.run_installer(['clearnet', '', 'yes', 'local', digest_image, 'testnet4', '', 'disabled'])
        self.assert_started(result)
        self.assertEqual(self.config()['BITCOIN_IMAGE'], digest_image)

    def test_local_node_defaults_to_current_trusted_images(self):
        result = self.run_installer(['tor', '', 'local', '', 'testnet4', '', 'local', '', 'stagenet', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual({key: config[key] for key in ('BITCOIN_IMAGE', 'MONERO_IMAGE', 'MONERO_WALLET_IMAGE')},
                         self.compose_image_defaults())

    def test_local_mainnet_choice_fails_closed_before_writing_config(self):
        result = self.run_installer(['tor', '', 'local', '', 'live'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Live Bitcoin payments are disabled', result.stderr)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual(self.calls(), PREFLIGHT)

    def test_external_monero_wallet_rpc(self):
        daemon, wallet = 'https://monerod.example.test', 'http://user:pass@wallet.example.test:18083'
        result = self.run_installer(['tor', '', 'disabled', 'external', daemon, wallet])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['MONERO_RPC_URL'], daemon)
        self.assertEqual(config['MONERO_WALLET_RPC_URL'], wallet)
        self.assertEqual(config['MONERO_NETWORK'], '')
        self.assertNotIn('MONERO_WALLET_RPC_PASSWORD', config)
        self.assertNotIn('WARNING', result.stderr)
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('compose.external-egress.yaml', config['COMPOSE_FILE'])
        self.assertNotIn(wallet, result.stdout + result.stderr)

    def test_external_monero_daemon_without_wallet_warns(self):
        result = self.run_installer(['tor', '', 'disabled', 'external', 'https://monerod.example.test', ''])
        self.assert_started(result)
        config = self.config()
        self.assertNotIn('MONERO_WALLET_RPC_URL', config)
        self.assertIn('Monero payments stay disabled', result.stderr)
        # Nothing in the app would use the daemon, so Tor mode opens no direct egress for it.
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_secret_generation_failure_starts_nothing(self):
        # openssl calls: 1 database password, 2 APP_SECRET_KEY, 3 setup token, 4 audit key, then one per local node.
        scenarios = [
            (1, ['clearnet', '', '', 'disabled', 'disabled']),
            (2, ['clearnet', '', '', 'disabled', 'disabled']),
            (3, ['clearnet', '', '', 'disabled', 'disabled']),
            (5, ['tor', '', 'local', 'reviewed-bitcoin:test', 'testnet4', '', 'disabled']),
            (5, ['tor', '', 'disabled', 'local', 'reviewed-monero:test', 'stagenet', '']),
            (6, ['tor', '', 'local', 'reviewed-bitcoin:test', 'testnet4', '',
                 'local', 'reviewed-monero:test', 'stagenet', '']),
        ]
        for call, answers in scenarios:
            with self.subTest(call=call, answers=answers):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers, openssl_empty_call=call)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Could not generate a random secret', result.stderr)
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(list(self.root.glob('.env.install.*')), [])

    def test_containers_from_another_checkout_are_refused_before_anything_is_written(self):
        # A second checkout under the same project name would recreate the other installation's containers
        # with new secrets, and its `docker compose down -v` would delete that installation's volumes.
        other = '/srv/live-opsecmkt'
        result = self.run_installer([''] * 11, extra_env={
            'INSTALLER_TEST_CONTAINERS': f'{other}\n{self.root}\n{other}\n',
        })
        self.assert_refused_before_writing(result, PREFLIGHT[:2])
        self.assertIn("Compose project 'opsecmkt' already has containers created from another directory", result.stderr)
        self.assertEqual(result.stderr.count(other), 1)
        self.assertNotIn(str(self.root) + '\n', result.stderr)
        self.assertIn('work in its directory', result.stderr)
        self.assertIn('COMPOSE_PROJECT_NAME=opsecmkt-rehearsal ./scripts/install.sh', result.stderr)

    def test_local_shortcut_refusal_repeats_the_local_argument(self):
        result = self.run_installer([], arguments=['--local'], extra_env={
            'INSTALLER_TEST_CONTAINERS': '/srv/live-opsecmkt\n',
        })
        self.assert_refused_before_writing(result, PREFLIGHT[:2])
        self.assertIn('COMPOSE_PROJECT_NAME=opsecmkt-rehearsal ./scripts/install.sh --local', result.stderr)

    def test_unlabelled_project_container_is_refused(self):
        result = self.run_installer([], arguments=['--local'], extra_env={'INSTALLER_TEST_CONTAINERS': '\n'})
        self.assert_refused_before_writing(result, PREFLIGHT[:2])
        self.assertIn('(no working directory label)', result.stderr)

    def test_containers_from_this_checkout_proceed(self):
        # Also through a symlink: Compose labels the path it was started from, not the resolved one.
        links = tempfile.TemporaryDirectory(prefix='opsecmkt-installer-link-')
        self.addCleanup(links.cleanup)
        link = Path(links.name) / 'checkout'
        link.symlink_to(self.root, target_is_directory=True)
        result = self.run_installer([], arguments=['--local'], extra_env={
            'INSTALLER_TEST_CONTAINERS': f'{self.root}\n{link}\n',
        })
        self.assert_started(result)
        self.assertEqual(self.config()['COMPOSE_PROJECT_NAME'], 'opsecmkt')

    def test_project_volumes_without_containers_are_refused(self):
        # `docker compose down` (README's normal stop) removes the containers, so their labels are gone, but
        # keeps the volumes. Their checkout cannot be identified; starting on them here is refused too.
        result = self.run_installer([''] * 11, extra_env={
            'INSTALLER_TEST_VOLUMES': 'opsecmkt_postgres_data\nopsecmkt_bitcoin_data\n',
        })
        self.assert_refused_before_writing(result, PREFLIGHT)
        self.assertIn("Compose project 'opsecmkt' has volumes but no containers", result.stderr)
        self.assertIn('  opsecmkt_postgres_data\n  opsecmkt_bitcoin_data\n', result.stderr)
        self.assertIn('COMPOSE_PROJECT_NAME=opsecmkt-rehearsal ./scripts/install.sh', result.stderr)

    def test_init_refuses_existing_project_volumes_even_with_own_containers(self):
        # I10: containers from this very checkout (its .env deleted) still sit on volumes created with other
        # secrets; init never starts on them.
        result = self.run_installer([], arguments=['--local'], extra_env={
            'INSTALLER_TEST_CONTAINERS': f'{self.root}\n',
            'INSTALLER_TEST_VOLUMES': 'opsecmkt_postgres_data\n',
        })
        self.assert_refused_before_writing(result, PREFLIGHT)
        self.assertIn("Compose project 'opsecmkt' already has volumes, created by an earlier install from this directory",
                      result.stderr)
        self.assertIn('  opsecmkt_postgres_data\n', result.stderr)

    def test_distinct_project_name_is_checked_and_saved(self):
        result = self.run_installer([], arguments=['--local'], extra_env={
            'COMPOSE_PROJECT_NAME': 'opsecmkt-rehearsal', 'APP_PORT': '8081',
        })
        self.assert_started(result, preflight('opsecmkt-rehearsal'))
        config = self.config()
        # Saved so every later docker compose command in this checkout (backup.sh, restore.sh, down) uses it.
        self.assertEqual(config['COMPOSE_PROJECT_NAME'], 'opsecmkt-rehearsal')
        self.assertEqual(config['APP_PORT'], '8081')

    def test_invalid_project_name_or_unreachable_docker_starts_nothing(self):
        for name in ('Opsecmkt', '-opsecmkt', 'opsec mkt', "opsecmkt'"):
            with self.subTest(name=name):
                self.log.unlink(missing_ok=True)
                result = self.run_installer([], arguments=['--local'], extra_env={'COMPOSE_PROJECT_NAME': name})
                self.assert_refused_before_writing(result, ['compose version'])
                self.assertIn('COMPOSE_PROJECT_NAME must use lower-case letters', result.stderr)
        self.log.unlink(missing_ok=True)
        result = self.run_installer([], arguments=['--local'], extra_env={'INSTALLER_TEST_PS_FAIL': '1'})
        self.assert_refused_before_writing(result, PREFLIGHT[:2])
        self.assertIn('Could not list Docker containers', result.stderr)

    def test_existing_env_is_never_changed(self):
        path = self.root / '.env'
        original = b'EXISTING_TEST_VALUE=preserve-me\n'
        path.write_bytes(original)
        for arguments in ([], ['--local'], ['init', '--tor']):
            with self.subTest(arguments=arguments):
                self.log.unlink(missing_ok=True)
                result = self.run_installer([], arguments=arguments)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('./scripts/install.sh upgrade', result.stderr)
                self.assertEqual(path.read_bytes(), original)
                self.assertEqual(self.calls(), ['compose version'])

    def test_failed_compose_validation_never_starts_services(self):
        result = self.run_installer(['tor', '', 'disabled', 'disabled'], bad_config=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.calls(), [*PREFLIGHT, *CHECK])
        self.config()  # The retained configuration still has restrictive permissions.

    def test_invalid_input_leaves_no_configuration_or_services(self):
        scenarios = [
            ['invalid-mode'],
            ['tor', 'https://not-postgres'],
            ['tor', '', 'invalid-node'],
            ['tor', '', 'external', 'ftp://invalid-rpc'],
            ['tor', '', 'disabled', 'external', 'https://monerod.example.test', 'ftp://invalid-wallet'],
            ['tor', '', 'local', ''],
            ['tor', '', 'local', '', 'testnet4', 'maybe', 'disabled'],
            ['tor', '', 'disabled', 'local', '', 'stagenet', 'maybe'],
            ['tor', "postgres://test:quote'@database/scratch", 'disabled', 'disabled'],
        ]
        for answers in scenarios:
            with self.subTest(case=scenarios.index(answers)):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(list(self.root.glob('.env.install.*')), [])

    # ---- init: S2 -------------------------------------------------------------------------------------

    def test_init_tor_prints_onion_setup_url_and_token(self):
        # The onion is written a few seconds after the services are healthy: init waits for it.
        result = self.run_installer([], arguments=['init', '--tor', '--nodes', 'none'],
                                    extra_env={'INSTALLER_TEST_ONION_WAITS': '3'})
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assertEqual(self.calls(), [*PREFLIGHT, *CHECK, UP] + [f'compose exec -T tor {HOSTNAME}'] * 4)
        self.assertIn(f'Onion address: {MAIN_ONION}\n', result.stdout)
        self.assertIn(f'Ready. Open http://{MAIN_ONION}/setup in Tor Browser', result.stdout)
        self.assertIn('1-3 more minutes', result.stdout)
        self.assert_token_only_in_final_block(result, config['SETUP_TOKEN'])

    def test_init_tor_without_an_onion_after_90_seconds_still_finishes(self):
        result = self.run_installer([], arguments=['init', '--tor', '--nodes', 'none'],
                                    extra_env={'INSTALLER_TEST_NO_ONION': '1'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls().count(f'compose exec -T tor {HOSTNAME}'), 90)
        self.assertIn('Onion address: not written yet', result.stdout)
        self.assert_token_only_in_final_block(result, self.config()['SETUP_TOKEN'])

    def test_init_prints_no_secret_but_the_setup_token(self):
        cases = [
            ([''] * 11, []),
            (['tor', '', 'local', '', '', '', 'external', 'https://monerod.example.test',
              'http://user:wallet-secret@wallet.example.test:18083'], []),
            ([], ['--local']),
        ]
        for answers, arguments in cases:
            with self.subTest(arguments=arguments, answers=answers[:2]):
                (self.root / '.env').unlink(missing_ok=True)
                Path(str(self.log) + '.secrets').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers, arguments=arguments)
                self.assertEqual(result.returncode, 0, result.stderr)
                config = self.config()
                self.assert_no_secret_printed(result, config, allow_token=True)
                self.assert_token_only_in_final_block(result, config['SETUP_TOKEN'])

    def test_init_writes_distinct_secret_key_and_setup_token(self):
        result = self.run_installer([], arguments=['--local'])
        self.assert_started(result)
        config = self.config()
        for key in ('APP_SECRET_KEY', 'SETUP_TOKEN'):
            self.assertRegex(config[key], r'^[0-9a-f]{64}$')
        self.assertNotEqual(config['APP_SECRET_KEY'], config['SETUP_TOKEN'])
        self.assertNotIn(config['APP_SECRET_KEY'], result.stdout + result.stderr)

    def test_init_flags_need_no_prompt(self):
        # Empty input: a question would stop the installer at "end of input".
        wallet = 'http://user:wallet-secret@wallet.example.test:18083'
        cases = [
            (['init', '--tor', '--nodes', 'none'],
             dict(APP_MODE='tor', COOKIE_SECURE='false', COMPOSE_PROFILES='internal-db')),
            (['--clearnet', '--behind-proxy', 'no', '--nodes', 'local'],
             dict(APP_MODE='clearnet', COOKIE_SECURE='false', COMPOSE_PROFILES='internal-db,bitcoin,monero,monero-wallet',
                  BITCOIN_CHAIN='testnet4', MONERO_NETWORK='stagenet', BITCOIN_PRUNE_MB='2000',
                  BITCOIN_IMAGE='bitcoin/bitcoin:31.1')),
            (['init', '--clearnet', '--behind-proxy', 'yes', '--nodes', 'none'],
             dict(APP_MODE='clearnet', COOKIE_SECURE='true', COMPOSE_PROFILES='internal-db')),
            (['init', '--tor', '--external-db', 'postgres://market@database.example/market', '--nodes', 'custom',
              '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test', '--bitcoin-network', 'signet',
              '--bitcoin-prune', 'no', '--monero', 'external', '--monero-rpc-url', 'https://monerod.example.test',
              '--monero-wallet-rpc-url', wallet],
             dict(APP_MODE='tor', DATABASE_URL='postgres://market@database.example/market', COMPOSE_PROFILES='bitcoin',
                  BITCOIN_IMAGE='reviewed-bitcoin:test', BITCOIN_CHAIN='signet', BITCOIN_PRUNE_MB='0',
                  MONERO_RPC_URL='https://monerod.example.test', MONERO_WALLET_RPC_URL=wallet,
                  COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.external-egress.yaml')),
            (['--tor', '--bitcoin', 'external', '--bitcoin-rpc-url', 'https://rpc.example.test', '--monero', 'local',
              '--monero-image', 'reviewed-monero:test', '--monero-network', 'testnet', '--monero-prune', 'yes'],
             dict(BITCOIN_RPC_URL='https://rpc.example.test', MONERO_IMAGE='reviewed-monero:test', MONERO_NETWORK='testnet',
                  MONERO_PRUNE_FLAGS=PRUNED_MONERO_FLAGS, COMPOSE_PROFILES='internal-db,monero,monero-wallet')),
        ]
        for arguments, expected in cases:
            with self.subTest(arguments=arguments):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=arguments)
                self.assert_started(result)
                config = self.config()
                self.assertEqual({key: config.get(key) for key in expected}, expected)
                self.assert_no_secret_printed(result, config, allow_token=True)

    def test_init_flags_ask_only_what_they_leave_open(self):
        # --tor alone asks one question: the nodes.
        result = self.run_installer(['none'], arguments=['init', '--tor'])
        self.assert_started(result)
        self.assertEqual(self.config()['COMPOSE_PROFILES'], 'internal-db')
        (self.root / '.env').unlink()
        self.log.unlink()
        # --clearnet asks about the proxy and the nodes.
        result = self.run_installer(['no', 'none'], arguments=['init', '--clearnet'])
        self.assert_started(result)
        self.assertEqual(self.config()['COOKIE_SECURE'], 'false')

    def test_init_rejects_bad_options_before_docker(self):
        cases = [['--nodes', 'maybe'], ['--tor', '--clearnet'], ['--local', '--tor'], ['--site-name', 'Market'],
                 ['--admin', 'no'], ['--bitcoin'], ['--unknown'], ['init', 'extra']]
        for arguments in cases:
            with self.subTest(arguments=arguments):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=arguments)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(self.calls(), [])
                self.assertFalse((self.root / '.env').exists())
        self.log.unlink(missing_ok=True)
        result = self.run_installer(None, arguments=['--tor', '--nodes', 'local', '--bitcoin', 'external'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('RPC URL', result.stderr)
        self.assertFalse((self.root / '.env').exists())

    def test_bare_invocation_and_local_alias_are_init(self):
        def written(answers, arguments):
            (self.root / '.env').unlink(missing_ok=True)
            self.log.unlink(missing_ok=True)
            result = self.run_installer(answers, arguments=arguments)
            self.assert_started(result)
            return {key: ('<secret>' if key in SECRET_KEYS or key.endswith('_RPC_URL') and value else value)
                    for key, value in self.config().items()}, self.calls()
        self.assertEqual(written([''] * 11, []), written([''] * 11, ['init']))
        self.assertEqual(written(None, ['--local']), written(None, ['init', '--local']))

    # ---- init --admin (terminal bootstrap) ------------------------------------------------------------

    def test_init_admin_pipes_password_on_stdin_and_prints_no_token(self):
        result = self.run_installer([PASSWORD, PASSWORD],
                                    arguments=['--local', '--admin', 'first_admin', '--site-name', 'Review Market'])
        self.assertEqual(result.returncode, 0, result.stderr)
        run = 'compose run --rm -T app -bootstrap-admin first_admin -site-name Review Market'
        self.assertEqual(self.calls(), [*PREFLIGHT, *CHECK, UP, run, 'compose up -d --wait'])
        self.assertEqual((self.state / 'bootstrap-stdin').read_text(), PASSWORD + '\n')
        config = self.config()
        # The token opened nothing and grants nothing now; it is gone from .env and was never shown.
        self.assertNotIn('SETUP_TOKEN', config)
        self.assertRegex(config['APP_SECRET_KEY'], r'^[0-9a-f]{64}$')
        self.assertNotIn('Setup token', result.stdout)
        self.assert_no_secret_printed(result, config)
        self.assertIn('Administrator first_admin is created; sign in at http://127.0.0.1:8080/login', result.stdout)
        # Without --site-name the server's default name is used.
        (self.root / '.env').unlink()
        self.log.unlink()
        self.set_installed(False)
        result = self.run_installer([PASSWORD, PASSWORD], arguments=['init', '--tor', '--nodes', 'none', '--admin', 'onion_admin'])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('compose run --rm -T app -bootstrap-admin onion_admin', self.calls())
        self.assertIn(f'sign in at http://{MAIN_ONION}/login', result.stdout)

    def test_init_admin_password_never_in_argv_or_env(self):
        result = self.run_installer([PASSWORD, PASSWORD], arguments=['--local', '--admin', 'first_admin'])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(PASSWORD, result.stdout + result.stderr)
        self.assertNotIn(PASSWORD, (self.root / '.env').read_text())
        self.assertNotIn(PASSWORD, self.log.read_text())  # every docker argv
        for line in Path(str(self.log) + '.env').read_text().splitlines():
            self.assertNotIn(PASSWORD, ''.join(json.loads(line).values()))
        # Only the bootstrap's stdin carried it.
        for path in self.root.rglob('*'):
            if path.is_file() and path.name != 'bootstrap-stdin':
                self.assertNotIn(PASSWORD.encode(), path.read_bytes(), path)

    def test_init_admin_mismatched_passwords_refused(self):
        for answers, message in (([PASSWORD, PASSWORD + 'x'], 'do not match'), (['short', 'short'], '12-72 bytes'),
                                 ([PASSWORD], 'No password')):
            with self.subTest(message=message):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers, arguments=['--local', '--admin', 'first_admin'])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(self.generated_secrets(), [])
                self.assertNotIn(PASSWORD, result.stdout + result.stderr)

    def test_init_admin_failure_keeps_the_token_path(self):
        result = self.run_installer([PASSWORD, PASSWORD], arguments=['--local', '--admin', 'first_admin'],
                                    extra_env={'INSTALLER_TEST_BOOTSTRAP_FAIL': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('setup-token', result.stderr)
        config = self.config()
        self.assertIn('SETUP_TOKEN', config)
        self.assert_no_secret_printed(result, config)

    # ---- the .env editor ------------------------------------------------------------------------------

    def test_env_set_preserves_other_lines_byte_for_byte(self):
        # setup-token --new rewrites SETUP_TOKEN through env_set; every other byte stays.
        key = hex_value(64, 'key')
        base = (f"# operator's note: keep\nDATABASE_URL='postgres://opsecmkt:pw@db:5432/opsecmkt'\r\n"
                f"export APP_MODE=clearnet\n\n  COOKIE_SECURE=false   \nAPP_SECRET_KEY='{key}'\n"
                "SETUP_TOKEN_NOTE=unrelated key with the same prefix\nMONERO_PRUNE_FLAGS=\n")
        old_line = f"SETUP_TOKEN='{hex_value(64, 'old')}'\n"
        for name, before, tail in (('replaced in place', base + old_line + 'LAST=no-final-newline', 'LAST=no-final-newline'),
                                   ('appended after an unterminated line', base + 'LAST=no-final-newline', None)):
            with self.subTest(name):
                self.write_env(before.encode(), mode=0o644)
                self.set_installed(False)
                result = self.command('setup-token', '--new')
                self.assertEqual(result.returncode, 0, result.stderr)
                after = (self.root / '.env').read_bytes().decode()
                token = re.search(r"^SETUP_TOKEN='([0-9a-f]{64})'$", after, re.M)[1]
                new_line = f"SETUP_TOKEN='{token}'\n"
                expected = before.replace(old_line, new_line) if tail else before + '\n' + new_line
                self.assertEqual(after, expected)
                self.assertEqual(stat.S_IMODE((self.root / '.env').stat().st_mode), 0o600)
                self.assertEqual(list(self.root.glob('.env.edit.*')), [])

    def test_env_unset_removes_only_that_key(self):
        # status on an installed market retires SETUP_TOKEN through env_unset (duplicates included).
        values = installed_env()
        text = env_text(values) + "SETUP_TOKEN='a-later-duplicate-line-with-enough-length-0123456789'\nSETUP_TOKEN_NOTE=keep\n"
        self.write_env(text)
        self.set_installed(True)
        result = self.command('status')
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = ''.join(line for line in text.splitlines(keepends=True) if not line.startswith('SETUP_TOKEN='))
        self.assertEqual((self.root / '.env').read_text(), expected)
        self.assertIn('Removed SETUP_TOKEN from .env', result.stdout)
        self.assert_no_secret_printed(result, values)
        # Without APP_SECRET_KEY the token is still the key root: status leaves it.
        values = installed_env(APP_SECRET_KEY=None)
        original = self.write_env(values)
        result = self.command('status')
        self.assertEqual((self.root / '.env').read_bytes(), original)

    # ---- check ------------------------------------------------------------------------------------------

    def check_env(self, values, expected_message=None, extra_env=None):
        self.write_env(values)
        result = self.command('check', extra_env=extra_env)
        if expected_message is None:
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('Configuration OK.', result.stdout)
            self.assertEqual(self.calls(), ['compose version', *CHECK])
        else:
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(expected_message, result.stderr)
            self.assertNotIn('compose config --quiet', self.calls())
        self.assert_no_secret_printed(result, values)
        return result

    def test_check_accepts_installer_configurations(self):
        self.check_env(installed_env())
        self.check_env(clearnet_env())
        self.check_env(installed_env(SETUP_TOKEN=None))  # after setup
        self.check_env(installed_env(APP_SECRET_KEY=None))  # legacy key root
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,mirror'))

    def test_check_refuses_overlay_mismatch(self):
        self.check_env(installed_env(APP_MODE='clearnet'), 'APP_MODE=clearnet needs compose.clearnet.yaml in COMPOSE_FILE')
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.clearnet.yaml:compose.tor.yaml'),
                       'lists both compose.clearnet.yaml and compose.tor.yaml')
        self.check_env(installed_env(APP_MODE='onion'), 'APP_MODE must be clearnet or tor')
        self.check_env(installed_env(COOKIE_SECURE='TRUE'), 'COOKIE_SECURE must be exactly true or false')

    def test_check_refuses_placeholders(self):
        self.check_env(installed_env(SETUP_TOKEN='REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS'),
                       'SETUP_TOKEN is still a placeholder value')
        self.check_env(installed_env(APP_SECRET_KEY='0123456789' * 4), 'APP_SECRET_KEY is not random')
        self.check_env(installed_env(APP_SECRET_KEY='short'), 'APP_SECRET_KEY must contain at least 32')
        self.check_env(installed_env(APP_SECRET_KEY='', SETUP_TOKEN=None), 'APP_SECRET_KEY is not set')
        self.check_env(installed_env(DATABASE_URL='postgres://opsecmkt:REPLACE_ME@db:5432/opsecmkt?sslmode=disable',
                                     POSTGRES_PASSWORD='REPLACE_ME'), 'DATABASE_URL still contains a placeholder')

    def test_check_refuses_latest_images(self):
        for image in ('ghcr.io/sethforprivacy/simple-monero-wallet-rpc:latest', 'bitcoin/bitcoin',
                      'registry.example:5000/node'):
            with self.subTest(image=image):
                self.check_env(installed_env(MONERO_WALLET_IMAGE=image), 'MONERO_WALLET_IMAGE must name a release tag')
        self.check_env(installed_env(BITCOIN_IMAGE='registry.example:5000/node@sha256:' + 'a' * 64))
        self.check_env(installed_env(BITCOIN_IMAGE='bitcoin/bitcoin:31.1'))

    def test_check_refuses_postgres_password_mismatch(self):
        self.check_env(installed_env(POSTGRES_PASSWORD=hex_value(48, 'other')), 'POSTGRES_PASSWORD differs')
        self.check_env(installed_env(DATABASE_URL='postgres://market@database.example/market'),
                       'COMPOSE_PROFILES runs the internal database (internal-db) but DATABASE_URL points elsewhere')
        self.check_env(installed_env(COMPOSE_PROFILES=''), 'COMPOSE_PROFILES lacks internal-db')
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,bitcoin'), 'needs BITCOIN_RPC_PASSWORD')

    def test_check_refuses_mirror_profile_on_clearnet(self):
        self.check_env(clearnet_env(COMPOSE_PROFILES='internal-db,mirror'), 'mirror, which needs APP_MODE=tor')

    def test_check_refuses_containers_of_another_checkout_or_project(self):
        self.write_env(installed_env())
        result = self.command('check', extra_env={'INSTALLER_TEST_CONTAINERS': '/srv/live-opsecmkt'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('/srv/live-opsecmkt', result.stderr)
        self.assertNotIn('compose config --quiet', self.calls())
        # A-135: the project name was changed under a running install.
        result = self.command('check', extra_env={'INSTALLER_TEST_CONTAINERS': '',
                                                  'INSTALLER_TEST_OTHER_CONTAINERS': f'opsecmkt-old|{self.root}'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('opsecmkt-old', result.stderr)
        self.assertIn('new, empty volumes', result.stderr)

    def test_check_warns_tor_with_secure_cookies(self):
        result = self.check_env(installed_env(COOKIE_SECURE='true'))
        self.assertIn('warning: APP_MODE=tor with COOKIE_SECURE=true', result.stderr)

    def test_commands_need_an_env(self):
        for arguments in (['check'], ['status'], ['setup-token'], ['add-mirror'], ['upgrade', '--no-backup']):
            with self.subTest(arguments=arguments):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=arguments)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('run ./scripts/install.sh init first', result.stderr)
                self.assertEqual(self.calls(), ['compose version'])

    # ---- status and setup-token -----------------------------------------------------------------------

    def test_status_reports_pending_then_complete(self):
        values = installed_env()
        self.write_env(values)
        self.set_installed(False)
        result = self.command('status')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Mode: tor (Compose project opsecmkt)', result.stdout)
        self.assertIn('app: Up 1 minute (healthy)', result.stdout)
        self.assertIn('Setup: pending', result.stdout)
        self.assertIn('Schema: 021', result.stdout)
        self.assertIn(STATUS, self.calls())
        self.assert_no_secret_printed(result, values)
        self.set_installed(True)
        result = self.command('status')
        self.assertIn('Setup: complete', result.stdout)
        self.assert_no_secret_printed(result, values)
        # The app is not running: a one-off container reads the database instead.
        result = self.command('status', extra_env={'INSTALLER_TEST_APP_DOWN': '1'})
        self.assertIn('Setup: complete', result.stdout)
        self.assertIn('compose run --rm -T --no-deps app -status', self.calls())
        result = self.command('status', extra_env={'INSTALLER_TEST_APP_DOWN': '1', 'INSTALLER_TEST_STATUS_FAIL': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Setup: unknown', result.stdout)

    def test_status_lists_all_addresses(self):
        self.write_env(installed_env(COMPOSE_PROFILES='internal-db,mirror'))
        result = self.command('status')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'  main:   {MAIN_ONION}\n', result.stdout)
        self.assertIn(f'  mirror: {MIRROR_ONION}\n', result.stdout)
        self.write_env(installed_env())
        result = self.command('status')
        self.assertIn(MAIN_ONION, result.stdout)
        self.assertNotIn('mirror', result.stdout)
        self.assertNotIn(f'compose exec -T tor-mirror {HOSTNAME}', self.calls())
        self.write_env(clearnet_env(APP_PORT='18090'))
        result = self.command('status')
        self.assertIn('App: http://127.0.0.1:18090', result.stdout)
        self.assertNotIn('.onion', result.stdout)

    def test_setup_token_prints_it_only_while_setup_is_pending(self):
        values = clearnet_env(COOKIE_SECURE='false', APP_PORT='18091')
        original = self.write_env(values)
        self.set_installed(False)
        result = self.command('setup-token')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, f"Setup token: {values['SETUP_TOKEN']}\nSetup page: http://127.0.0.1:18091/setup\n")
        self.assertEqual((self.root / '.env').read_bytes(), original)
        # A legacy install's token is also its key root: it is never printed.
        for key in (None, values['SETUP_TOKEN']):
            with self.subTest(app_secret_key=key):
                legacy = clearnet_env(APP_SECRET_KEY=key)
                self.write_env(legacy)
                result = self.command('setup-token')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('setup-token --new', result.stderr)
                self.assert_no_secret_printed(result, legacy)

    def test_setup_token_refuses_after_install(self):
        values = installed_env()
        original = self.write_env(values)
        self.set_installed(True)
        for arguments in (['setup-token'], ['setup-token', '--new']):
            with self.subTest(arguments=arguments):
                result = self.command(*arguments)
                self.assertEqual(result.returncode, 1)
                self.assertIn('Setup is complete', result.stderr)
                self.assert_no_secret_printed(result, values)
                self.assertEqual((self.root / '.env').read_bytes(), original)
                self.assertNotIn('compose up -d --wait', self.calls())

    def test_setup_token_new_rotates_only_that_key(self):
        values = installed_env()
        original = self.write_env(values)
        self.set_installed(False)
        result = self.command('setup-token', '--new')
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assertNotEqual(config['SETUP_TOKEN'], values['SETUP_TOKEN'])
        self.assertRegex(config['SETUP_TOKEN'], r'^[0-9a-f]{64}$')
        self.assertEqual((self.root / '.env').read_text(),
                         original.decode().replace(values['SETUP_TOKEN'], config['SETUP_TOKEN']))
        self.assertIn('compose up -d --wait', self.calls())
        self.assertIn(f"Setup token: {config['SETUP_TOKEN']}\n", result.stdout)
        self.assertIn(f'Setup page: http://{MAIN_ONION}/setup', result.stdout)
        self.assertEqual(result.stdout.count(config['SETUP_TOKEN']), 1)
        self.assert_no_secret_printed(result, {**values, 'SETUP_TOKEN': config['SETUP_TOKEN']}, allow_token=True)
        self.assertNotIn(values['SETUP_TOKEN'], result.stdout + result.stderr)
        # The app refuses to start without a token until setup is done; --new then works from the database.
        self.write_env(installed_env(SETUP_TOKEN=''))
        result = self.command('setup-token', '--new', extra_env={'INSTALLER_TEST_APP_DOWN': '1'})
        self.assertEqual(result.returncode, 0, result.stderr)
        # A legacy install keeps its key root: APP_SECRET_KEY takes the old token first.
        legacy = installed_env(APP_SECRET_KEY=None)
        self.write_env(legacy)
        result = self.command('setup-token', '--new')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.config()['APP_SECRET_KEY'], legacy['SETUP_TOKEN'])
        self.assertNotIn(legacy['SETUP_TOKEN'], result.stdout + result.stderr)

    # ---- add-mirror -------------------------------------------------------------------------------------

    def test_add_mirror_appends_profile_keeping_existing(self):
        for profiles, expected in (('internal-db,bitcoin,monero,monero-wallet', 'internal-db,bitcoin,monero,monero-wallet,mirror'),
                                   ('internal-db', 'internal-db,mirror'), ('', 'mirror')):
            with self.subTest(profiles=profiles):
                values = installed_env(COMPOSE_PROFILES=profiles, BITCOIN_RPC_PASSWORD=hex_value(48, 'rpc'),
                                       MONERO_WALLET_RPC_PASSWORD=hex_value(48, 'wallet'))
                if 'internal-db' not in profiles:
                    values['DATABASE_URL'] = 'postgres://market@database.example/market'
                original = self.write_env(values)
                result = self.command('add-mirror')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.config()['COMPOSE_PROFILES'], expected)
                self.assertEqual((self.root / '.env').read_text(),
                                 original.decode().replace(f"COMPOSE_PROFILES='{profiles}'", f"COMPOSE_PROFILES='{expected}'"))
                self.assertEqual(self.calls()[:4], ['compose version', *CHECK, 'compose up -d --wait'])
                self.assertFalse(any('--profile' in call for call in self.calls()))
                self.assert_no_secret_printed(result, values)

    def test_add_mirror_is_idempotent(self):
        self.write_env(installed_env())
        self.assertEqual(self.command('add-mirror').returncode, 0)
        after_first = (self.root / '.env').read_bytes()
        result = self.command('add-mirror')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), after_first)
        self.assertIn('already in COMPOSE_PROFILES', result.stdout)
        self.assertEqual(self.config()['COMPOSE_PROFILES'], 'internal-db,mirror')

    def test_add_mirror_refuses_clearnet(self):
        original = self.write_env(clearnet_env())
        result = self.command('add-mirror')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('needs a Tor install', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        self.assertEqual(self.calls(), ['compose version'])

    def test_add_mirror_waits_and_prints_both_addresses(self):
        self.write_env(installed_env())
        result = self.command('add-mirror', extra_env={'INSTALLER_TEST_TOR_MIRROR_ONION_WAITS': '5'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls().count(f'compose exec -T tor-mirror {HOSTNAME}'), 6)
        self.assertIn(f'Main onion:   {MAIN_ONION}\n', result.stdout)
        self.assertIn(f'Mirror onion: {MIRROR_ONION}\n', result.stdout)
        self.write_env(installed_env())
        result = self.command('add-mirror', extra_env={'INSTALLER_TEST_NO_ONION': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('within 90 seconds', result.stderr)

    def test_add_mirror_prints_backup_reminder(self):
        self.write_env(installed_env())
        result = self.command('add-mirror')
        self.assertIn('identity=tor_mirror_identity AGE_RECIPIENT=age1', result.stdout)
        self.assertIn('docs/operator-guide.md#secrets-env-and-onion-identities', result.stdout)
        guide = (REPOSITORY / 'docs' / 'operator-guide.md').read_text()
        self.assertIn('## Secrets, .env and onion identities', guide)
        self.assertIn('Back up an onion identity', guide)
        self.assertIn('operator-signed verification document', result.stdout)

    # ---- upgrade ----------------------------------------------------------------------------------------

    def upgrade(self, *arguments, extra_env=None):
        return self.command('upgrade', *(arguments or ('--no-backup',)), extra_env=extra_env)

    def test_upgrade_requires_backup_or_no_backup(self):
        original = self.write_env(alpha_env())
        result = self.command('upgrade')
        self.assertEqual(result.returncode, 1)
        self.assertIn('AGE_RECIPIENT', result.stderr)
        self.assertIn('--no-backup', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        self.assertEqual(self.calls(), ['compose version'])
        self.assertEqual(self.pre_upgrade_copies(), [])
        # With a recipient, a backup of this project is taken before anything changes.
        result = self.command('upgrade', extra_env={'AGE_RECIPIENT': 'age1testrecipient'})
        self.assertEqual(result.returncode, 0, result.stderr)
        backup = Path(str(self.log) + '.backup').read_text()
        self.assertRegex(backup, r'^backups/pre-upgrade-\d{8}T\d{6}Z\.dump\.age project=opsecmkt recipient=age1testrecipient\n$')
        # A failed backup stops the upgrade before .env changes.
        self.write_env(alpha_env())
        for copy in self.pre_upgrade_copies():
            copy.unlink()
        self.backup_stub(code=1)
        result = self.command('upgrade', extra_env={'AGE_RECIPIENT': 'age1testrecipient'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('backup failed', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        self.assertEqual(self.pre_upgrade_copies(), [])

    def test_upgrade_writes_pre_upgrade_copy_0600(self):
        original = self.write_env(alpha_env(), mode=0o644)
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        copies = self.pre_upgrade_copies()
        self.assertEqual(len(copies), 1)
        self.assertRegex(copies[0].name, r'^\.env\.pre-upgrade-\d{8}T\d{6}Z$')
        self.assertEqual(copies[0].read_bytes(), original)
        self.assertEqual(stat.S_IMODE(copies[0].stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE((self.root / '.env').stat().st_mode), 0o600)
        self.assertIn(copies[0].name, result.stdout)

    def test_upgrade_adds_missing_keys_with_defaults(self):
        values = alpha_env()
        self.write_env(values)
        result = self.upgrade(extra_env={'APP_PORT': '18092'})
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        example = example_values()
        for key in ('PAYMENT_EXPIRY', 'PAYMENT_POLL_INTERVAL', 'PAYMENT_CONFIRMATIONS_BTC', 'BITCOIN_WALLET',
                    'MIGRATION_TIMEOUT', 'BITCOIN_IMAGE', 'MONERO_WALLET_IMAGE', 'BITCOIN_PRUNE_MB', 'MONERO_PRUNE_FLAGS'):
            self.assertEqual(config[key], example[key], key)
        self.assertEqual(config['APP_PORT'], '18092')  # the port the caller runs it on
        self.assertEqual(config['COMPOSE_PROJECT_NAME'], 'opsecmkt')
        # Existing values are never changed (only :latest images are pinned).
        for key in ('DATABASE_URL', 'POSTGRES_PASSWORD', 'APP_MODE', 'COOKIE_SECURE', 'COMPOSE_FILE', 'MONERO_RPC_URL'):
            self.assertEqual(config[key], values[key], key)
        self.assertNotIn('AUDIT_SIGNING_KEY', config)  # secrets are not invented
        self.assertIn('AUDIT_SIGNING_KEY is not set', result.stdout)
        self.assertIn('added PAYMENT_EXPIRY (default)', result.stdout)
        self.assert_no_secret_printed(result, values)
        self.assertEqual(self.calls()[-3:], [UP, STATUS, 'compose up -d --wait --force-recreate tor'])

    def test_upgrade_defaults_cover_env_example_keys(self):
        # Every non-secret key .env.example documents is written, with the value .env.example shows.
        password = hex_value(48, 'minimal')
        self.write_env({'DATABASE_URL': f'postgres://opsecmkt:{password}@db:5432/opsecmkt?sslmode=disable',
                        'POSTGRES_PASSWORD': password, 'SETUP_TOKEN': hex_value(64, 'minimal-token')})
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        example = example_values()
        for key, value in example.items():
            if key not in SECRET_KEYS:
                self.assertIn(key, config, f'upgrade does not add {key}')
                self.assertEqual(config[key], value, key)
        self.assertEqual(sorted(set(config) - set(example)), [])

    def test_upgrade_copies_setup_token_to_app_secret_key(self):
        values = alpha_env()
        self.write_env(values)
        self.set_installed(False)
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assertEqual(config['APP_SECRET_KEY'], values['SETUP_TOKEN'])
        self.assertEqual(config['SETUP_TOKEN'], values['SETUP_TOKEN'])  # setup still pending
        self.assertIn('set APP_SECRET_KEY to the current SETUP_TOKEN value', result.stdout)
        self.assert_no_secret_printed(result, values)
        # An existing APP_SECRET_KEY is never replaced.
        values = installed_env()
        self.write_env(values)
        self.upgrade()
        self.assertEqual(self.config()['APP_SECRET_KEY'], values['APP_SECRET_KEY'])

    def test_upgrade_strips_setup_token_only_when_installed(self):
        values = alpha_env()
        self.write_env(values)
        self.set_installed(True)
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assertNotIn('SETUP_TOKEN', config)
        self.assertEqual(config['APP_SECRET_KEY'], values['SETUP_TOKEN'])
        # The app is recreated without it, then Tor once.
        self.assertEqual(self.calls()[-4:], [UP, STATUS, 'compose up -d --wait', 'compose up -d --wait --force-recreate tor'])
        self.assert_no_secret_printed(result, values)
        # Setup pending, or the state unreadable: the token stays.
        for installed, extra in ((False, {}), (True, {'INSTALLER_TEST_APP_DOWN': '1', 'INSTALLER_TEST_STATUS_FAIL': '1'})):
            with self.subTest(installed=installed, extra=extra):
                self.write_env(alpha_env())
                self.set_installed(installed)
                self.assertEqual(self.upgrade(extra_env=extra).returncode, 0)
                self.assertEqual(self.config()['SETUP_TOKEN'], values['SETUP_TOKEN'])

    def test_upgrade_rewrites_latest_images(self):
        values = alpha_env(BITCOIN_IMAGE='bitcoin/bitcoin:latest',
                           MONERO_WALLET_IMAGE='ghcr.io/sethforprivacy/simple-monero-wallet-rpc:latest')
        self.write_env(values)
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        defaults = self.compose_image_defaults()
        for key in defaults:
            self.assertEqual(config[key], defaults[key], key)
        self.assertIn(f"pinned MONERO_WALLET_IMAGE to {defaults['MONERO_WALLET_IMAGE']}", result.stdout)
        # A reviewed image of another publisher is kept as it is; on :latest, check refuses it.
        self.write_env(alpha_env(BITCOIN_IMAGE='registry.example/bitcoin:26.0'))
        self.assertEqual(self.upgrade().returncode, 0)
        self.assertEqual(self.config()['BITCOIN_IMAGE'], 'registry.example/bitcoin:26.0')
        self.write_env(alpha_env(BITCOIN_IMAGE='registry.example/bitcoin:latest'))
        result = self.upgrade()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('BITCOIN_IMAGE must name a release tag', result.stderr)
        self.assertNotIn(UP, self.calls())

    def test_upgrade_syncs_mirror_profile(self):
        self.write_env(alpha_env())
        result = self.upgrade(extra_env={'INSTALLER_TEST_MIRROR_CONTAINER': '1'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.config()['COMPOSE_PROFILES'], 'internal-db,monero,mirror')
        self.assertIn('added mirror to COMPOSE_PROFILES', result.stdout)
        # No mirror container: no mirror.
        self.write_env(alpha_env())
        self.upgrade()
        self.assertEqual(self.config()['COMPOSE_PROFILES'], 'internal-db,monero')

    def test_upgrade_recreates_tor_services(self):
        self.write_env(alpha_env(COMPOSE_PROFILES='internal-db,monero,mirror'))
        self.assertEqual(self.upgrade().returncode, 0)
        self.assertEqual(self.calls()[-1], 'compose up -d --wait --force-recreate tor tor-mirror')
        self.write_env(alpha_env())
        self.assertEqual(self.upgrade().returncode, 0)
        self.assertEqual(self.calls()[-1], 'compose up -d --wait --force-recreate tor')
        # Clearnet has no Tor to recreate.
        self.write_env(alpha_env(APP_MODE='clearnet', COMPOSE_FILE='compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml'))
        self.assertEqual(self.upgrade().returncode, 0)
        self.assertFalse(any('--force-recreate' in call for call in self.calls()))

    def test_upgrade_second_run_changes_nothing(self):
        self.write_env(alpha_env())
        self.set_installed(True)
        self.assertEqual(self.upgrade().returncode, 0)
        after_first = (self.root / '.env').read_bytes()
        copies = self.pre_upgrade_copies()
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), after_first)
        self.assertEqual(self.pre_upgrade_copies(), copies)
        self.assertIn('.env is already up to date.', result.stdout)
        mirror = ('ps -a -q --filter label=com.docker.compose.project=opsecmkt '
                  '--filter label=com.docker.compose.service=tor-mirror')
        self.assertEqual(self.calls(), ['compose version', mirror, *CHECK, UP, STATUS])

    def test_upgrade_takes_the_project_name_from_this_directorys_containers(self):
        self.write_env(alpha_env())
        result = self.upgrade(extra_env={'INSTALLER_TEST_PROJECT': 'market-live'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.config()['COMPOSE_PROJECT_NAME'], 'market-live')
        self.write_env(alpha_env())
        result = self.upgrade(extra_env={'INSTALLER_TEST_CONTAINERS': '',
                                         'INSTALLER_TEST_OTHER_CONTAINERS': f'one|{self.root}\ntwo|{self.root}'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('more than one Compose project', result.stderr)
        self.assertEqual(self.pre_upgrade_copies()[1:], [])

    # ---- invariants -------------------------------------------------------------------------------------

    def test_installer_makes_no_outbound_fetch(self):
        text = INSTALLER.read_text()
        for word in ('curl', 'wget', '/dev/tcp', '/dev/udp', 'docker pull', 'compose pull'):
            self.assertNotIn(word, text)
        # No command runs git, nc or ssh (messages may mention `git pull` for the operator to run).
        for line in text.splitlines():
            self.assertNotRegex(line.split(' #')[0], r'''(^|[;&|(`]|\$\(|then|do)\s*(git|nc|ssh|scp)\s''', line)
        self.assertEqual(self.run_installer([''] * 11).returncode, 0)
        self.assertFalse(Path(str(self.log) + '.fetch').exists())

    def test_isolated_keys_cover_env_example(self):
        # Commands unset these so Compose reads .env alone; .env.example documents every key the app reads.
        keys = re.search(r"^env_keys='([^']*)'", INSTALLER.read_text(), re.M)[1].split()
        self.assertEqual(sorted(keys), sorted(example_values()))


if __name__ == '__main__':
    unittest.main()
