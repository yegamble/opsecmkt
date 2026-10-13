"""Exercise installer decisions in disposable directories with no real Docker."""
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import resource
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
# The one-off -status container: init reads an external database with it before starting anything.
PROBE = 'compose run --rm -T --no-deps app -status'
# Keys whose values are secrets: upgrade never invents them and no command prints them.
SECRET_KEYS = ('DATABASE_URL', 'POSTGRES_PASSWORD', 'APP_SECRET_KEY', 'SETUP_TOKEN', 'AUDIT_SIGNING_KEY',
               'BITCOIN_RPC_PASSWORD', 'BITCOIN_RPC_AUTH', 'MONERO_WALLET_SECRET', 'ESCROW_XMR_WALLET_RPC_PASSWORD')
PASSWORD = 'correct horse battery staple'
# The malware scanner (A-184): what clamdscan --version prints once clamd answers, and the calls it takes.
CLAMD_VERSION = 'ClamAV 1.5.4/28130/Mon Sep 21 06:24:29 2026'
CLAMD_READ = 'compose exec -T clamd clamdscan --version'
CLAMD_CONFIG_READ = 'compose exec -T clamd cat /etc/clamav/clamd.conf'
MEMORY = 'info --format {{.MemTotal}}'
# A-523: before writing .env for local nodes, init reads Docker's data directory and its free space (warn only).
DOCKER_ROOT = 'info --format {{.DockerRootDir}}'
CHECK_SCANNER = [*CHECK, 'compose config']
# With the scanner on, `up --wait` names every service but clamd, which is then started without waiting: its
# health never decides whether a command succeeds (ruling C1).
SERVICES = 'compose config --services'
START_CLAMD = 'compose up -d clamd'
UP_SCANNER = [SERVICES, 'compose up -d --build --wait app db tor', START_CLAMD]
UP_WAIT_SCANNER = [SERVICES, 'compose up -d --wait app db tor', START_CLAMD]
SCANNER_FILES = 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml:compose.scanner.yaml'
# `docker compose config` for a Tor install with the scanner, as Compose prints it (abridged to what the
# installer reads). ScannerConfigTests.test_check_reads_real_compose_output checks the parser on the real thing.
SCANNER_CONFIG = '''name: opsecmkt
services:
  app:
    build:
      context: /srv/opsecmkt
      dockerfile: Dockerfile
    environment:
      CLAMD_ADDRESS: clamd:3310
    networks:
      backend: null
      scanner: null
  clamd:
    profiles:
      - scanner
    entrypoint:
      - clamd
    image: clamav/clamav:1.5.4-debian
    networks:
      scanner: null
    read_only: true
    user: clamav
    volumes:
      - type: bind
        source: /srv/opsecmkt/deploy/clamd.conf
        target: /etc/clamav/clamd.conf
        read_only: true
        bind: {}
  db:
    image: postgres:17-alpine
    networks:
      backend: null
    volumes:
      - type: volume
        source: postgres_data
        target: /var/lib/postgresql/data
        volume: {}
  tor:
    networks:
      backend: null
      tor_egress: null
networks:
  backend:
    name: opsecmkt_backend
    internal: true
  scanner:
    name: opsecmkt_scanner
    internal: true
  tor_egress:
    name: opsecmkt_tor_egress
volumes:
  postgres_data:
    name: opsecmkt_postgres_data
x-logging:
  driver: json-file
  options:
    max-file: "3"
    max-size: 10m
'''
# Stubs write logs; a test that limits the installer's file size must not limit them.
LIFT_FILE_SIZE_LIMIT = '''
import resource
resource.setrlimit(resource.RLIMIT_FSIZE, (resource.getrlimit(resource.RLIMIT_FSIZE)[1],) * 2)
'''


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


def bitcoin_rpcauth(password, salt='0123456789abcdef0123456789abcdef'):
    digest = hmac.new(salt.encode(), password.encode(), hashlib.sha256).hexdigest()
    return f'marketplace:{salt}${digest}'


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
        'BITCOIN_RPC_URL': '',
        'BITCOIN_WALLET': '',
        'BITCOIN_CHAIN': '',
        'MONERO_RPC_URL': '',
        'MONERO_WALLET_RPC_URL': '',
        'MONERO_NETWORK': '',
        'COMPOSE_FILE': 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml',
        'COMPOSE_PROFILES': 'internal-db',
    }
    values.update(overrides)
    if ('bitcoin' in (values.get('COMPOSE_PROFILES') or '').split(',') and values.get('BITCOIN_RPC_PASSWORD')
            and 'BITCOIN_RPC_AUTH' not in overrides):
        values['BITCOIN_RPC_AUTH'] = bitcoin_rpcauth(values['BITCOIN_RPC_PASSWORD'])
    return {key: value for key, value in values.items() if value is not None}


def clearnet_env(**overrides):
    return installed_env(**{'APP_MODE': 'clearnet', 'COOKIE_SECURE': 'true',
                            'COMPOSE_FILE': 'compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml',
                            **overrides})


def scanner_env(**overrides):
    """installed_env with the malware scanner, as add-scanner leaves it."""
    return installed_env(**{'COMPOSE_FILE': SCANNER_FILES, 'COMPOSE_PROFILES': 'internal-db,scanner',
                            'CLAMD_ADDRESS': 'clamd:3310', 'CLAMAV_IMAGE': 'clamav/clamav:1.5.4-debian', **overrides})


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
        'COMPOSE_FILE': 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml',
        'COMPOSE_PROFILES': 'internal-db,monero',
    }
    values.update(overrides)
    return {key: value for key, value in values.items() if value is not None}


def installer_image_defaults():
    """The node images the installer writes, as bash evaluates its assignments (a tag, then its digest)."""
    lines = [line for line in INSTALLER.read_text().splitlines()
             if re.match(r'^(bitcoin|monero|monero_wallet)_image_default\+?=', line)]
    script = '\n'.join(lines) + '\nprintf "%s\\n" "$bitcoin_image_default" "$monero_image_default" "$monero_wallet_image_default"\n'
    values = subprocess.run(['/bin/bash', '-c', script], capture_output=True, text=True, check=True).stdout.split()
    return dict(zip(('BITCOIN_IMAGE', 'MONERO_IMAGE', 'MONERO_WALLET_IMAGE'), values))


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
        # check reads clamd's configuration file, which clamd reads as its own user (A-184).
        (self.root / 'deploy').mkdir()
        shutil.copyfile(REPOSITORY / 'deploy' / 'clamd.conf', self.root / 'deploy' / 'clamd.conf')
        (self.root / 'deploy' / 'clamd.conf').chmod(0o644)
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
import re
import sys
''' + LIFT_FILE_SIZE_LIMIT + '''
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
def keyroot():
    # The key root the app reports: the one its environment gives it. A one-off container reads .env; the
    # running app may have been started with another .env (INSTALLER_TEST_KEYROOT; 'omit' = an older app).
    value = os.environ.get('INSTALLER_TEST_KEYROOT')
    if value is None:
        key = ''
        for line in Path('.env').read_text().splitlines():
            match = re.match(r"\\s*(export\\s+)?APP_SECRET_KEY\\s*=\\s*'?([^'\\s]*)", line)
            if match:
                key = match[2]
        value = 'app_secret_key' if key else 'setup_token'
    return '' if value == 'omit' else ' keyroot=' + value
def print_status():
    if flag('INSTALLER_TEST_STATUS_FAIL'):
        sys.exit('status: database connection failed')
    print(f'installed={installed()} schema=021{keyroot()}')
    sys.exit(0)
if args == ['compose', 'version']:
    sys.exit(0)
if args == ['compose', 'config', '--quiet']:
    assert Path('.env').is_file(), 'Configuration must exist before validation'
    sys.exit(42 if flag('INSTALLER_TEST_BAD_CONFIG') else 0)
if args[:2] == ['compose', 'up']:
    if args == ['compose', 'up', '-d', '--force-recreate', 'clamd'] and flag('INSTALLER_TEST_CLAMD_RECREATE_FAIL'):
        sys.exit('Error response from daemon: clamd recreate failed')
    # A clamd that never becomes healthy fails `up --wait` whenever that wait includes it: named, or no service
    # named (every service of the active profiles).
    named = [arg for arg in args[2:] if not arg.startswith('-')]
    if flag('INSTALLER_TEST_CLAMD_UNHEALTHY') and '--wait' in args and (not named or 'clamd' in named):
        sys.exit('dependency failed to start: container clamd is unhealthy')
    sys.exit(1 if flag('INSTALLER_TEST_UNHEALTHY') else 0)
if args == ['compose', 'config', '--services']:
    print(os.environ.get('INSTALLER_TEST_SERVICES', 'app\\ndb\\ntor\\nclamd'))
    sys.exit(0)
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
if args == ['compose', 'port', 'app', '8080']:
    published = os.environ.get('INSTALLER_TEST_APP_PUBLISHED')
    if not published:
        sys.exit('no container found for app')
    print(published)
    sys.exit(0)
# The malware scanner: the host's memory, the resolved Compose configuration, pulls and clamd's VERSION.
if args == ['info', '--format', '{{.DockerRootDir}}']:
    # Read before .env is written (A-523): the mainnet notice and the disk check come first.
    assert not Path('.env').exists(), '.env was written before the disk check'
    if flag('INSTALLER_TEST_DOCKER_ROOT_FAIL'):
        sys.exit('Cannot connect to the Docker daemon')
    print(os.environ.get('INSTALLER_TEST_DOCKER_ROOT', os.getcwd()))
    sys.exit(0)
if args == ['info', '--format', '{{.MemTotal}}']:
    if flag('INSTALLER_TEST_INFO_FAIL'):
        sys.exit('Cannot connect to the Docker daemon')
    print(os.environ.get('INSTALLER_TEST_MEMTOTAL', str(8 * 2**30)))
    sys.exit(0)
if args == ['compose', 'config']:
    assert Path('.env').is_file(), 'Configuration must exist before validation'
    print(os.environ['INSTALLER_TEST_COMPOSE_CONFIG'], end='')
    sys.exit(0)
if args == ['compose', 'pull', 'clamd']:
    if flag('INSTALLER_TEST_PULL_FAIL'):
        sys.exit('Error response from daemon: Get "https://registry-1.docker.io/v2/": dial tcp: i/o timeout')
    if os.environ.get('INSTALLER_TEST_PULLED_VERSION'):
        (state / 'clamd-version').write_text(os.environ['INSTALLER_TEST_PULLED_VERSION'])
    sys.exit(0)
if args == ['compose', 'exec', '-T', 'clamd', 'clamdscan', '--version']:
    if flag('INSTALLER_TEST_CLAMD_STOPPED'):
        sys.exit('service "clamd" is not running')
    counter = state / 'clamd-reads'
    reads = int(counter.read_text()) + 1 if counter.exists() else 1
    counter.write_text(str(reads))
    # clamdscan prints only its own version, and exits 0, while clamd does not answer.
    if flag('INSTALLER_TEST_CLAMD_DOWN') or reads <= int(os.environ.get('INSTALLER_TEST_CLAMD_WAITS', '0')):
        print('ClamAV 1.5.4')
        print('ERROR: Could not connect to clamd on 0.0.0.0: Connection refused', file=sys.stderr)
        sys.exit(0)
    version = state / 'clamd-version'
    print(version.read_text() if version.exists() else os.environ['INSTALLER_TEST_CLAMD_VERSION'])
    sys.exit(0)
if args == ['compose', 'exec', '-T', 'clamd', 'cat', '/etc/clamav/clamd.conf']:
    if flag('INSTALLER_TEST_CLAMD_CONFIG_READ_FAIL'):
        sys.exit('service "clamd" is not running')
    config = os.environ.get('INSTALLER_TEST_CLAMD_CONFIG')
    sys.stdout.write(config if config is not None else Path('deploy/clamd.conf').read_text())
    sys.exit(0)
sys.exit('Unexpected docker command')
''')
        self.stub('openssl', '''
import hashlib
import hmac
import os
from pathlib import Path
import sys
''' + LIFT_FILE_SIZE_LIMIT + '''
if sys.argv[1:3] == ['dgst', '-sha256']:
    assert len(sys.argv) == 5 and sys.argv[3] == '-hmac', sys.argv
    if os.environ.get('INSTALLER_TEST_OPENSSL_BAD_DGST') == 'fail':
        sys.exit(1)
    if os.environ.get('INSTALLER_TEST_OPENSSL_BAD_DGST') == 'malformed':
        print('SHA2-256(stdin)= not-a-hash')
        sys.exit(0)
    print('SHA2-256(stdin)= ' + hmac.new(sys.argv[4].encode(), sys.stdin.buffer.read(), hashlib.sha256).hexdigest())
    sys.exit(0)
assert sys.argv[1:3] == ['rand', '-hex'], sys.argv
counter = Path(os.environ['INSTALLER_TEST_LOG'] + '.openssl')
call = int(counter.read_text()) + 1 if counter.exists() else 1
counter.write_text(str(call))
if str(call) == os.environ.get('INSTALLER_TEST_OPENSSL_EMPTY_CALL'):
    sys.exit(1)  # generation failure: no output
if sys.argv[3] == '16' and os.environ.get('INSTALLER_TEST_OPENSSL_BAD_SALT'):
    bad = os.environ['INSTALLER_TEST_OPENSSL_BAD_SALT']
    if bad == 'fail':
        sys.exit(1)
    print('' if bad == 'empty' else 'not-hex')
    sys.exit(0)
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
        # Free space under Docker's data directory, as POSIX `df -Pk` prints it (KiB): plenty unless a test says.
        self.stub('df', '''
[ "$1" = -Pk ] || exit 64
[ -z "$INSTALLER_TEST_DF_FAIL" ] || exit 1
echo 'Filesystem 1024-blocks Used Available Capacity Mounted on'
echo "/dev/test 4000000000 1000 ${INSTALLER_TEST_DF_KB:-3000000000} 1% /"
''', interpreter='/bin/sh')
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

    def run_installer(self, answers, bad_config=False, openssl_empty_call=None, arguments=None, extra_env=None, timeout=30,
                      file_size_limit=None):
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
            'INSTALLER_TEST_COMPOSE_CONFIG': SCANNER_CONFIG,
            'INSTALLER_TEST_CLAMD_VERSION': CLAMD_VERSION,
            # macOS's Python can redirect bytecode caches beneath the temporary
            # HOME. Stubs must not keep populating it while cleanup removes it.
            'PYTHONDONTWRITEBYTECODE': '1',
            **(extra_env or {}),
        }
        Path(str(self.log) + '.openssl').unlink(missing_ok=True)

        def limit_file_size():
            # A full disk or quota, as the kernel reports it: writes past the limit fail with EFBIG (the
            # signal ignored). Only the soft limit is lowered, so the stubs can lift it for their own logs.
            signal.signal(signal.SIGXFSZ, signal.SIG_IGN)
            resource.setrlimit(resource.RLIMIT_FSIZE, (file_size_limit, resource.getrlimit(resource.RLIMIT_FSIZE)[1]))
        process = subprocess.Popen(
            ['/bin/bash', str(self.root / 'scripts' / 'install.sh'), *(arguments or [])],
            cwd=self.root, env=env, stdin=subprocess.PIPE,
            # surrogateescape: an answer can carry bytes that are not UTF-8 ('\udce9' is the byte 0xe9).
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, encoding='utf-8', errors='surrogateescape',
            start_new_session=True, preexec_fn=limit_file_size if file_size_limit else None,
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

    def command(self, *arguments, extra_env=None, answers=None, file_size_limit=None, timeout=30):
        """Run a subcommand on an existing .env, with this checkout's containers present."""
        self.log.unlink(missing_ok=True)
        env = {'INSTALLER_TEST_CONTAINERS': str(self.root), **(extra_env or {})}
        return self.run_installer(answers, arguments=list(arguments), extra_env=env, file_size_limit=file_size_limit,
                                  timeout=timeout)

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
        # After the token come the two reminders and, with a local Bitcoin node, the escrow set-up commands (A-495, A-513).
        self.assertGreaterEqual(index, len(lines) - 40)

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
        try:
            state = subprocess.run(
                ['/bin/ps', '-o', 'stat=', '-p', str(child)],
                capture_output=True, text=True, timeout=5, check=False,
            ).stdout.strip()
        except PermissionError:
            self.skipTest('sandbox does not allow inspecting child process state')
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
        config = self.config()
        onion = [f'compose exec -T tor {HOSTNAME}'] if config['APP_MODE'] == 'tor' else []
        probe = [] if re.search(r'@db[:/]', config['DATABASE_URL']) else [PROBE]
        self.assertEqual(self.calls(), [*preflight_calls, *self.disk_calls(config), *CHECK, *probe, UP, *onion])
        self.assertEqual(list(self.root.glob('.env.install.*')), [])
        self.assertEqual(list(self.root.glob('.env.edit.*')), [])
        self.assertFalse((self.root / '.env.lock').exists())

    @staticmethod
    def disk_calls(config):
        """A-523: a local node on a chain with a size estimate (any but regtest) has its disk checked before .env."""
        profiles = config.get('COMPOSE_PROFILES', '').split(',')
        bitcoin = 'bitcoin' in profiles and config.get('BITCOIN_CHAIN') != 'regtest'
        return [DOCKER_ROOT] if bitcoin or 'monero' in profiles else []

    def set_clamav_default(self, image):
        script = self.root / 'scripts' / 'install.sh'
        source = script.read_text()
        current = "clamav_image_default='clamav/clamav:1.5.4-debian'"
        self.assertIn(current, source)
        script.write_text(source.replace(current, f"clamav_image_default='{image}'", 1))

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
        # choice, image, network and pruning, then Monero escrow. The defaults are pruned local MAINNET nodes (A-523):
        # written explicitly, never left to a blank (whose Compose fallback stays a test network).
        result = self.run_installer([''] * 12)
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['APP_MODE'], 'clearnet')
        self.assertEqual(config['COOKIE_SECURE'], 'true')
        # Existing installs have no COMPOSE_PROJECT_NAME and use compose.yaml's name; new ones keep that default.
        self.assertEqual(config['COMPOSE_PROJECT_NAME'], 'opsecmkt')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero')
        self.assertEqual(config['COMPOSE_FILE'],
                         'compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml')
        self.assertIn('@db:5432/opsecmkt', config['DATABASE_URL'])
        self.assertEqual(len(config['POSTGRES_PASSWORD']), 48)
        self.assertEqual(len(config['SETUP_TOKEN']), 64)
        self.assertEqual(len(config['AUDIT_SIGNING_KEY']), 64)
        self.assert_token_only_in_final_block(result, config['SETUP_TOKEN'])
        self.assert_no_secret_printed(result, config, allow_token=True)
        self.assertEqual(config['BITCOIN_CHAIN'], 'main')
        self.assertEqual(config['MONERO_NETWORK'], 'mainnet')
        self.assertEqual(config['MONERO_NETWORK_FLAG'], '')
        images = installer_image_defaults()
        self.assertEqual(config['BITCOIN_IMAGE'], images['BITCOIN_IMAGE'])
        self.assertEqual(config['MONERO_IMAGE'], images['MONERO_IMAGE'])
        self.assertEqual(config['MONERO_WALLET_IMAGE'], images['MONERO_WALLET_IMAGE'])
        # Pruned by default, written explicitly so the operator can see and change it in .env: on mainnet 15000 MiB
        # (about two months of blocks, an estimate), with six confirmations.
        self.assertEqual(config['BITCOIN_PRUNE_MB'], '15000')
        self.assertEqual(config['PAYMENT_CONFIRMATIONS_BTC'], '6')
        self.assertEqual(config['MONERO_PRUNE_FLAGS'], PRUNED_MONERO_FLAGS)
        self.assertIn('MAINNET', result.stdout)
        self.assertNotIn('ESCROW_XMR', config)
        self.assertEqual(config['CLAMAV_IMAGE_DEFAULT'], 'clamav/clamav:1.5.4-debian')
        self.assertEqual(config['BITCOIN_RPC_URL'], '')
        self.assertEqual(config['BITCOIN_WALLET'], '')
        self.assertRegex(config['BITCOIN_RPC_AUTH'], r'^marketplace:[0-9a-f]{32}\$[0-9a-f]{64}$')
        self.assertEqual(config['BITCOIN_RPC_AUTH'], bitcoin_rpcauth(
            config['BITCOIN_RPC_PASSWORD'], config['BITCOIN_RPC_AUTH'].split(':', 1)[1].split('$', 1)[0]))
        self.assertEqual(config['MONERO_RPC_URL'], '')
        self.assertEqual(config['MONERO_WALLET_RPC_URL'], '')
        self.assertNotIn('MONERO_WALLET_RPC_PASSWORD', config)
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])
        # The node question states the disk and first-sync cost before the operator answers.
        self.assertIn('pruned Bitcoin Core node', result.stdout)
        self.assertIn('pruned Monero daemon', result.stdout)
        self.assertIn('first sync', result.stdout)
        self.assertNotIn('Full node', result.stdout)
        self.assertNotIn('docs/testnet-runbook.md', result.stdout)
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

    def test_node_images_default_to_supported_release_tags(self):
        # Keep Compose, installer defaults and the example aligned; floating tags can change node behavior.
        defaults = self.compose_image_defaults()
        tags = {}
        for variable, image in defaults.items():
            tag = image.partition('@')[0].rpartition('/')[2].partition(':')[2]  # a digest may follow the tag
            self.assertNotIn(tag, ('', 'latest'), f'{variable} defaults to {image!r}, not a pinned release tag')
            tags[variable] = tag
        self.assertEqual(tags['MONERO_IMAGE'], tags['MONERO_WALLET_IMAGE'])
        # The installer and the example carry the same release, with its digest beside the tag (A-523).
        installer = installer_image_defaults()
        self.assertEqual({key: image.partition('@')[0] for key, image in installer.items()}, defaults)
        example = dict(line.split('=', 1) for line in (REPOSITORY / '.env.example').read_text().splitlines()
                       if line.split('=', 1)[0] in defaults)
        self.assertEqual(example, installer)

    def test_node_images_pinned_by_digest(self):
        # A-523: each default node image is its reviewed release tag and that tag's multi-arch index digest, as
        # `docker buildx imagetools inspect <tag>` printed it; check accepts the form and upgrade pins to it.
        compose = self.compose_image_defaults()
        installer = installer_image_defaults()
        for key, image in installer.items():
            with self.subTest(key=key):
                self.assertRegex(image, '^' + re.escape(compose[key]) + r'@sha256:[0-9a-f]{64}$')
        self.assertIn('docker buildx imagetools inspect', INSTALLER.read_text())
        result = self.run_installer(None, arguments=['--tor', '--nodes', 'local'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual({key: config[key] for key in installer}, installer)
        self.check_env(installed_env(**installer))

    def test_internal_clearnet_without_nodes(self):
        result = self.run_installer(['', '', '', 'disabled', 'disabled'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        # No node chosen: nothing forces a chain, so a node added later may use any test network.
        self.assertEqual(config['BITCOIN_CHAIN'], '')
        self.assertEqual(config['MONERO_NETWORK'], '')
        self.assertEqual(config['BITCOIN_RPC_URL'], '')
        self.assertEqual(config['BITCOIN_WALLET'], '')
        self.assertEqual(config['MONERO_RPC_URL'], '')
        self.assertEqual(config['MONERO_WALLET_RPC_URL'], '')
        self.assertNotIn('BITCOIN_PRUNE_MB', config)
        self.assertNotIn('MONERO_PRUNE_FLAGS', config)
        self.assertNotIn('docs/testnet-runbook.md', result.stdout)

    def test_local_nodes_can_opt_out_of_pruning(self):
        # On either class of network: the full-node notice names the chain it sizes (A-523).
        for chain, network, size in (('signet', 'stagenet', '28 GB on signet'), ('main', 'mainnet', '870 GB on mainnet')):
            with self.subTest(chain=chain):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                result = self.run_installer(['tor', '', 'local', '', chain, 'no', 'local', '', network, 'no', ''])
                self.assert_started(result)
                config = self.config()
                self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero')
                # -prune=0 disables pruning in bitcoind; a blank flag list drops --prune-blockchain for monerod.
                self.assertEqual(config['BITCOIN_PRUNE_MB'], '0')
                self.assertEqual(config['MONERO_PRUNE_FLAGS'], '')
                self.assertEqual(config['BITCOIN_CHAIN'], chain)
                self.assertEqual(config['MONERO_NETWORK'], network)
                self.assertEqual(result.stdout.count('Full node'), 2)
                self.assertIn(size, result.stdout)
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
        # The default network is mainnet, pruned to 15000 MiB; a test chain keeps Compose's 2000 MiB default.
        for chain, network, prune in (('', '', '15000'), ('signet', 'stagenet', self.compose_bitcoin_prune_default())):
            with self.subTest(chain=chain):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                result = self.run_installer(['tor', '', 'local', '', chain, 'yes', 'local', '', network, 'yes', ''])
                self.assert_started(result)
                config = self.config()
                self.assertEqual(config['BITCOIN_PRUNE_MB'], prune)
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
        self.assertIn('app direct network egress is enabled', result.stdout)

    def test_tor_profiles_match_the_mirror_command_check(self):
        # A-140: scripts/test-mirror-commands.sh (run by CI) checks the guide's onion-mirror commands against .env
        # files holding COMPOSE_FILE and COMPOSE_PROFILES exactly as the installer writes them for Tor. If either
        # side drifts, this fails.
        script = (REPOSITORY / 'scripts' / 'test-mirror-commands.sh').read_text()
        external = 'postgres://test@database/scratch'
        cases = [
            ('internal', ['tor', ''] + [''] * 9, 'internal-db,bitcoin,monero'),
            ('internal', ['tor', '', 'disabled', 'disabled'], 'internal-db'),
            ('external', ['tor', external] + [''] * 9, 'bitcoin,monero'),
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
        self.assertIn("COMPOSE_PROFILES='internal-db,bitcoin,monero,mirror'", guide)
        self.assertIn('./scripts/install.sh add-mirror', guide)

    def test_legacy_external_wallet_options_are_rejected_before_writes(self):
        cases = [
            ['--tor', '--bitcoin', 'external', '--bitcoin-rpc-url', 'https://rpc.example.test'],
            ['--tor', '--monero', 'external', '--monero-rpc-url', 'https://monerod.example.test'],
            ['--tor', '--monero-wallet-rpc-url', 'https://wallet.example.test'],
        ]
        for arguments in cases:
            with self.subTest(arguments=arguments):
                result = self.run_installer(None, arguments=arguments)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(self.calls(), [])

    def test_local_node_profiles_and_images(self):
        result = self.run_installer([
            'tor', '', 'local', 'reviewed-bitcoin:test', 'testnet4', 'yes',
            'local', 'reviewed-monero:test', 'stagenet', 'yes', '',
        ])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero')
        self.assertEqual(config['BITCOIN_IMAGE'], 'reviewed-bitcoin:test')
        self.assertEqual(config['MONERO_IMAGE'], 'reviewed-monero:test')
        self.assertEqual(config['MONERO_WALLET_IMAGE'], installer_image_defaults()['MONERO_WALLET_IMAGE'])
        self.assertEqual(len(config['BITCOIN_RPC_PASSWORD']), 48)
        self.assertEqual(config['BITCOIN_RPC_URL'], '')
        self.assertRegex(config['BITCOIN_RPC_AUTH'], r'^marketplace:[0-9a-f]{32}\$[0-9a-f]{64}$')
        self.assertEqual(config['BITCOIN_RPC_AUTH'], bitcoin_rpcauth(
            config['BITCOIN_RPC_PASSWORD'], config['BITCOIN_RPC_AUTH'].split(':', 1)[1].split('$', 1)[0]))
        self.assertEqual(config['BITCOIN_CHAIN'], 'testnet4')
        self.assertEqual(config['MONERO_NETWORK'], 'stagenet')
        self.assertEqual(config['MONERO_RPC_URL'], '')
        self.assertEqual(config['MONERO_WALLET_RPC_URL'], '')
        self.assertNotIn('MONERO_WALLET_RPC_PASSWORD', config)
        self.assert_no_secret_printed(result, config, allow_token=True)
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_local_bitcoin_node_ends_with_the_runnable_escrow_setup_commands(self):
        # A-495 (D-58), A-513: the installer only tells the operator how Bitcoin escrow is turned on; it edits no file.
        # The commands are the ones scripts/test-escrow-node-rehearsal.py runs verbatim in CI.
        for network in ('regtest', 'signet', 'testnet4'):
            with self.subTest(network=network):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=[
                    '--tor', '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test',
                    '--bitcoin-network', network, '--bitcoin-prune', 'yes', '--monero', 'disabled'])
                self.assert_started(result)
                config = self.config()
                commands = [line.strip() for line in result.stdout.splitlines() if 'escrow-node-setup.sh' in line
                            and not line.startswith('#')]
                self.assertEqual(len(commands), 2, result.stdout)
                setup, verify = commands
                # bitcoind listens on 8332 (compose.nodes.yaml -rpcport) and bitcoin-cli defaults to the chain's own
                # port, so the cli must say -rpcport=8332 too (SRE60-1).
                self.assertEqual(setup, f"./scripts/escrow-node-setup.sh --chain {network} --rpc-port 8332 --signer --cli "
                                        "'docker compose exec -T -u bitcoin bitcoin bitcoin-cli -datadir=/data -rpcport=8332'")
                self.assertNotIn('--verify', setup)
                self.assertEqual(verify, "ESCROW_BTC_RPC_URL='<the URL it printed>' ./scripts/escrow-node-setup.sh "
                                         f'--chain {network} --verify --via-compose app --wait 120')
                # The conf append quotes its heredoc so the $ in rpcauth stays literal (SRE60-3), then restarts the node.
                self.assertIn("docker compose exec -T -u bitcoin bitcoin sh -c 'cat >> /data/bitcoin.conf' <<'EOF'\n",
                              result.stdout)
                self.assertIn("cat >> .env <<'EOF'\nESCROW_BTC=on\nCOMPOSE_PROFILES=internal-db,bitcoin,escrow-signer\n",
                              result.stdout)
                self.assertLess(result.stdout.index('cat >> /data/bitcoin.conf'), result.stdout.index('docker compose restart bitcoin'))
                self.assertLess(result.stdout.index('docker compose restart bitcoin'), result.stdout.index('--verify'))
                self.assertIn('docker compose run --rm --no-deps escrow-signer init', result.stdout)
                self.assertGreater(result.stdout.index('escrow-node-setup.sh'), result.stdout.index('Setup token:'))
                self.assertIn('docs/operator-guide.md#escrow-node-set-up-and-the-public-network-smoke', result.stdout)
                self.assertIn('off until', result.stdout)
                # Nothing is written for it, and no escrow setting or secret appears in the output.
                self.assertFalse([key for key in config if key.startswith('ESCROW_')], config)
                self.assert_no_secret_printed(result, config, allow_token=True)

    def test_no_local_bitcoin_node_prints_no_escrow_setup_commands(self):
        for arguments in (['--tor', '--nodes', 'none'],
                          ['--tor', '--bitcoin', 'disabled', '--monero', 'local', '--monero-image', 'reviewed-monero:test',
                           '--monero-network', 'stagenet', '--monero-prune', 'yes']):
            with self.subTest(arguments=arguments):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                result = self.run_installer([''] * 6, arguments=arguments)
                self.assert_started(result)
                self.assertNotIn('escrow-node-setup.sh', result.stdout)

    def test_local_bitcoin_verifier_crypto_failure_leaves_no_install(self):
        for key, value in (('INSTALLER_TEST_OPENSSL_BAD_SALT', 'fail'),
                           ('INSTALLER_TEST_OPENSSL_BAD_SALT', 'empty'),
                           ('INSTALLER_TEST_OPENSSL_BAD_SALT', 'malformed'),
                           ('INSTALLER_TEST_OPENSSL_BAD_DGST', 'fail'),
                           ('INSTALLER_TEST_OPENSSL_BAD_DGST', 'malformed')):
            with self.subTest(key=key, value=value):
                self.log.unlink(missing_ok=True)
                result = self.run_installer([''] * 12, extra_env={key: value})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Could not generate the Bitcoin RPC verifier', result.stderr)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(list(self.root.glob('.env.install.*')), [])
                self.assertFalse(any('compose up' in call for call in self.calls()))

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
        result = self.run_installer(['tor', '', 'local', '', 'testnet4', '', 'local', '', 'stagenet', '', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual({key: config[key] for key in ('BITCOIN_IMAGE', 'MONERO_IMAGE', 'MONERO_WALLET_IMAGE')},
                         installer_image_defaults())

    def test_local_nodes_restrict_networks_to_escrow_support(self):
        # A-523: mainnet is main for Bitcoin Core and mainnet for Monero; a misnamed network is refused with the right
        # name (as scripts/escrow-node-setup.sh does), before Docker is asked anything.
        cases = (
            ('--bitcoin-network', 'mainnet', 'give --bitcoin-network main'),
            ('--bitcoin-network', 'testnet', 'testnet3 is not an escrow chain'),
            ('--bitcoin-network', 'testnet3', 'testnet3 is not an escrow chain'),
            ('--bitcoin-network', 'live', '--bitcoin-network takes one of: main signet testnet4 regtest'),
            ('--monero-network', 'main', 'give --monero-network mainnet'),
            ('--monero-network', 'testnet', 'give --monero-network stagenet'),
            ('--monero-network', 'fakechain', '--monero-network takes one of: mainnet stagenet'),
        )
        for flag, unsupported, message in cases:
            with self.subTest(flag=flag, unsupported=unsupported):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=['--tor', '--nodes', 'custom', '--bitcoin', 'disabled',
                                                              '--monero', 'disabled', flag, unsupported])
                self.assertEqual(result.returncode, 2)
                self.assertIn(message, result.stderr)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(self.calls(), [])
        # The same names are refused as answers.
        for answers, message in ((['tor', '', 'local', '', 'mainnet'], 'give main'),
                                 (['tor', '', 'disabled', 'local', '', 'testnet'], 'give stagenet')):
            with self.subTest(answers=answers):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(self.calls(), PREFLIGHT)

    # ---- init: mainnet by default (A-523) ------------------------------------------------------------------
    # Config strings only: Docker is a stub, so no node (and never a mainnet one) starts here.

    def env_lines(self):
        return (self.root / '.env').read_text().splitlines()

    def test_default_writes_mainnet_explicitly(self):
        # The bare interactive defaults and --nodes local write the mainnet class into .env, never a blank.
        for answers, arguments in (([''] * 12, []), (None, ['--tor', '--nodes', 'local'])):
            with self.subTest(arguments=arguments):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                self.assert_started(self.run_installer(answers, arguments=arguments))
                lines = self.env_lines()
                self.assertIn("BITCOIN_CHAIN='main'", lines)
                self.assertIn("MONERO_NETWORK='mainnet'", lines)
                self.assertIn("MONERO_NETWORK_FLAG=''", lines)
        # Bitcoin alone: no Monero key says mainnet, and no network flag is written for a daemon that does not run.
        (self.root / '.env').unlink()
        self.log.unlink()
        self.assert_started(self.run_installer([''], arguments=[
            '--tor', '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test', '--bitcoin-prune', 'yes',
            '--monero', 'disabled']))
        config = self.config()
        self.assertEqual(config['BITCOIN_CHAIN'], 'main')
        self.assertEqual(config['MONERO_NETWORK'], '')
        self.assertNotIn('MONERO_NETWORK_FLAG', config)
        self.assertNotIn('mainnet', [value for key, value in config.items() if key.startswith('MONERO')])

    def test_monero_only_mainnet_leaves_bitcoin_chain_blank(self):
        # The app reads a blank BITCOIN_CHAIN beside MONERO_NETWORK=mainnet as a mainnet market (marketLiveFromEnv).
        result = self.run_installer(['', ''], arguments=[
            '--tor', '--bitcoin', 'disabled', '--monero', 'local', '--monero-image', 'reviewed-monero:test',
            '--monero-prune', 'yes'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['BITCOIN_CHAIN'], '')
        self.assertEqual(config['MONERO_NETWORK'], 'mainnet')
        self.assertEqual(config['MONERO_NETWORK_FLAG'], '')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,monero')
        for key in ('BITCOIN_PRUNE_MB', 'PAYMENT_CONFIRMATIONS_BTC', 'BITCOIN_IMAGE', 'ESCROW_XMR'):
            self.assertNotIn(key, config)
        self.assertIn('MAINNET', result.stdout)
        self.assertNotIn('escrow-node-setup.sh', result.stdout)

    def test_init_refuses_mixed_network_class(self):
        # Two chosen networks of different classes (one mainnet, one a test network): refused before .env is written
        # (errMixedNetworks), naming both options. A defaulted network follows the chosen one instead (below).
        cases = [
            (None, ['--tor', '--nodes', 'local', '--bitcoin-network', 'main', '--monero-network', 'stagenet']),
            (None, ['--tor', '--nodes', 'local', '--bitcoin-network', 'regtest', '--monero-network', 'mainnet']),
            (['tor', '', 'local', '', 'testnet4', '', 'local', '', 'mainnet', ''], []),
        ]
        for answers, arguments in cases:
            with self.subTest(arguments=arguments, answers=answers):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers, arguments=arguments)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('mix mainnet with a test network', result.stderr)
                self.assertIn('--bitcoin-network', result.stderr)
                self.assertIn('--monero-network', result.stderr)
                self.assertIn('Nothing was written', result.stderr)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(list(self.root.glob('.env.install.*')), [])
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertEqual(self.generated_secrets(), [])

    def assert_test_class(self, result, chain, network):
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['BITCOIN_CHAIN'], chain)
        self.assertEqual(config['MONERO_NETWORK'], network)
        self.assertEqual(config['MONERO_NETWORK_FLAG'], '--stagenet')
        self.assertEqual(config['BITCOIN_PRUNE_MB'], self.compose_bitcoin_prune_default())
        self.assertNotIn('PAYMENT_CONFIRMATIONS_BTC', config)
        self.assertNotIn('MAINNET', result.stdout)
        self.assertNotIn('No mainnet run', result.stdout)

    def test_explicit_test_network_carries_the_other_coin(self):
        # The market's class follows an explicitly chosen network: a Bitcoin test chain makes Monero default to stagenet.
        self.assert_test_class(self.run_installer(None, arguments=['--tor', '--nodes', 'local', '--bitcoin-network', 'signet']),
                               'signet', 'stagenet')
        # So does an answer: Monero's network question then defaults to stagenet (Enter), as does its escrow question (no).
        (self.root / '.env').unlink()
        self.log.unlink()
        self.assert_test_class(self.run_installer(['tor', '', 'local', '', 'testnet4', '', 'local', '', '', '', '']),
                               'testnet4', 'stagenet')

    def test_explicit_monero_test_network_carries_bitcoin(self):
        self.assert_test_class(self.run_installer(None, arguments=['--tor', '--nodes', 'local', '--monero-network', 'stagenet']),
                               'signet', 'stagenet')
        # Asked interactively, Bitcoin's network question comes first: its default follows a --monero-network option...
        (self.root / '.env').unlink()
        self.log.unlink()
        self.assert_test_class(self.run_installer([''] * 6, arguments=[
            '--tor', '--nodes', 'custom', '--bitcoin', 'local', '--monero', 'local', '--monero-network', 'stagenet']),
            'signet', 'stagenet')
        # ...and a Monero test answer after a defaulted Bitcoin network moves Bitcoin to signet, saying so.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer(['tor', '', 'local', '', '', '', 'local', '', 'stagenet', '', ''])
        self.assert_test_class(result, 'signet', 'stagenet')
        self.assertIn('BITCOIN follows the Monero network', result.stdout)
        # A coin disabled by its option chooses nothing: Monero alone stays on the mainnet default.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer([''], arguments=['--tor', '--bitcoin', 'disabled', '--bitcoin-network', 'signet',
                                                     '--monero', 'local', '--monero-image', 'reviewed-monero:test',
                                                     '--monero-prune', 'yes', '--monero-escrow', 'no'])
        self.assert_started(result)
        self.assertEqual(self.config()['MONERO_NETWORK'], 'mainnet')

    def test_mainnet_prune_and_confirmations(self):
        cases = (
            (['--bitcoin-network', 'main'], dict(BITCOIN_PRUNE_MB='15000', PAYMENT_CONFIRMATIONS_BTC='6')),
            (['--bitcoin-network', 'main', '--bitcoin-prune', 'no'], dict(BITCOIN_PRUNE_MB='0', PAYMENT_CONFIRMATIONS_BTC='6')),
            (['--bitcoin-network', 'signet'], dict(BITCOIN_PRUNE_MB='2000', PAYMENT_CONFIRMATIONS_BTC=None)),
            (['--bitcoin-network', 'testnet4', '--bitcoin-prune', 'no'], dict(BITCOIN_PRUNE_MB='0', PAYMENT_CONFIRMATIONS_BTC=None)),
            (['--bitcoin-network', 'regtest'], dict(BITCOIN_PRUNE_MB='2000', PAYMENT_CONFIRMATIONS_BTC=None)),
        )
        for extra, expected in cases:
            with self.subTest(extra=extra):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                arguments = ['--tor', '--nodes', 'local', '--monero', 'disabled', *extra]
                self.assert_started(self.run_installer(None, arguments=arguments))
                config = self.config()
                self.assertEqual({key: config.get(key) for key in expected}, expected)

    def test_mainnet_init_warns_real_funds_unrun(self):
        result = self.run_installer(None, arguments=['--tor', '--nodes', 'local'])
        self.assert_started(result)
        out = result.stdout
        for text in ('MAINNET', 'real funds', 'No mainnet run of this software is recorded',
                     # Per coin, pruned size and multi-day first sync, each labelled an estimate.
                     'Bitcoin mainnet', 'about 30 GB', '856 GB', '14 GB', 'Monero mainnet', '45 GB in April 2022',
                     'more today', 'takes days', 'estimate',
                     # The prune window and its backup consequence.
                     'about two months', 'cannot be rescanned',
                     # Tor with local nodes: their peer traffic leaves directly unless the overlay is added.
                     'not routed through Tor', 'compose.nodes-tor.yaml',
                     # Monero escrow on mainnet.
                     'custodial'):
            self.assertIn(text, out)
        # Printed before .env is written: the stub refuses the disk check once .env exists, and the notice comes first.
        self.assertLess(out.index('MAINNET'), out.index('Docker data directory'))
        # Clearnet: nothing about Tor routing; test networks: no mainnet notice at all.
        for arguments, mainnet in ((['--clearnet', '--behind-proxy', 'yes', '--nodes', 'local'], True),
                                   (['--tor', '--nodes', 'local', '--bitcoin-network', 'signet',
                                     '--monero-network', 'stagenet'], False)):
            with self.subTest(arguments=arguments):
                (self.root / '.env').unlink()
                self.log.unlink()
                result = self.run_installer(None, arguments=arguments)
                self.assert_started(result)
                self.assertNotIn('not routed through Tor', result.stdout)
                self.assertEqual('MAINNET' in result.stdout, mainnet)
                self.assertEqual('No mainnet run' in result.stdout, mainnet)

    def test_local_node_disk_check_warns_and_never_refuses(self):
        # A-523 (D-66): free space under Docker's data directory below the chain's estimate is a WARNING, never a
        # refusal; an unreadable value is said and the install continues. The text names the chain it sizes.
        mainnet = ['--tor', '--nodes', 'local']
        cases = (
            (mainnet, {}, 'has 2861 GB free', None),
            (mainnet, {'INSTALLER_TEST_DF_KB': str(20 * 2**20)}, 'WARNING', 'about 75 GB'),
            (['--tor', '--nodes', 'local', '--bitcoin-network', 'signet', '--monero-network', 'stagenet'],
             {'INSTALLER_TEST_DF_KB': str(20 * 2**20)}, 'WARNING', 'about 28 GB'),
            (mainnet, {'INSTALLER_TEST_DOCKER_ROOT_FAIL': '1'}, 'Could not read the free space', None),
            (mainnet, {'INSTALLER_TEST_DF_FAIL': '1'}, 'Could not read the free space', None),
            (mainnet, {'INSTALLER_TEST_DOCKER_ROOT': '/nonexistent/docker'}, 'Could not read the free space', None),
        )
        for arguments, extra, message, size in cases:
            with self.subTest(extra=extra, arguments=arguments):
                (self.root / '.env').unlink(missing_ok=True)
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=arguments, extra_env=extra)
                self.assert_started(result)
                self.assertIn(message, result.stdout + result.stderr)
                if size:
                    self.assertIn(size, result.stdout + result.stderr)
                if message != 'WARNING':
                    self.assertNotIn('WARNING', result.stdout + result.stderr)
        # Regtest alone has no size to check: Docker is not asked.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer(None, arguments=['--tor', '--nodes', 'local', '--bitcoin-network', 'regtest',
                                                     '--monero', 'disabled'])
        self.assert_started(result)
        self.assertNotIn(DOCKER_ROOT, self.calls())

    def test_mainnet_prints_chain_main_setup(self):
        result = self.run_installer(None, arguments=[
            '--tor', '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test', '--bitcoin-network', 'main',
            '--bitcoin-prune', 'yes', '--monero', 'disabled'])
        self.assert_started(result)
        commands = [line.strip() for line in result.stdout.splitlines() if 'escrow-node-setup.sh' in line
                    and not line.startswith('#')]
        self.assertEqual(len(commands), 2, result.stdout)
        self.assertTrue(commands[0].startswith('./scripts/escrow-node-setup.sh --chain main --rpc-port 8332 '), commands[0])
        self.assertIn('--chain main --verify --via-compose app', commands[1])
        block = result.stdout[result.stdout.index('Bitcoin escrow stays off'):]
        for text in ('plain fee address is refused on mainnet', 'descriptor', 'wpkh(', 'sized on test chains',
                     'PAYMENT_CONFIRMATIONS_BTC', 'ESCROW_BTC_FEE_RATE'):
            self.assertIn(text, block)
        # Every added line is a comment, safe to paste.
        for line in block.splitlines():
            if 'descriptor' in line or 'test chains' in line:
                self.assertTrue(line.startswith('#'), line)
        # A test chain's block is as it was.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer(None, arguments=[
            '--tor', '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test', '--bitcoin-network', 'signet',
            '--bitcoin-prune', 'yes', '--monero', 'disabled'])
        self.assert_started(result)
        self.assertNotIn('plain fee address', result.stdout)
        self.assertNotIn('sized on test chains', result.stdout)

    def test_mainnet_monero_escrow_defaults_off(self):
        # The question is asked on mainnet too, says real funds and custodial, and defaults to no.
        result = self.run_installer(['tor', '', 'disabled', 'local', '', '', '', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['MONERO_NETWORK'], 'mainnet')
        for key in ('ESCROW_XMR', 'ESCROW_XMR_WALLET_RPC_PASSWORD', 'MONERO_WALLET_SECRET', 'ESCROW_XMR_SEED_KEY'):
            self.assertNotIn(key, config)
        self.assertIn('MONERO escrow (off by default)', result.stdout)
        self.assertIn('real funds', result.stdout)
        # Turned on: allowed on mainnet, with its seed key still required (asked, never defaulted).
        (self.root / '.env').unlink()
        self.log.unlink()
        flags = ['--tor', '--bitcoin', 'disabled', '--monero', 'local', '--monero-image', 'reviewed-monero:test',
                 '--monero-network', 'mainnet', '--monero-prune', 'yes', '--monero-escrow', 'yes']
        result = self.run_installer(None, arguments=flags)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('end of input', result.stderr)
        self.assertFalse((self.root / '.env').exists())
        key = self.seed_key()
        self.log.unlink()
        result = self.run_installer(None, arguments=[*flags, '--monero-escrow-seed-key', str(key)])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['ESCROW_XMR'], 'on')
        self.assertEqual(config['MONERO_NETWORK'], 'mainnet')
        self.assertEqual(config['MONERO_NETWORK_FLAG'], '')
        self.assertIn('Monero escrow is on (mainnet, custodial', result.stdout)
        self.assertIn('real funds', result.stdout)
        self.assert_no_secret_printed(result, config, allow_token=True)

    def seed_key(self, text=None):
        path = self.root / 'operator-seed-key.asc'
        path.write_text(text or '-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmDMEZtest\n-----END PGP PUBLIC KEY BLOCK-----\n')
        return path

    def assert_monero_escrow(self, result, key):
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['ESCROW_XMR'], 'on')
        self.assertRegex(config['ESCROW_XMR_WALLET_RPC_PASSWORD'], '^[0-9a-f]{48}$')
        self.assertEqual(config['ESCROW_XMR_WALLET_RPC_URL'],
                         'http://escrow:' + config['ESCROW_XMR_WALLET_RPC_PASSWORD'] + '@monero-escrow-wallet:18083')
        self.assertEqual(config['ESCROW_XMR_DAEMON_URL'], 'http://monero:18081')
        self.assertRegex(config['MONERO_WALLET_SECRET'], '^[0-9a-f]{64}$')
        for other in ('APP_SECRET_KEY', 'SETUP_TOKEN', 'ESCROW_XMR_WALLET_RPC_PASSWORD'):
            self.assertNotEqual(config['MONERO_WALLET_SECRET'], config[other])
        self.assertEqual(config['ESCROW_XMR_SEED_KEY'], '/etc/opsecmkt/escrow-xmr-seed-key.asc')
        self.assertEqual(config['COMPOSE_PROFILES'].split(',')[-2:], ['monero', 'monero-escrow-wallet'])
        self.assertTrue(config['COMPOSE_FILE'].endswith(':compose.escrow-xmr.yaml'), config['COMPOSE_FILE'])
        copy = self.root / 'escrow-xmr-seed-key.asc'
        self.assertEqual(copy.read_text(), key.read_text())
        self.assertEqual(stat.S_IMODE(copy.stat().st_mode), 0o644)
        self.assert_no_secret_printed(result, config, allow_token=True)
        self.assertIn('Monero escrow is on (stagenet, custodial', result.stdout)

    def test_monero_escrow_on_stagenet_is_asked_with_the_monero_questions(self):
        key = self.seed_key()
        result = self.run_installer(['tor', '', 'disabled', 'local', '', 'stagenet', '', 'yes', str(key)])
        self.assert_monero_escrow(result, key)
        self.assertIn('custodial', result.stdout)

    def test_monero_escrow_flags_need_no_prompt(self):
        key = self.seed_key()
        result = self.run_installer(None, arguments=['--tor', '--bitcoin', 'disabled', '--monero', 'local', '--monero-image', 'reviewed-monero:test',
                                                     '--monero-network', 'stagenet', '--monero-prune', 'yes',
                                                     '--monero-escrow', 'yes', '--monero-escrow-seed-key', str(key)])
        self.assert_monero_escrow(result, key)

    def test_monero_escrow_off_by_default_and_never_defaulted(self):
        # The default answer is no, and nothing of Monero escrow is written.
        result = self.run_installer(['tor', '', 'disabled', 'local', '', 'stagenet', '', ''])
        self.assert_started(result)
        config = self.config()
        for key in ('ESCROW_XMR', 'ESCROW_XMR_WALLET_RPC_PASSWORD', 'MONERO_WALLET_SECRET', 'ESCROW_XMR_SEED_KEY'):
            self.assertNotIn(key, config)
        self.assertNotIn('monero-escrow-wallet', config['COMPOSE_PROFILES'])
        self.assertNotIn('compose.escrow-xmr.yaml', config['COMPOSE_FILE'])
        self.assertFalse((self.root / 'escrow-xmr-seed-key.asc').exists())
        # Turned on with the flag but no seed key: it is asked, never defaulted (every other question has its flag, and
        # bash prints no prompt without a terminal), so empty input stops the installer before anything is written.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer(None, arguments=['--tor', '--bitcoin', 'disabled', '--monero', 'local', '--monero-image', 'reviewed-monero:test',
                                                     '--monero-network', 'stagenet', '--monero-prune', 'yes',
                                                     '--monero-escrow', 'yes'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('end of input', result.stderr)
        self.assertFalse((self.root / '.env').exists())

    def test_monero_escrow_refusals_write_nothing(self):
        key = self.seed_key()
        private = self.root / 'private.asc'
        private.write_text('-----BEGIN PGP PRIVATE KEY BLOCK-----\nxx\n-----END PGP PRIVATE KEY BLOCK-----\n')
        not_a_key = self.root / 'notes.txt'
        not_a_key.write_text('not a key\n')
        base = ['--tor', '--bitcoin', 'disabled', '--monero-image', 'reviewed-monero:test', '--monero-prune', 'yes', '--monero-escrow', 'yes']
        cases = [
            (['--monero', 'local', '--monero-network', 'testnet', '--monero-escrow-seed-key', str(key)], 'give --monero-network stagenet', []),
            (['--monero', 'disabled', '--monero-escrow-seed-key', str(key)], 'Monero escrow needs the local Monero node'),
            (['--monero', 'local', '--monero-network', 'stagenet', '--monero-escrow-seed-key', str(private)], 'holds a private key'),
            (['--monero', 'local', '--monero-network', 'stagenet', '--monero-escrow-seed-key', str(not_a_key)], 'not an armored OpenPGP public key'),
            (['--monero', 'local', '--monero-network', 'stagenet', '--monero-escrow-seed-key', str(self.root / 'missing.asc')], 'does not exist'),
        ]
        for case in cases:
            extra, message = case[:2]
            expected_calls = case[2] if len(case) == 3 else PREFLIGHT
            with self.subTest(message=message, extra=extra):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=[*base, *extra])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.calls(), expected_calls)
                self.assertFalse((self.root / '.env').exists())
                self.assertFalse((self.root / 'escrow-xmr-seed-key.asc').exists())
        result = self.run_installer(None, arguments=['--tor', '--monero-escrow-seed-key', str(key)])
        self.assertEqual(result.returncode, 2)
        self.assertIn('needs --monero-escrow yes', result.stderr)

    def test_check_refuses_monero_escrow_without_its_password_or_key(self):
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,monero,monero-escrow-wallet'),
                       'needs ESCROW_XMR_WALLET_RPC_PASSWORD')
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml:compose.escrow-xmr.yaml'),
                       'escrow-xmr-seed-key.asc (the Monero escrow seed key it mounts) is missing')

    def test_secret_generation_failure_starts_nothing(self):
        # openssl rand calls: 1 database password, 2 APP_SECRET_KEY, 3 setup token, 4 audit key; local Bitcoin
        # adds 5 its RPC password and 6 the verifier salt; Monero escrow adds its wallet-RPC password and
        # MONERO_WALLET_SECRET. A Monero daemon without escrow needs no wallet credentials.
        seed_key = self.seed_key()
        bitcoin = ['--tor', '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test',
                   '--bitcoin-network', 'testnet4', '--bitcoin-prune', 'yes', '--monero', 'disabled']
        monero_escrow = ['--tor', '--bitcoin', 'disabled', '--monero', 'local', '--monero-image', 'reviewed-monero:test',
                         '--monero-network', 'stagenet', '--monero-prune', 'yes', '--monero-escrow', 'yes',
                         '--monero-escrow-seed-key', str(seed_key)]
        both_escrows = ['--tor', '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test',
                        '--bitcoin-network', 'testnet4', '--bitcoin-prune', 'yes', '--monero', 'local',
                        '--monero-image', 'reviewed-monero:test', '--monero-network', 'stagenet',
                        '--monero-prune', 'yes', '--monero-escrow', 'yes', '--monero-escrow-seed-key', str(seed_key)]
        scenarios = [
            (1, 'random secret', ['clearnet', '', '', 'disabled', 'disabled'], []),
            (2, 'random secret', ['clearnet', '', '', 'disabled', 'disabled'], []),
            (3, 'random secret', ['clearnet', '', '', 'disabled', 'disabled'], []),
            (5, 'random secret', [], monero_escrow),  # Monero escrow wallet-RPC password.
            (6, 'random secret', [], monero_escrow),  # Monero escrow wallet-seed sealing secret.
            (5, 'random secret', [], bitcoin),        # Local Bitcoin RPC password.
            (6, 'Bitcoin RPC verifier', [], bitcoin), # Salt for the local Bitcoin RPC verifier.
            (7, 'random secret', [], both_escrows),   # Monero escrow wallet-RPC password after Bitcoin setup.
            (8, 'random secret', [], both_escrows),   # Monero escrow wallet-seed sealing secret.
        ]
        for call, message, answers, arguments in scenarios:
            with self.subTest(call=call, arguments=arguments):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers, arguments=arguments, openssl_empty_call=call)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(list(self.root.glob('.env.install.*')), [])

    def test_containers_from_another_checkout_are_refused_before_anything_is_written(self):
        # A second checkout under the same project name would recreate the other installation's containers
        # with new secrets, and its `docker compose down -v` would delete that installation's volumes.
        other = '/srv/live-opsecmkt'
        result = self.run_installer([''] * 12, extra_env={
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
        result = self.run_installer([''] * 12, extra_env={
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
            ['tor', '', 'custom', 'external'],
            ['tor', '', 'custom', 'disabled', 'external'],
            ['tor', '', 'local', ''],
            ['tor', '', 'local', '', 'testnet4', 'maybe', 'disabled'],
            ['tor', '', 'disabled', 'local', '', 'stagenet', 'maybe'],
            ['tor', "postgres://test:quote'@database/scratch", 'disabled', 'disabled'],
            # I8: a placeholder database URL is refused before .env is written.
            ['tor', 'postgres://opsecmkt:REPLACE_ME@database/scratch', 'disabled', 'disabled'],
            ['clearnet', 'postgres://opsecmkt:changeme@database/scratch', 'yes', 'disabled', 'disabled'],
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
            ([''] * 12, []),
            (['tor', '', 'local', '', 'testnet4', '', 'local', '', 'stagenet', '', ''], []),
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
             dict(APP_MODE='clearnet', COOKIE_SECURE='false', COMPOSE_PROFILES='internal-db,bitcoin,monero',
                  BITCOIN_CHAIN='main', MONERO_NETWORK='mainnet', MONERO_NETWORK_FLAG='', BITCOIN_PRUNE_MB='15000',
                  PAYMENT_CONFIRMATIONS_BTC='6', BITCOIN_IMAGE=installer_image_defaults()['BITCOIN_IMAGE'])),
            (['--clearnet', '--behind-proxy', 'no', '--nodes', 'local', '--bitcoin-network', 'signet',
              '--monero-network', 'stagenet'],
             dict(COMPOSE_PROFILES='internal-db,bitcoin,monero', BITCOIN_CHAIN='signet', MONERO_NETWORK='stagenet',
                  MONERO_NETWORK_FLAG='--stagenet', BITCOIN_PRUNE_MB='2000', PAYMENT_CONFIRMATIONS_BTC=None)),
            (['init', '--clearnet', '--behind-proxy', 'yes', '--nodes', 'none'],
             dict(APP_MODE='clearnet', COOKIE_SECURE='true', COMPOSE_PROFILES='internal-db')),
            (['init', '--tor', '--external-db', 'postgres://market@database.example/market', '--nodes', 'custom',
              '--bitcoin', 'local', '--bitcoin-image', 'reviewed-bitcoin:test', '--bitcoin-network', 'signet',
              '--bitcoin-prune', 'no', '--monero', 'disabled'],
             dict(APP_MODE='tor', DATABASE_URL='postgres://market@database.example/market', COMPOSE_PROFILES='bitcoin',
                  BITCOIN_IMAGE='reviewed-bitcoin:test', BITCOIN_CHAIN='signet', BITCOIN_PRUNE_MB='0',
                  COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.external-egress.yaml')),
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
        self.assertIn('takes one of: local disabled', result.stderr)
        self.assertEqual(self.calls(), [])
        self.assertFalse((self.root / '.env').exists())

    def test_bare_invocation_and_local_alias_are_init(self):
        def written(answers, arguments):
            (self.root / '.env').unlink(missing_ok=True)
            self.log.unlink(missing_ok=True)
            result = self.run_installer(answers, arguments=arguments)
            self.assert_started(result)
            return {key: ('<secret>' if key in SECRET_KEYS or key.endswith('_RPC_URL') and value else value)
                    for key, value in self.config().items()}, self.calls()
        self.assertEqual(written([''] * 12, []), written([''] * 12, ['init']))
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

    def test_init_external_db_refuses_a_database_that_belongs_to_a_market(self):
        arguments = ['init', '--clearnet', '--external-db', 'postgres://market@database.example/market',
                     '--behind-proxy', 'yes', '--nodes', 'none']
        self.set_installed(True)
        result = self.run_installer(None, arguments=arguments)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('already belongs to a market', result.stderr)
        self.assertIn("restore that market's .env", result.stderr)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual(self.calls(), [*PREFLIGHT, *CHECK, PROBE])
        self.assertNotIn('Setup token', result.stdout)
        self.assertFalse((self.root / '.env.lock').exists())
        # A database whose setup is pending (or empty) is used.
        self.log.unlink()
        self.set_installed(False)
        self.assert_started(self.run_installer(None, arguments=arguments))
        # An unreadable database starts nothing; .env is kept for diagnosis, as a failed check keeps it.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer(None, arguments=arguments, extra_env={'INSTALLER_TEST_STATUS_FAIL': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('Could not read the external database', result.stderr)
        self.assertNotIn(UP, self.calls())

    def test_xtrace_from_the_environment_prints_no_secret(self):
        # SHELLOPTS=xtrace in the caller's environment would trace every expansion, passwords included.
        result = self.run_installer([PASSWORD, PASSWORD], arguments=['--local', '--admin', 'site_admin'],
                                    extra_env={'SHELLOPTS': 'xtrace'})
        self.assertEqual(result.returncode, 0, result.stderr)
        output = result.stdout + result.stderr
        self.assertNotIn(PASSWORD, output)
        self.assertNotIn(PASSWORD.replace(' ', '\\ '), output)
        # Only the two lines before tracing is switched off are traced.
        traced = [line for line in result.stderr.splitlines() if line.startswith('+')]
        self.assertEqual(traced, ['+ set -euo pipefail', '+ set +x'])
        self.assert_no_secret_printed(result, self.config())

    def test_init_admin_mismatched_passwords_refused(self):
        for answers, message in (([PASSWORD, PASSWORD + 'x'], 'do not match'), (['short', 'short'], '12-72 bytes'),
                                 ([PASSWORD], 'No password')):
            with self.subTest(message=message):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers, arguments=['--local', '--admin', 'first_admin'])
                self.assertNotEqual(result.returncode, 0)
                # Either refusal means nothing was changed; which step trips first can differ by platform.
                self.assertTrue(message in result.stderr or 'nothing was changed' in result.stderr, result.stderr)
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(self.generated_secrets(), [])
                self.assertNotIn(PASSWORD, result.stdout + result.stderr)

    def test_init_admin_password_not_utf8_refused_before_anything_is_written(self):
        # QA5W5B-7: -bootstrap-admin refuses a password that is not valid UTF-8, which the sign-in form could never send;
        # the installer refuses it up front, before .env is written or anything is started. LATIN1 "été" (a terminal
        # set to LATIN1), an encoded surrogate and an overlong "/"; QA5W6-4 (mutant S6): the code point above U+10FFFF that
        # \xf4 starts (U+110000) and a 4-byte sequence led by \xf5, which no UTF-8 holds.
        for bad in ('correct horse \udce9t\udce9 battery', 'surrogate \udced\udca0\udc80 battery', 'overlong \udcc0\udcaf battery',
                    'above U+10FFFF \udcf4\udc90\udc80\udc80 battery', 'F5 lead \udcf5\udc80\udc80\udc80 battery'):
            with self.subTest(bad=bad):
                self.log.unlink(missing_ok=True)
                result = self.run_installer([bad, bad], arguments=['--local', '--admin', 'first_admin'])
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('The password is not valid UTF-8', result.stderr)
                self.assertIn('nothing was written or started', result.stderr)
                self.assertEqual(self.calls(), PREFLIGHT)
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(self.generated_secrets(), [])
                self.assertFalse((self.state / 'bootstrap-stdin').exists())
        # Valid multibyte UTF-8, of every length, is the password piped to the server.
        good = 'Crème brûlée 東京 🔒'
        result = self.run_installer([good, good], arguments=['--local', '--admin', 'first_admin'])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.state / 'bootstrap-stdin').read_text(encoding='utf-8'), good + '\n')
        # Bash versions differ on a NUL in read input: some discard it, others stop at it. The installer must
        # send only the value Bash actually read, and reject a shortened prefix before writing configuration.
        nul_input = PASSWORD.replace(' ', ' \0', 1)
        probe = subprocess.run(['/bin/bash', '-c', 'IFS= read -r -s -p x value; printf %s "$value"'],
                               input=(nul_input + '\n').encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               check=True).stdout
        (self.root / '.env').unlink()
        self.log.unlink()
        self.set_installed(False)
        (self.state / 'bootstrap-stdin').unlink(missing_ok=True)
        result = self.run_installer([nul_input] * 2, arguments=['--local', '--admin', 'first_admin'])
        if probe == PASSWORD.encode():
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((self.state / 'bootstrap-stdin').read_text(), PASSWORD + '\n')
        elif probe == b'correct ':
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertIn('Use a password of 12-72 bytes', result.stderr)
            self.assertEqual(self.calls(), PREFLIGHT)
            self.assertFalse((self.root / '.env').exists())
            self.assertFalse((self.state / 'bootstrap-stdin').exists())
        else:
            self.fail(f'unexpected Bash read behavior for NUL input: {len(probe)} bytes')

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

    def test_env_edit_write_failure_leaves_env_unchanged(self):
        # A full disk while status retires SETUP_TOKEN (its caller tolerates failure, so errexit is off there),
        # or while add-mirror or upgrade edit .env: the edit stops and .env keeps every byte.
        padding = ''.join(f'# operator note {i}: keep this line exactly as it is written here\n' for i in range(40))
        # upgrade first copies .env whole (that copy fits under the second limit, the size of .env), then adds
        # keys. (macOS does not apply the limit to appending writes; the installer makes none to .env.)
        for arguments, limit, message in ((['status'], 1024, 'Could not write .env; it was not changed.'),
                                          (['add-mirror'], 1024, 'Could not write .env; it was not changed.'),
                                          (['upgrade', '--no-backup'], 1024, 'Could not copy .env to .env.pre-upgrade-'),
                                          (['upgrade', '--no-backup'], 0, 'Could not write .env; it was not changed.')):
            with self.subTest(arguments=arguments, limit=limit):
                values = installed_env() if arguments[0] != 'upgrade' else alpha_env()
                original = self.write_env(padding + env_text(values))
                self.assertGreater(len(original), 3000)
                if not limit:
                    # Headroom so the whole-file copy fits on Linux too (cat there refuses a copy that reaches
                    # the limit exactly); any edit that follows adds at least one byte.
                    limit = len(original) + 1
                self.set_installed(True)
                result = self.command(*arguments, file_size_limit=limit)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                # Either refusal means nothing was changed; which step trips first can differ by platform.
                self.assertTrue(message in result.stderr or 'nothing was changed' in result.stderr, result.stderr)
                if limit == 1024:
                    self.assertEqual(self.pre_upgrade_copies(), [])
                self.assertEqual((self.root / '.env').read_bytes(), original)
                self.assertEqual(stat.S_IMODE((self.root / '.env').stat().st_mode), 0o600)
                self.assertEqual(list(self.root.glob('.env.edit.*')), [])
                self.assertFalse((self.root / '.env.lock').exists())
                self.assertNotIn('Removed SETUP_TOKEN', result.stdout)
                self.assert_no_secret_printed(result, values)
                for copy in self.pre_upgrade_copies():
                    copy.unlink()

    def test_env_lock_refuses_a_second_command(self):
        values = installed_env()
        original = self.write_env(values)
        self.set_installed(True)
        (self.root / '.env.lock').mkdir()
        for arguments in (['upgrade', '--no-backup'], ['add-mirror'], ['setup-token', '--new'], ['add-scanner']):
            with self.subTest(arguments=arguments):
                result = self.command(*arguments)
                self.assertEqual(result.returncode, 1)
                self.assertIn('Another ./scripts/install.sh command is changing .env', result.stderr)
                self.assertIn('rmdir .env.lock', result.stderr)
                self.assertEqual((self.root / '.env').read_bytes(), original)
                self.assertEqual(self.calls(), ['compose version'])
                self.assertEqual(self.pre_upgrade_copies(), [])
                self.assertTrue((self.root / '.env.lock').is_dir())  # the holder's lock is left alone
        # status still reports, but leaves .env to the command holding the lock.
        result = self.command('status')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Setup: complete', result.stdout)
        self.assertIn('.env.lock', result.stdout)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        # init refuses before writing anything.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer(None, arguments=['--local'])
        self.assertEqual(result.returncode, 1)
        self.assertIn('.env.lock', result.stderr)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual(self.calls(), ['compose version'])
        # Released: every command removes its lock when it ends, also when it fails.
        (self.root / '.env.lock').rmdir()
        self.write_env(values)
        self.assertEqual(self.command('status').returncode, 0)
        self.assertFalse((self.root / '.env.lock').exists())
        self.write_env(installed_env(COOKIE_SECURE='TRUE'))
        self.assertNotEqual(self.upgrade().returncode, 0)
        self.assertFalse((self.root / '.env.lock').exists())

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

    def test_check_restricts_local_nodes_to_escrow_networks(self):
        # A-460: main is the mainnet opt-in; testnet3 stays refused.
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,bitcoin', BITCOIN_CHAIN='test'),
                       'BITCOIN_CHAIN must be regtest, testnet4, signet or main')
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,monero', MONERO_NETWORK='testnet'),
                       'MONERO_NETWORK must be stagenet or mainnet')
        # A local mainnet daemon needs a blank MONERO_NETWORK_FLAG= line (monerod has no --mainnet).
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,monero', MONERO_NETWORK='mainnet'),
                       'MONERO_NETWORK=mainnet needs a blank MONERO_NETWORK_FLAG= line')
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,monero', MONERO_NETWORK='mainnet', MONERO_NETWORK_FLAG=''))
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,monero', MONERO_NETWORK='stagenet', MONERO_NETWORK_FLAG='--testnet'),
                       'MONERO_NETWORK_FLAG must be --stagenet (or absent)')
        # A blank flag beside a stagenet (or blank) MONERO_NETWORK would run a mainnet daemon for a test market.
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,monero', MONERO_NETWORK='stagenet', MONERO_NETWORK_FLAG=''),
                       'MONERO_NETWORK_FLAG must be --stagenet (or absent)')
        # What init writes (A-523): main and mainnet with a blank flag, or the test networks with --stagenet.
        password = hex_value(48, 'rpc')
        nodes = dict(COMPOSE_PROFILES='internal-db,bitcoin,monero', BITCOIN_RPC_PASSWORD=password)
        self.check_env(installed_env(**nodes, BITCOIN_CHAIN='main', MONERO_NETWORK='mainnet', MONERO_NETWORK_FLAG=''))
        self.check_env(installed_env(**nodes, BITCOIN_CHAIN='signet', MONERO_NETWORK='stagenet',
                                     MONERO_NETWORK_FLAG='--stagenet'))

    def test_check_refuses_mixed_class(self):
        # Mirrors the app's errMixedNetworks (marketLiveFromEnv): a market is all mainnet or all test networks, whatever
        # node profiles run; a blank network takes the other coin's class.
        mixed = 'mix mainnet with a test network'
        self.check_env(installed_env(BITCOIN_CHAIN='main', MONERO_NETWORK='stagenet'), mixed)
        self.check_env(installed_env(BITCOIN_CHAIN='signet', MONERO_NETWORK='mainnet'), mixed)
        self.check_env(installed_env(BITCOIN_CHAIN='regtest', MONERO_NETWORK='mainnet'), mixed)
        self.check_env(installed_env(BITCOIN_CHAIN='main', MONERO_NETWORK='mainnet'))
        self.check_env(installed_env(BITCOIN_CHAIN='main', MONERO_NETWORK=''))
        self.check_env(installed_env(BITCOIN_CHAIN='', MONERO_NETWORK='mainnet'))
        self.check_env(installed_env(BITCOIN_CHAIN='testnet4', MONERO_NETWORK='stagenet'))

    def test_check_refuses_legacy_pooled_values_and_profile(self):
        for key in ('BITCOIN_RPC_URL', 'BITCOIN_WALLET', 'MONERO_RPC_URL', 'MONERO_WALLET_RPC_URL'):
            with self.subTest(key=key):
                result = self.check_env(installed_env(**{key: 'legacy-value'}), f'{key} is a retired pooled-payment setting')
                self.assertNotIn('legacy-value', result.stdout + result.stderr)
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,monero,monero-wallet'),
                       'retired monero-wallet profile')

    def test_check_refuses_overlay_mismatch(self):
        self.check_env(installed_env(APP_MODE='clearnet'), 'APP_MODE=clearnet needs compose.clearnet.yaml in COMPOSE_FILE')
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.clearnet.yaml:compose.tor.yaml'),
                       'lists both compose.clearnet.yaml and compose.tor.yaml')
        self.check_env(installed_env(APP_MODE='onion'), 'APP_MODE must be clearnet or tor')
        self.check_env(installed_env(COOKIE_SECURE='TRUE'), 'COOKIE_SECURE must be exactly true or false')

    def test_check_takes_the_node_tor_overlay_only_after_tor_and_the_nodes(self):
        # A-442: compose.nodes-tor.yaml replaces the node commands, so it must follow compose.nodes.yaml (listed
        # before it, compose.nodes.yaml's commands would win and drop the proxy), and it is for Tor mode only.
        base = 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml'
        self.check_env(installed_env(COMPOSE_FILE=base + ':compose.nodes-tor.yaml',
                                     COMPOSE_PROFILES='internal-db,bitcoin', BITCOIN_RPC_PASSWORD=hex_value(48, 'rpc')))
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.nodes-tor.yaml:'
                                                  'compose.internal-db.yaml'))
        after = 'compose.nodes-tor.yaml must come after compose.nodes.yaml in COMPOSE_FILE'
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes-tor.yaml:compose.nodes.yaml:'
                                                  'compose.internal-db.yaml'), after)
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.internal-db.yaml:compose.nodes-tor.yaml'),
                       after)
        tor_only = 'compose.nodes-tor.yaml (local nodes over Tor) needs APP_MODE=tor and compose.tor.yaml'
        self.check_env(clearnet_env(COMPOSE_FILE='compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml:'
                                                 'compose.nodes-tor.yaml'), tor_only)
        # Each half of the Tor rule on its own (the APP_MODE rule above refuses these too, for its own reason).
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.nodes.yaml:compose.internal-db.yaml:compose.nodes-tor.yaml'),
                       tor_only)
        self.check_env(clearnet_env(COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml:'
                                                 'compose.nodes-tor.yaml'), tor_only)
        # Paths and separators as Compose takes them: by base name, split on ':' and ';' (A-485 NIT-2).
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml:compose.tor.yaml:./compose.nodes.yaml:compose.internal-db.yaml:'
                                                  './compose.nodes-tor.yaml'))
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml;compose.tor.yaml;compose.nodes.yaml;compose.internal-db.yaml;'
                                                  'compose.nodes-tor.yaml'))
        self.check_env(installed_env(COMPOSE_FILE='compose.yaml;compose.tor.yaml;./compose.nodes-tor.yaml;compose.nodes.yaml;'
                                                  'compose.internal-db.yaml'), after)

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

    def test_check_local_bitcoin_verifier_and_inactive_profile(self):
        password = hex_value(48, 'rpc')
        local = installed_env(COMPOSE_PROFILES='internal-db,bitcoin', BITCOIN_RPC_PASSWORD=password,
                              BITCOIN_RPC_URL='')
        self.check_env(local)
        self.check_env({**local, 'BITCOIN_RPC_AUTH': ''}, 'needs BITCOIN_RPC_AUTH')
        self.check_env({**local, 'BITCOIN_RPC_AUTH': password}, 'must be a marketplace salted HMAC verifier')
        self.check_env({**local, 'BITCOIN_RPC_AUTH': bitcoin_rpcauth('wrong')},
                       'does not match BITCOIN_RPC_PASSWORD')
        original = self.write_env(local)
        result = self.command('check', extra_env={'INSTALLER_TEST_OPENSSL_BAD_DGST': 'fail'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Could not check the local Bitcoin RPC verifier', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        # A disabled Bitcoin node requires no local verifier.
        self.check_env(clearnet_env(BITCOIN_RPC_AUTH=None))

    def test_check_refuses_postgres_password_mismatch(self):
        self.check_env(installed_env(POSTGRES_PASSWORD=hex_value(48, 'other')), 'POSTGRES_PASSWORD differs')
        self.check_env(installed_env(DATABASE_URL='postgres://market@database.example/market'),
                       'COMPOSE_PROFILES runs the internal database (internal-db) but DATABASE_URL points elsewhere')
        self.check_env(installed_env(COMPOSE_PROFILES=''), 'COMPOSE_PROFILES lacks internal-db')
        self.check_env(installed_env(COMPOSE_PROFILES='internal-db,bitcoin'), 'needs BITCOIN_RPC_PASSWORD')

    def test_check_decodes_the_database_url_password(self):
        # DATABASE_URL percent-encodes reserved characters; POSTGRES_PASSWORD holds them as they are.
        password = 'p@ss/w:rd%' + hex_value(32, 'encoded')
        encoded = 'p%40ss%2Fw%3Ard%25' + hex_value(32, 'encoded')
        url = f'postgres://opsecmkt:{encoded}@db:5432/opsecmkt?sslmode=disable'
        self.check_env(installed_env(DATABASE_URL=url, POSTGRES_PASSWORD=password))
        self.check_env(installed_env(DATABASE_URL=url, POSTGRES_PASSWORD=encoded), 'POSTGRES_PASSWORD differs')

    def test_check_refuses_mirror_profile_on_clearnet(self):
        self.check_env(clearnet_env(COMPOSE_PROFILES='internal-db,mirror'), 'mirror, which needs APP_MODE=tor')

    def test_check_refuses_containers_of_another_checkout_or_project(self):
        self.write_env(installed_env())
        other = tempfile.TemporaryDirectory(prefix='opsecmkt-other-checkout-')
        self.addCleanup(other.cleanup)
        result = self.command('check', extra_env={'INSTALLER_TEST_CONTAINERS': other.name})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(other.name, result.stderr)
        self.assertIn('Give this checkout its own COMPOSE_PROJECT_NAME', result.stderr)
        self.assertNotIn('compose config --quiet', self.calls())
        # A-135: the project name was changed under a running install.
        result = self.command('check', extra_env={'INSTALLER_TEST_CONTAINERS': '',
                                                  'INSTALLER_TEST_OTHER_CONTAINERS': f'opsecmkt-old|{self.root}'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('opsecmkt-old', result.stderr)
        self.assertIn('new, empty volumes', result.stderr)

    def test_check_explains_a_moved_checkout(self):
        # The containers' working directory no longer exists: most likely this checkout was moved from there.
        # A new project name would start on empty volumes; recreating the containers from here relabels them.
        values = installed_env()
        original = self.write_env(values)
        moved = str(self.root / 'moved-away' / 'opsecmkt')
        for arguments in (['check'], ['add-mirror'], ['upgrade', '--no-backup']):
            with self.subTest(arguments=arguments):
                result = self.command(*arguments, extra_env={'INSTALLER_TEST_CONTAINERS': moved})
                self.assertEqual(result.returncode, 1)
                self.assertIn(f'{moved} (it no longer exists)', result.stderr)
                self.assertIn('docker compose up -d --force-recreate', result.stderr)
                self.assertIn('keeps the volumes', result.stderr)
                self.assertNotIn('its own COMPOSE_PROJECT_NAME', result.stderr)
                self.assertNotIn('COMPOSE_PROJECT_NAME=', result.stderr)
                self.assertNotIn('compose config --quiet', self.calls())
                self.assertNotIn(UP, self.calls())
                if arguments[0] != 'upgrade':  # upgrade checks after adding keys; its copy of .env is kept
                    self.assertEqual((self.root / '.env').read_bytes(), original)
                for copy in self.pre_upgrade_copies():
                    copy.unlink()
        # A directory that still exists is another checkout, as before.
        result = self.command('check', extra_env={'INSTALLER_TEST_CONTAINERS': f'{moved}\n{self.root.parent}'})
        self.assertIn('Give this checkout its own COMPOSE_PROJECT_NAME', result.stderr)

    def test_check_warns_tor_with_secure_cookies(self):
        result = self.check_env(installed_env(COOKIE_SECURE='true'))
        self.assertIn('warning: APP_MODE=tor with COOKIE_SECURE=true', result.stderr)

    def test_check_warns_monero_escrow_relays_held_in_tor_mode(self):
        # A-515: with APP_MODE=tor and ESCROW_XMR=on the app holds Monero escrow relays unless COMPOSE_FILE has the
        # node Tor overlay or ESCROW_XMR_TOR_RELAY=clearnet-accepted; check warns (names, never values) and passes.
        warning = 'warning: APP_MODE=tor with ESCROW_XMR=on'
        base = 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml'
        wallet = 'http://escrow:held-relay-secret-0123456789@monero-escrow-wallet:18083'
        held = installed_env(ESCROW_XMR='on', ESCROW_XMR_WALLET_RPC_URL=wallet)
        result = self.check_env(held)
        self.assertIn(warning, result.stderr)
        for name in ('ESCROW_XMR_TOR_RELAY=clearnet-accepted', 'compose.nodes-tor.yaml', 'COMPOSE_FILE'):
            self.assertIn(name, result.stderr)
        self.assertNotIn('held-relay-secret', result.stdout + result.stderr)
        self.assertEqual(result.stderr.count(warning), 1)
        # Either condition, clearnet mode, or Monero escrow off: no warning.
        for values in (installed_env(ESCROW_XMR='on', COMPOSE_FILE=base + ':compose.nodes-tor.yaml'),
                       installed_env(ESCROW_XMR='on', ESCROW_XMR_TOR_RELAY='clearnet-accepted'),
                       installed_env(ESCROW_XMR='on', COMPOSE_FILE=base.replace(':', ';') + ';./compose.nodes-tor.yaml'),
                       clearnet_env(ESCROW_XMR='on'),
                       installed_env(ESCROW_XMR='off'),
                       installed_env()):
            with self.subTest(values={k: values.get(k) for k in ('APP_MODE', 'ESCROW_XMR', 'ESCROW_XMR_TOR_RELAY', 'COMPOSE_FILE')}):
                self.assertNotIn(warning, self.check_env(values).stderr)
        # Any other value is refused, as the app refuses it at startup; the value is never printed.
        result = self.check_env(installed_env(ESCROW_XMR='on', ESCROW_XMR_TOR_RELAY='yes-really'),
                                'ESCROW_XMR_TOR_RELAY must be blank or clearnet-accepted')
        self.assertNotIn('yes-really', result.stdout + result.stderr)

    def test_commands_need_an_env(self):
        for arguments in (['check'], ['status'], ['setup-token'], ['add-mirror'], ['upgrade', '--no-backup'],
                          ['add-scanner'], ['refresh-scanner']):
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

    def test_setup_token_is_retired_only_when_the_running_app_uses_app_secret_key(self):
        # .env has APP_SECRET_KEY, but the running app was started before it was added (or by an older
        # release that does not report its key root): its keys still come from SETUP_TOKEN, so it stays.
        values = installed_env()
        self.set_installed(True)
        for keyroot in ('setup_token', 'omit'):
            for arguments in (['status'], ['add-mirror'], ['upgrade', '--no-backup']):
                with self.subTest(keyroot=keyroot, arguments=arguments):
                    original = self.write_env(values)
                    result = self.command(*arguments, extra_env={'INSTALLER_TEST_KEYROOT': keyroot})
                    self.assertEqual(result.returncode, 0, result.stderr)
                    if arguments == ['status']:
                        self.assertEqual((self.root / '.env').read_bytes(), original)
                    else:  # add-mirror and upgrade change other keys
                        self.assertEqual(self.config()['SETUP_TOKEN'], values['SETUP_TOKEN'])
                    self.assertNotIn('Removed SETUP_TOKEN', result.stdout)
                    self.assert_no_secret_printed(result, values)
                    for copy in self.pre_upgrade_copies():
                        copy.unlink()
        result = self.command('status', extra_env={'INSTALLER_TEST_KEYROOT': 'setup_token'})
        self.assertIn('Setup: complete', result.stdout)
        self.assertIn('SETUP_TOKEN stays in .env', result.stdout)
        # The running app uses APP_SECRET_KEY: every command retires the token.
        for arguments in (['status'], ['add-mirror'], ['upgrade', '--no-backup']):
            with self.subTest(keyroot='app_secret_key', arguments=arguments):
                self.write_env(values)
                result = self.command(*arguments, extra_env={'INSTALLER_TEST_KEYROOT': 'app_secret_key'})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn('SETUP_TOKEN', self.config())
                self.assertIn('Removed SETUP_TOKEN from .env', result.stdout)

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
        for profiles, expected in (('internal-db,bitcoin,monero', 'internal-db,bitcoin,monero,mirror'),
                                   ('internal-db', 'internal-db,mirror'), ('', 'mirror')):
            with self.subTest(profiles=profiles):
                values = installed_env(COMPOSE_PROFILES=profiles, BITCOIN_RPC_PASSWORD=hex_value(48, 'rpc'))
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
        # --no-backup wins over an exported recipient.
        Path(str(self.log) + '.backup').unlink()
        self.write_env(alpha_env())
        result = self.command('upgrade', '--no-backup', extra_env={'AGE_RECIPIENT': 'age1testrecipient'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(Path(str(self.log) + '.backup').exists())
        for copy in self.pre_upgrade_copies():
            copy.unlink()
        # A failed backup stops the upgrade before .env changes.
        original = self.write_env(alpha_env())
        self.backup_stub(code=1)
        result = self.command('upgrade', extra_env={'AGE_RECIPIENT': 'age1testrecipient'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('backup failed', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        self.assertEqual(self.pre_upgrade_copies(), [])

    def test_upgrade_refuses_legacy_pooled_config_before_backup_or_mutation(self):
        cases = [
            {'BITCOIN_RPC_URL': 'http://legacy:secret@bitcoin:8332'},
            {'BITCOIN_WALLET': 'opsecmkt'},
            {'MONERO_RPC_URL': 'http://monero:18081'},
            {'MONERO_WALLET_RPC_URL': 'http://legacy:secret@monero-wallet:18083'},
            {'COMPOSE_PROFILES': 'internal-db,monero-wallet'},
        ]
        for overrides in cases:
            with self.subTest(overrides=overrides):
                original = self.write_env(alpha_env(**overrides))
                result = self.command('upgrade', extra_env={'AGE_RECIPIENT': 'age1testrecipient'})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Refusing:', result.stderr)
                self.assertEqual((self.root / '.env').read_bytes(), original)
                self.assertEqual(self.pre_upgrade_copies(), [])
                self.assertFalse(Path(str(self.log) + '.backup').exists())
                self.assertFalse((self.root / '.env.lock').exists())

    def test_upgrade_adds_local_bitcoin_verifier_without_password_rotation(self):
        password = hex_value(48, 'old-rpc')
        legacy = installed_env(COMPOSE_PROFILES=None, BITCOIN_RPC_PASSWORD=password,
                               BITCOIN_RPC_AUTH=None)
        self.write_env(legacy)
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assertIn('bitcoin', config['COMPOSE_PROFILES'].split(','))
        self.assertEqual(config['BITCOIN_RPC_PASSWORD'], password)
        self.assertEqual(config['BITCOIN_RPC_URL'], '')
        salt = config['BITCOIN_RPC_AUTH'].split(':', 1)[1].split('$', 1)[0]
        self.assertEqual(config['BITCOIN_RPC_AUTH'], bitcoin_rpcauth(password, salt))
        self.assertEqual(stat.S_IMODE((self.root / '.env').stat().st_mode), 0o600)
        self.assertIn('added BITCOIN_RPC_AUTH', result.stdout)
        self.assert_no_secret_printed(result, config)
        first = (self.root / '.env').read_bytes()
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), first)
        self.assertEqual(self.config()['BITCOIN_RPC_AUTH'], config['BITCOIN_RPC_AUTH'])
        # A manually changed password with stale verifier is refused without auto-rotation or node restart.
        stale = {**config, 'BITCOIN_RPC_PASSWORD': hex_value(48, 'new-rpc')}
        original = self.write_env(stale)
        result = self.upgrade()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('does not match BITCOIN_RPC_PASSWORD', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        self.assertNotIn(UP, self.calls())
        self.assert_no_secret_printed(result, stale)

    def test_upgrade_repairs_blank_local_verifier_and_failed_crypto_preserves_env(self):
        password = hex_value(48, 'old-rpc')
        base = installed_env(COMPOSE_PROFILES='internal-db,bitcoin', BITCOIN_RPC_PASSWORD=password,
                             BITCOIN_RPC_AUTH='')
        for key, value in (('INSTALLER_TEST_OPENSSL_BAD_SALT', 'empty'),
                           ('INSTALLER_TEST_OPENSSL_BAD_SALT', 'malformed'),
                           ('INSTALLER_TEST_OPENSSL_BAD_DGST', 'fail'),
                           ('INSTALLER_TEST_OPENSSL_BAD_DGST', 'malformed')):
            with self.subTest(key=key, value=value):
                original = self.write_env(base)
                result = self.upgrade(extra_env={key: value})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Could not generate the local Bitcoin RPC verifier', result.stderr)
                self.assertEqual((self.root / '.env').read_bytes(), original)
                self.assertNotIn(UP, self.calls())
                self.assert_no_secret_printed(result, base)
        self.write_env(base)
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assertEqual(config['BITCOIN_RPC_PASSWORD'], password)
        self.assertEqual(config['BITCOIN_RPC_URL'], '')
        self.assertEqual(config['BITCOIN_RPC_AUTH'], bitcoin_rpcauth(
            password, config['BITCOIN_RPC_AUTH'].split(':', 1)[1].split('$', 1)[0]))
        self.assert_no_secret_printed(result, config)

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
        for key in ('DATABASE_URL', 'POSTGRES_PASSWORD', 'APP_MODE', 'COOKIE_SECURE', 'COMPOSE_FILE'):
            self.assertEqual(config[key], values[key], key)
        self.assertEqual(config['MONERO_RPC_URL'], '')
        self.assertNotIn('AUDIT_SIGNING_KEY', config)  # secrets are not invented
        self.assertIn('AUDIT_SIGNING_KEY is not set', result.stdout)
        self.assertRegex(result.stdout, r'added, with the values Compose already used: [A-Z_, ]*PAYMENT_EXPIRY')
        self.assert_no_secret_printed(result, values)
        self.assertEqual(self.calls()[-3:], [UP, STATUS, 'compose up -d --wait --force-recreate tor'])

    def test_upgrade_takes_app_port_from_the_app_container(self):
        clearnet = dict(APP_MODE='clearnet', COMPOSE_FILE='compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml')
        for published, shell, expected in (('127.0.0.1:18093', None, '18093'), ('[::1]:18095', None, '18095'),
                                           ('127.0.0.1:18093', '18094', '18094'), ('', None, '8080')):
            with self.subTest(published=published, shell=shell):
                self.write_env(alpha_env(**clearnet))
                extra = {'INSTALLER_TEST_APP_PUBLISHED': published}
                if shell:
                    extra['APP_PORT'] = shell
                result = self.upgrade(extra_env=extra)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.config()['APP_PORT'], expected)
                self.assertIn(f'added APP_PORT ({expected}', result.stdout)
                self.assertEqual('compose port app 8080' in self.calls(), not shell)
                for copy in self.pre_upgrade_copies():
                    copy.unlink()
        # An answer that names no port is not used.
        self.write_env(alpha_env(**clearnet))
        result = self.upgrade(extra_env={'INSTALLER_TEST_APP_PUBLISHED': 'garbage'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.config()['APP_PORT'], '8080')

    def test_upgrade_external_database_backup_needs_its_url(self):
        values = installed_env(DATABASE_URL='postgres://market@database.example/market', COMPOSE_PROFILES='',
                               COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.external-egress.yaml')
        original = self.write_env(values)
        result = self.command('upgrade', extra_env={'AGE_RECIPIENT': 'age1testrecipient'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1', result.stderr)
        self.assertIn('./scripts/install.sh upgrade', result.stderr)
        self.assertFalse(Path(str(self.log) + '.backup').exists())
        self.assertEqual((self.root / '.env').read_bytes(), original)
        self.assertEqual(self.pre_upgrade_copies(), [])
        # With the URL given, backup.sh takes the backup of that database.
        result = self.command('upgrade', extra_env={'AGE_RECIPIENT': 'age1testrecipient',
                                                    'BACKUP_DATABASE_URL': 'postgres://market@database.example/market'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(Path(str(self.log) + '.backup').exists())
        # A failed backup names the variable too.
        for copy in self.pre_upgrade_copies():
            copy.unlink()
        self.write_env(values)
        self.backup_stub(code=1)
        result = self.command('upgrade', extra_env={'AGE_RECIPIENT': 'age1testrecipient',
                                                    'BACKUP_DATABASE_URL': 'postgres://market@database.example/market'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('BACKUP_DATABASE_URL', result.stderr)
        for path in ('UPGRADING.md', 'docs/operator-guide.md'):
            self.assertIn('BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade',
                          (REPOSITORY / path).read_text(), path)

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
        # CLAMAV_IMAGE_DEFAULT is installer bookkeeping, not a Compose setting shown in .env.example.
        self.assertEqual(sorted(set(config) - set(example)), ['CLAMAV_IMAGE_DEFAULT'])

    def test_upgrade_advances_recorded_clamav_default_and_preserves_custom_images(self):
        previous = 'clamav/clamav:1.5.4-debian'
        # Test-only sentinel models a later installer default; it makes no claim about upstream tag availability.
        current = 'clamav/clamav:99.99.99-debian'
        self.set_clamav_default(current)
        custom_tag = 'registry.example/security/clamav:1.4.6-debian'
        custom_digest = 'clamav/clamav@sha256:' + 'a' * 64
        cases = (
            ('managed default', previous, previous, current, current),
            ('custom tag', custom_tag, previous, custom_tag, current),
            ('custom digest', custom_digest, previous, custom_digest, current),
            ('disabled blank image', '', previous, '', current),
        )
        for name, image, marker, want_image, want_marker in cases:
            with self.subTest(name=name):
                self.write_env(alpha_env(CLAMAV_IMAGE=image, CLAMAV_IMAGE_DEFAULT=marker))
                result = self.upgrade()
                self.assertEqual(result.returncode, 0, result.stderr)
                config = self.config()
                self.assertEqual(config['CLAMAV_IMAGE'], want_image)
                self.assertEqual(config['CLAMAV_IMAGE_DEFAULT'], want_marker)
                self.assertNotIn('scanner', config['COMPOSE_PROFILES'].split(','))

    def test_upgrade_marks_only_a_known_current_markerless_clamav_default(self):
        default = 'clamav/clamav:1.5.4-debian'
        self.write_env(alpha_env(CLAMAV_IMAGE=default))
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.config()['CLAMAV_IMAGE_DEFAULT'], default)

        custom = 'registry.example/security/clamav:1.4.6-debian'
        self.write_env(alpha_env(CLAMAV_IMAGE=custom))
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assertEqual(config['CLAMAV_IMAGE'], custom)
        self.assertNotIn('CLAMAV_IMAGE_DEFAULT', config)

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
        defaults = installer_image_defaults()
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

    def test_upgrade_never_switches_network(self):
        # A-523 changes what init writes, never an existing install: upgrade keeps test-network and blank-network .env
        # files on their networks (a blank still means Compose's test-network fallback) and never writes the flag.
        password = hex_value(48, 'rpc')
        nodes = dict(COMPOSE_PROFILES='internal-db,bitcoin,monero', BITCOIN_RPC_PASSWORD=password)
        cases = (
            ('test networks', installed_env(**nodes, BITCOIN_CHAIN='signet', MONERO_NETWORK='stagenet'), 'signet', 'stagenet'),
            ('testnet4 with the flag', installed_env(**nodes, BITCOIN_CHAIN='testnet4', MONERO_NETWORK='stagenet',
                                                     MONERO_NETWORK_FLAG='--stagenet'), 'testnet4', 'stagenet'),
            ('blank networks', installed_env(**nodes), '', ''),
            ('absent networks', alpha_env(), '', ''),
            ('mainnet', installed_env(**nodes, BITCOIN_CHAIN='main', MONERO_NETWORK='mainnet', MONERO_NETWORK_FLAG=''),
             'main', 'mainnet'),
        )
        for name, values, chain, network in cases:
            with self.subTest(name=name):
                self.write_env(values)
                result = self.upgrade()
                self.assertEqual(result.returncode, 0, result.stderr)
                config = self.config()
                self.assertEqual(config['BITCOIN_CHAIN'], chain)
                self.assertEqual(config['MONERO_NETWORK'], network)
                self.assertEqual(config.get('MONERO_NETWORK_FLAG'), values.get('MONERO_NETWORK_FLAG'))
                self.assertEqual(config.get('BITCOIN_PRUNE_MB'), values.get('BITCOIN_PRUNE_MB', '2000'))
                self.assertEqual(config.get('PAYMENT_CONFIRMATIONS_BTC'), values.get('PAYMENT_CONFIRMATIONS_BTC', '3'))
                for copy in self.pre_upgrade_copies():
                    copy.unlink()

    def test_monero_network_flag_isolated(self):
        # A shell MONERO_NETWORK_FLAG would override .env for Compose (a blank one runs a mainnet daemon): every
        # command unsets it. upgrade never defaults it: absent means --stagenet, and a blank line means mainnet.
        source = INSTALLER.read_text()
        keys = re.search(r"^env_keys='([^']*)'", source, re.M)[1].split()
        self.assertIn('MONERO_NETWORK_FLAG', keys)
        defaults = re.search(r'^upgrade_defaults="([^"]*)"', source, re.M)[1]
        self.assertNotIn('MONERO_NETWORK_FLAG', defaults)
        stagenet = installed_env(COMPOSE_PROFILES='internal-db,monero', MONERO_NETWORK='stagenet')
        for shell in ('', '--testnet'):
            with self.subTest(shell=shell):
                self.check_env(stagenet, extra_env={'MONERO_NETWORK_FLAG': shell})
                self.write_env(stagenet)
                self.assertEqual(self.upgrade(extra_env={'MONERO_NETWORK_FLAG': shell}).returncode, 0)
                self.assertNotIn('MONERO_NETWORK_FLAG', self.config())
                for copy in self.pre_upgrade_copies():
                    copy.unlink()
                for line in Path(str(self.log) + '.env').read_text().splitlines():
                    self.assertNotIn('MONERO_NETWORK_FLAG', json.loads(line))
        (self.root / '.env').unlink()
        self.log.unlink()
        Path(str(self.log) + '.env').unlink()
        self.assert_started(self.run_installer(None, arguments=['--tor', '--nodes', 'local', '--bitcoin-network', 'signet',
                                                                '--monero-network', 'stagenet'],
                                               extra_env={'MONERO_NETWORK_FLAG': ''}))
        self.assertEqual(self.config()['MONERO_NETWORK_FLAG'], '--stagenet')
        for line in Path(str(self.log) + '.env').read_text().splitlines():
            self.assertNotIn('MONERO_NETWORK_FLAG', json.loads(line))

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

    # ---- malware scanner (A-184) ------------------------------------------------------------------------

    def assert_scanner_added(self, config, original_profiles, original_files):
        self.assertEqual(config['COMPOSE_PROFILES'], f'{original_profiles},scanner' if original_profiles else 'scanner')
        self.assertEqual(config['COMPOSE_FILE'], f'{original_files}:compose.scanner.yaml')
        self.assertEqual(config['CLAMD_ADDRESS'], 'clamd:3310')
        self.assertEqual(config['CLAMAV_IMAGE'], 'clamav/clamav:1.5.4-debian')

    def test_add_scanner_refuses_small_host(self):
        original = self.write_env(installed_env())
        for memory in (3 * 2**30 - 1, 512 * 2**20):
            with self.subTest(memory=memory):
                result = self.command('add-scanner', extra_env={'INSTALLER_TEST_MEMTOTAL': str(memory)})
                self.assertEqual(result.returncode, 1)
                self.assertIn('at least 3 GiB', result.stderr)
                self.assertIn(f'{memory // 2**20} MiB', result.stderr)
                self.assertIn('Nothing was changed', result.stderr)
                self.assertEqual((self.root / '.env').read_bytes(), original)
                self.assertEqual(self.calls(), ['compose version', MEMORY])
                self.assertFalse((self.root / '.env.lock').exists())
                self.assertEqual(list(self.root.glob('.env.edit.*')), [])
        # Docker cannot say: refused the same way.
        result = self.command('add-scanner', extra_env={'INSTALLER_TEST_INFO_FAIL': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('Could not read', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        # 3 GiB to 4 GiB: a warning, then the scanner.
        result = self.command('add-scanner', extra_env={'INSTALLER_TEST_MEMTOTAL': str(3 * 2**30)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('warning: this Docker host has 3072 MiB of memory', result.stderr)
        self.assert_scanner_added(self.config(), 'internal-db', installed_env()['COMPOSE_FILE'])

    def test_add_scanner_idempotent(self):
        for profiles in ('internal-db,bitcoin', ''):
            with self.subTest(profiles=profiles):
                values = installed_env(COMPOSE_PROFILES=profiles, BITCOIN_RPC_PASSWORD=hex_value(48, 'rpc'))
                if 'internal-db' not in profiles:
                    values['DATABASE_URL'] = 'postgres://market@database.example/market'
                # A comment, a CRLF line and an unrelated key stay byte for byte.
                original = self.write_env(env_text(values).encode() + b"# keep me\r\nLOG_DRIVER='local'")
                (self.root / '.env').chmod(0o640)
                (self.state / 'clamd-reads').unlink(missing_ok=True)
                result = self.command('add-scanner', extra_env={'INSTALLER_TEST_CLAMD_WAITS': '3'})
                self.assertEqual(result.returncode, 0, result.stderr)
                text = (self.root / '.env').read_bytes()
                config = {key: value[1:-1] for key, value in (line.split('=', 1) for line in text.decode().splitlines()
                                                              if not line.startswith('#'))}
                self.assert_scanner_added(config, profiles, values['COMPOSE_FILE'])
                self.assertTrue(text.startswith(original.split(b'COMPOSE_FILE=')[0]))
                self.assertIn(b"# keep me\r\nLOG_DRIVER='local'\n", text)
                self.assertEqual(stat.S_IMODE((self.root / '.env').stat().st_mode), 0o600)
                # Checked, started, waited for (three unanswered reads), then the app recreated with it.
                self.assertEqual(self.calls(), ['compose version', MEMORY, *CHECK_SCANNER, 'compose up -d clamd',
                                                *[CLAMD_READ] * 4, *UP_WAIT_SCANNER])
                self.assertIn(f'Signatures: {CLAMD_VERSION}', result.stdout)
                self.assertIn('refresh-scanner', result.stdout)
                self.assertEqual(list(self.root.glob('.env.edit.*')), [])
                self.assertFalse((self.root / '.env.lock').exists())
                self.assert_no_secret_printed(result, values)
                # Again: .env is left alone and clamd is only checked.
                after_first = (self.root / '.env').read_bytes()
                (self.state / 'clamd-reads').unlink()
                result = self.command('add-scanner')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((self.root / '.env').read_bytes(), after_first)
                self.assertIn('already', result.stdout)
                self.assertEqual(self.calls(), ['compose version', MEMORY, *CHECK_SCANNER,
                                                'compose up -d clamd', CLAMD_READ, *UP_WAIT_SCANNER])
                (self.root / '.env').unlink()

    def test_add_scanner_bounded_wait_names_the_logs(self):
        original_values = installed_env()
        self.write_env(original_values)
        # 120 stub calls (sleep is stubbed): allow for a slow interpreter start under load.
        result = self.command('add-scanner', extra_env={'INSTALLER_TEST_CLAMD_DOWN': '1'}, timeout=180)
        self.assertEqual(result.returncode, 1)
        self.assertIn('within 10 minutes', result.stderr)
        self.assertIn('docker compose logs clamd', result.stderr)
        # Bounded: 120 reads 5 seconds apart, and the app is not recreated.
        self.assertEqual(self.calls().count(CLAMD_READ), 120)
        self.assertNotIn('compose up -d --wait', self.calls())
        self.assertFalse((self.root / '.env.lock').exists())
        # A configuration the check refuses after the change is put back.
        original = self.write_env(original_values)
        (self.root / 'deploy' / 'clamd.conf').chmod(0o600)
        result = self.command('add-scanner')
        self.assertEqual(result.returncode, 1)
        self.assertIn('deploy/clamd.conf must be readable', result.stderr)
        self.assertIn('.env was not changed', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)
        self.assertNotIn('compose up -d clamd', self.calls())
        self.assertEqual(list(self.root.glob('.env.edit.*')), [])

    def test_add_scanner_completes_a_partial_configuration(self):
        # check refuses an address without the profile and points here; add-scanner must not refuse it too.
        self.write_env(installed_env(CLAMD_ADDRESS='clamd:3310'))
        result = self.command('add-scanner')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_scanner_added(self.config(), 'internal-db', installed_env()['COMPOSE_FILE'])

    def test_add_scanner_needs_compose_file(self):
        values = installed_env(COMPOSE_FILE=None)
        original = self.write_env(values)
        result = self.command('add-scanner')
        self.assertEqual(result.returncode, 1)
        self.assertIn('./scripts/install.sh upgrade', result.stderr)
        self.assertEqual((self.root / '.env').read_bytes(), original)

    def test_check_scanner_profile_needs_address(self):
        self.write_env(scanner_env())
        result = self.command('check')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls(), ['compose version', *CHECK_SCANNER])
        self.check_env(scanner_env(CLAMD_ADDRESS=''), 'the malware scanner (scanner profile) needs CLAMD_ADDRESS=clamd:3310')
        self.check_env(scanner_env(CLAMD_ADDRESS=None), 'needs CLAMD_ADDRESS=clamd:3310')
        self.check_env(installed_env(CLAMD_ADDRESS='clamd:3310'), 'CLAMD_ADDRESS is set but COMPOSE_PROFILES lacks scanner')
        self.check_env(scanner_env(CLAMD_ADDRESS='scanner.example:3310'), 'CLAMD_ADDRESS must be clamd:3310')
        self.check_env(scanner_env(COMPOSE_FILE=installed_env()['COMPOSE_FILE']),
                       'the scanner profile needs compose.scanner.yaml in COMPOSE_FILE')
        (self.root / 'deploy' / 'clamd.conf').chmod(0o600)
        self.check_env(scanner_env(), 'deploy/clamd.conf must be readable by every user')
        (self.root / 'deploy' / 'clamd.conf').chmod(0o644)
        # Off: a blank CLAMD_ADDRESS and no profile, with or without the key.
        self.check_env(installed_env(CLAMD_ADDRESS=''))

    def test_check_refuses_unpinned_clamav(self):
        for image in ('clamav/clamav:latest', 'clamav/clamav', 'clamav/clamav:1.5.4_base', 'clamav/clamav:stable_base'):
            with self.subTest(image=image):
                self.check_env(scanner_env(CLAMAV_IMAGE=image), 'CLAMAV_IMAGE must name a ClamAV release')
                # Also while the scanner is off: the key would be used as soon as it is turned on.
                self.check_env(installed_env(CLAMAV_IMAGE=image), 'CLAMAV_IMAGE must name a ClamAV release')
        self.write_env(scanner_env(CLAMAV_IMAGE='clamav/clamav@sha256:' + 'b' * 64))
        self.assertEqual(self.command('check').returncode, 0)
        for digest in ('sha256:abc', 'sha256:' + 'A' * 64, 'sha512:' + 'b' * 64,
                       'sha256:' + 'b' * 63, 'sha256:' + 'b' * 65,
                       'bad@sha256:' + 'b' * 64):
            with self.subTest(digest=digest):
                self.check_env(scanner_env(CLAMAV_IMAGE='clamav/clamav@' + digest),
                               'CLAMAV_IMAGE must name a ClamAV release')
        self.check_env(scanner_env(CLAMAV_IMAGE='@sha256:' + 'b' * 64),
                       'CLAMAV_IMAGE must name a ClamAV release')
        # The image Compose resolves (an override file can set one) is checked too.
        self.write_env(scanner_env())
        rendered = SCANNER_CONFIG.replace('image: clamav/clamav:1.5.4-debian', 'image: clamav/clamav:latest')
        result = self.command('check', extra_env={'INSTALLER_TEST_COMPOSE_CONFIG': rendered})
        self.assertEqual(result.returncode, 1)
        self.assertIn('clamd runs clamav/clamav:latest', result.stderr)

    def test_check_refuses_clamd_off_scanner_network(self):
        self.write_env(scanner_env())
        cases = [
            (SCANNER_CONFIG.replace('    networks:\n      scanner: null\n    read_only',
                                    '    networks:\n      backend: null\n      scanner: null\n    read_only'),
             'clamd must be on the scanner network only (it is on: backend scanner)'),
            (SCANNER_CONFIG.replace('    read_only: true\n    user', '    network_mode: host\n    read_only: true\n    user'),
             'clamd must not set network_mode'),
            (SCANNER_CONFIG.replace('    read_only: true\n    user', '    ports:\n      - target: 3310\n    read_only: true\n    user'),
             'clamd must publish no port'),
            (SCANNER_CONFIG.replace('  scanner:\n    name: opsecmkt_scanner\n    internal: true\n',
                                    '  scanner:\n    name: opsecmkt_scanner\n'),
             'the scanner network must be internal'),
            (SCANNER_CONFIG.replace('      backend: null\n      tor_egress: null\n', '      backend: null\n      scanner: null\n      tor_egress: null\n'),
             'only app and clamd may join the scanner network (it has: app clamd tor)'),
            (SCANNER_CONFIG.replace('        read_only: true\n        bind: {}\n',
                                    '        read_only: true\n        bind: {}\n      - type: volume\n        source: signatures\n'
                                    '        target: /var/lib/clamav\n        volume: {}\n'),
             'clamd must mount no volume'),
        ]
        for rendered, message in cases:
            with self.subTest(message=message):
                self.assertNotEqual(rendered, SCANNER_CONFIG)
                result = self.command('check', extra_env={'INSTALLER_TEST_COMPOSE_CONFIG': rendered})
                self.assertEqual(result.returncode, 1)
                self.assertIn(message, result.stderr)
                self.assertNotIn('Configuration OK', result.stdout)

    @unittest.skipUnless(shutil.which('docker') and subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode == 0,
                         'Docker Compose is not installed')
    def test_check_reads_real_compose_output(self):
        # The parser against what this machine's Compose prints for the real files, in both modes.
        for mode in ('tor', 'clearnet'):
            with self.subTest(mode=mode):
                files = f'compose.yaml:compose.{mode}.yaml:compose.nodes.yaml:compose.internal-db.yaml:compose.scanner.yaml'
                environment = {key: value for key, value in os.environ.items() if not key.startswith('COMPOSE_')}
                environment.update(COMPOSE_FILE=files, COMPOSE_PROFILES='internal-db,scanner,bitcoin,monero',
                                   DATABASE_URL='postgres://unused@db/unused',
                                   BITCOIN_RPC_AUTH=bitcoin_rpcauth('unused'))
                rendered = subprocess.run(['docker', 'compose', '--env-file', os.devnull, 'config'], cwd=REPOSITORY,
                                          env=environment, capture_output=True, text=True, check=True).stdout
                values = scanner_env(COMPOSE_FILE=files)
                if mode == 'clearnet':
                    values.update(APP_MODE='clearnet', COOKIE_SECURE='true')
                self.write_env(values)
                result = self.command('check', extra_env={'INSTALLER_TEST_COMPOSE_CONFIG': rendered})
                self.assertEqual(result.returncode, 0, result.stderr)
                broken = rendered.replace('    networks:\n      scanner: null\n    pids_limit: 256',
                                          '    networks:\n      scanner: null\n      backend: null\n    pids_limit: 256')
                self.assertNotEqual(broken, rendered)
                result = self.command('check', extra_env={'INSTALLER_TEST_COMPOSE_CONFIG': broken})
                self.assertIn('clamd must be on the scanner network only', result.stderr)

    def test_refresh_scanner_pulls_and_recreates(self):
        values = scanner_env()
        self.write_env(values)
        newer = 'ClamAV 1.5.4/28140/Thu Oct  1 07:20:00 2026'
        result = self.command('refresh-scanner', extra_env={'INSTALLER_TEST_PULLED_VERSION': newer})
        self.assertEqual(result.returncode, 0, result.stderr)
        # Pulled even though the tag is present, then clamd recreated from it (Compose recreates it when the
        # pull brought a newer build) and waited for.
        self.assertEqual(self.calls(), ['compose version', *CHECK_SCANNER, CLAMD_READ, 'compose pull clamd',
                                        'compose up -d clamd', CLAMD_READ])
        self.assertIn(f'Signatures before: {CLAMD_VERSION}', result.stdout)
        self.assertIn(f'Signatures now:    {newer}', result.stdout)
        self.assertEqual((self.root / '.env').read_bytes(), env_text(values).encode())
        # A failed pull changes nothing and says why.
        result = self.command('refresh-scanner', extra_env={'INSTALLER_TEST_PULL_FAIL': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('Could not pull', result.stderr)
        self.assertNotIn('compose up -d clamd', self.calls())
        # clamd stopped: refreshed all the same.
        (self.state / 'clamd-version').unlink()
        result = self.command('refresh-scanner', extra_env={'INSTALLER_TEST_CLAMD_STOPPED': '1'}, timeout=180)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('did not answer within 10 minutes', result.stderr)
        self.assertIn('Signatures before: (clamd not answering)', result.stdout)
        self.assertIn('compose pull clamd', self.calls())
        # Scanner off: nothing to refresh.
        self.write_env(installed_env())
        result = self.command('refresh-scanner')
        self.assertEqual(result.returncode, 1)
        self.assertIn('add-scanner', result.stderr)
        self.assertEqual(self.calls(), ['compose version'])

    def test_refresh_scanner_respects_env_lock(self):
        self.write_env(scanner_env())
        (self.root / '.env.lock').mkdir()
        result = self.command('refresh-scanner')
        self.assertEqual(result.returncode, 1)
        self.assertIn('Another ./scripts/install.sh command is changing .env', result.stderr)
        self.assertEqual(self.calls(), ['compose version'])
        self.assertTrue((self.root / '.env.lock').is_dir())

    def test_upgrade_does_not_enable_scanner(self):
        for values in (alpha_env(), installed_env(), clearnet_env()):
            with self.subTest(mode=values['APP_MODE'], alpha='SETUP_TOKEN' in values and 'APP_SECRET_KEY' not in values):
                self.write_env(values)
                result = self.upgrade(extra_env={'INSTALLER_TEST_MEMTOTAL': str(64 * 2**30)})
                self.assertEqual(result.returncode, 0, result.stderr)
                config = self.config()
                self.assertNotIn('scanner', config['COMPOSE_PROFILES'].split(','))
                self.assertNotIn('compose.scanner.yaml', config['COMPOSE_FILE'])
                self.assertEqual(config['CLAMD_ADDRESS'], '')
                self.assertEqual(config['CLAMAV_IMAGE'], 'clamav/clamav:1.5.4-debian')
                self.assertFalse([call for call in self.calls() if 'clamd' in call or call == MEMORY])
                for copy in self.pre_upgrade_copies():
                    copy.unlink()

    def test_upgrade_runs_refresh_scanner_only_when_profile_on(self):
        self.write_env(scanner_env())
        self.set_installed(False)
        newer = 'ClamAV 1.5.4/28140/Thu Oct  1 07:20:00 2026'
        result = self.upgrade(extra_env={'INSTALLER_TEST_PULLED_VERSION': newer})
        self.assertEqual(result.returncode, 0, result.stderr)
        # Last, once the upgrade itself is up and healthy.
        calls = self.calls()
        self.assertEqual(calls[-4:], [CLAMD_READ, 'compose pull clamd', 'compose up -d clamd', CLAMD_READ])
        self.assertLess(calls.index(UP_SCANNER[1]), calls.index('compose pull clamd'))
        self.assertNotIn(UP, calls)
        self.assertIn(f'Signatures now:    {newer}', result.stdout)
        # A failed pull does not fail the upgrade: it has already been applied.
        for copy in self.pre_upgrade_copies():
            copy.unlink()
        self.write_env(scanner_env())
        result = self.upgrade(extra_env={'INSTALLER_TEST_PULL_FAIL': '1'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('scanner signatures were not refreshed', result.stderr)
        self.assertIn('./scripts/install.sh refresh-scanner', result.stderr)
        self.assertIn('Upgrade complete', result.stdout)

    def test_upgrade_recreates_clamd_when_config_changed(self):
        self.write_env(scanner_env())
        self.set_installed(False)
        old_config = (self.root / 'deploy' / 'clamd.conf').read_text().replace('SelfCheck 600', 'SelfCheck 300')
        result = self.upgrade(extra_env={'INSTALLER_TEST_CLAMD_CONFIG': old_config})
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        config_check = calls.index(CLAMD_CONFIG_READ)
        force_recreate = calls.index('compose up -d --force-recreate clamd')
        pull = calls.index('compose pull clamd')
        self.assertLess(config_check, force_recreate)
        self.assertLess(force_recreate, pull)
        self.assertIn('configuration changed', result.stdout)
        self.assertNotIn('SelfCheck 300', result.stdout + result.stderr)

    def test_upgrade_does_not_force_recreate_clamd_when_config_matches(self):
        self.write_env(scanner_env())
        self.set_installed(False)
        result = self.upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        self.assertIn(CLAMD_CONFIG_READ, calls)
        self.assertNotIn('compose up -d --force-recreate clamd', calls)

    def test_upgrade_continues_if_clamd_config_recreate_fails(self):
        self.write_env(scanner_env())
        self.set_installed(False)
        old_config = (self.root / 'deploy' / 'clamd.conf').read_text().replace('SelfCheck 600', 'SelfCheck 300')
        result = self.upgrade(extra_env={'INSTALLER_TEST_CLAMD_CONFIG': old_config,
                                         'INSTALLER_TEST_CLAMD_RECREATE_FAIL': '1'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('warning: clamd could not be recreated to apply its configuration', result.stderr)
        self.assertIn('compose pull clamd', self.calls())
        self.assertIn('Upgrade complete', result.stdout)

    def test_status_prints_scanner(self):
        self.write_env(installed_env())
        result = self.command('status')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Malware scanner: off (photo uploads are off; ./scripts/install.sh add-scanner turns it on)', result.stdout)
        self.assertNotIn(CLAMD_READ, self.calls())
        self.write_env(scanner_env())
        result = self.command('status')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'Malware scanner: answering; signatures {CLAMD_VERSION}', result.stdout)
        for extra in ({'INSTALLER_TEST_CLAMD_DOWN': '1'}, {'INSTALLER_TEST_CLAMD_STOPPED': '1'}):
            with self.subTest(extra=extra):
                result = self.command('status', extra_env=extra)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('Malware scanner: not answering', result.stdout)
                self.assertIn('docker compose logs clamd', result.stdout)
                self.assertEqual(self.calls().count(CLAMD_READ), 1)

    def test_init_uploads(self):
        result = self.run_installer(None, arguments=['init', '--tor', '--nodes', 'none', '--uploads', 'yes'])
        self.assertEqual(result.returncode, 0, result.stderr)
        config = self.config()
        self.assert_scanner_added(config, 'internal-db', 'compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml')
        self.assertEqual(self.calls(), [*PREFLIGHT, MEMORY, *CHECK_SCANNER, *UP_SCANNER, f'compose exec -T tor {HOSTNAME}', CLAMD_READ])
        self.assertIn(f'Malware scanner: signatures {CLAMD_VERSION}', result.stdout)
        self.assert_token_only_in_final_block(result, config['SETUP_TOKEN'])
        # no, or no option: nothing about the scanner is written.
        for arguments in (['init', '--tor', '--nodes', 'none', '--uploads', 'no'], ['init', '--tor', '--nodes', 'none']):
            with self.subTest(arguments=arguments):
                (self.root / '.env').unlink()
                self.log.unlink()
                result = self.run_installer(None, arguments=arguments)
                self.assert_started(result)
                config = self.config()
                self.assertNotIn('CLAMD_ADDRESS', config)
                self.assertNotIn('scanner', config['COMPOSE_PROFILES'])
                self.assertNotIn('compose.scanner.yaml', config['COMPOSE_FILE'])
        # A small host is refused before anything is written.
        (self.root / '.env').unlink()
        self.log.unlink()
        result = self.run_installer(None, arguments=['init', '--tor', '--nodes', 'none', '--uploads', 'yes'],
                                    extra_env={'INSTALLER_TEST_MEMTOTAL': str(2 * 2**30)})
        self.assert_refused_before_writing(result, [*PREFLIGHT, MEMORY])
        self.assertIn('at least 3 GiB', result.stderr)
        # Bad values and --local are usage errors.
        for arguments in (['--uploads', 'maybe'], ['--uploads'], ['--local', '--uploads', 'yes']):
            with self.subTest(arguments=arguments):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(None, arguments=arguments)
                self.assertEqual(result.returncode, 2)
                self.assertFalse((self.root / '.env').exists())

    def test_unhealthy_clamd_fails_no_command(self):
        # Nothing depends on the scanner (ruling C1): a clamd that never becomes healthy (an OOM loop, a bad
        # upstream build) must not stop init, upgrade, add-mirror or setup-token part-way.
        broken = {'INSTALLER_TEST_CLAMD_UNHEALTHY': '1', 'INSTALLER_TEST_CLAMD_DOWN': '1'}
        result = self.run_installer(None, arguments=['init', '--tor', '--nodes', 'none', '--uploads', 'yes'],
                                    extra_env=broken, timeout=180)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Malware scanner: not answering after 10 minutes', result.stdout)
        self.assertIn('docker compose logs clamd', result.stdout)
        self.assertIn(f'Onion address: {MAIN_ONION}', result.stdout)
        self.assert_token_only_in_final_block(result, self.config()['SETUP_TOKEN'])
        self.assertEqual(self.calls()[len(PREFLIGHT) + 1 + len(CHECK_SCANNER):][:3], UP_SCANNER)
        # --admin: the administrator is created and the app recreated without the token.
        (self.root / '.env').unlink()
        self.log.unlink()
        self.set_installed(False)
        result = self.run_installer([PASSWORD, PASSWORD], extra_env=broken, timeout=180,
                                    arguments=['init', '--tor', '--nodes', 'none', '--uploads', 'yes', '--admin', 'alice'])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Administrator alice is created', result.stdout)
        self.assertNotIn('SETUP_TOKEN', self.config())
        self.assertEqual(self.calls().count(START_CLAMD), 2)
        # upgrade: the upgrade, the Tor recreate and the (failing) signature refresh all run.
        self.set_installed(True)
        self.write_env(scanner_env())
        result = self.command('upgrade', '--no-backup', extra_env=broken, timeout=180)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Upgrade complete', result.stdout)
        self.assertIn('compose up -d --wait --force-recreate tor', self.calls())
        self.assertIn('compose pull clamd', self.calls())
        self.assertIn('scanner signatures were not refreshed', result.stderr)
        self.assertNotIn(UP, self.calls())
        # add-mirror and setup-token --new finish too.
        self.write_env(scanner_env())
        result = self.command('add-mirror', extra_env=broken)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls()[len(CHECK_SCANNER) + 1:][:3], UP_WAIT_SCANNER)
        self.set_installed(False)
        self.write_env(scanner_env())
        result = self.command('setup-token', '--new', extra_env=broken)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Setup token: ', result.stdout)
        # Without the scanner nothing changes: one `up --wait` for every service.
        self.write_env(installed_env())
        result = self.command('add-mirror')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('compose up -d --wait', self.calls())
        self.assertNotIn(SERVICES, self.calls())

    def test_check_refuses_floating_clamav_tags(self):
        # Tags that move to new builds of other releases; only MAJOR.MINOR.PATCH (optionally -debian) or a digest.
        for image in ('clamav/clamav:stable-debian', 'clamav/clamav:latest-debian', 'clamav/clamav:unstable-debian',
                      'clamav/clamav:1.5-debian', 'clamav/clamav:1.5', 'clamav/clamav:unstable', 'clamav/clamav:1.5.4_base-debian',
                      'clamav/clamav:1.5.4_base@sha256:' + 'c' * 64):
            with self.subTest(image=image):
                self.check_env(installed_env(CLAMAV_IMAGE=image), 'CLAMAV_IMAGE must name a ClamAV release')
        for image in ('clamav/clamav:1.5.4', 'clamav/clamav:1.4.6-debian', 'registry.example:5000/clamav/clamav:1.5.4-debian',
                      'clamav/clamav:1.5.4-debian@sha256:' + 'd' * 64):
            with self.subTest(image=image):
                self.check_env(installed_env(CLAMAV_IMAGE=image))
        # The image Compose resolves follows the same rule.
        self.write_env(scanner_env())
        rendered = SCANNER_CONFIG.replace('image: clamav/clamav:1.5.4-debian', 'image: clamav/clamav:stable-debian')
        result = self.command('check', extra_env={'INSTALLER_TEST_COMPOSE_CONFIG': rendered})
        self.assertEqual(result.returncode, 1)
        self.assertIn('clamd runs clamav/clamav:stable-debian', result.stderr)

    # ---- invariants -------------------------------------------------------------------------------------

    def test_installer_makes_no_outbound_fetch(self):
        text = INSTALLER.read_text()
        for word in ('curl', 'wget', '/dev/tcp', '/dev/udp', 'docker pull', 'freshclam'):
            self.assertNotIn(word, text)
        # Docker pulls one image on the installer's behalf: the scanner's, for refresh-scanner (A-184, ruling C1).
        self.assertEqual(re.findall(r'.*compose pull.*', text), ['  docker compose pull clamd || {'])
        # No command runs git, nc or ssh (messages may mention `git pull` for the operator to run).
        for line in text.splitlines():
            self.assertNotRegex(line.split(' #')[0], r'''(^|[;&|(`]|\$\(|then|do)\s*(git|nc|ssh|scp)\s''', line)
        self.assertEqual(self.run_installer([''] * 12).returncode, 0)
        self.assertFalse(Path(str(self.log) + '.fetch').exists())

    def test_no_literal_password_pattern(self):
        # Secret scanners read KEY_PASSWORD='...' in a pattern as a hard-coded password.
        self.assertNotRegex(INSTALLER.read_text(), r"PASSWORD='")

    def test_compose_fallbacks_stay_test_networks(self):
        # A-523 writes mainnet explicitly; a blank or absent key keeps running test networks (A-513 precedent), so an
        # existing install never moves to mainnet by itself. (These files belong to other items; read only.)
        for name in ('compose.nodes.yaml', 'compose.nodes-tor.yaml'):
            text = (REPOSITORY / name).read_text()
            with self.subTest(name=name):
                self.assertIn('${BITCOIN_CHAIN:-signet}', text)
                self.assertIn('${MONERO_NETWORK_FLAG---stagenet}', text)

    def test_isolated_keys_cover_env_example(self):
        # Commands unset these so Compose reads .env alone; the installer also keeps one bookkeeping-only key.
        keys = re.search(r"^env_keys='([^']*)'", INSTALLER.read_text(), re.M)[1].split()
        self.assertIn('CLAMAV_IMAGE_DEFAULT', keys)
        # MONERO_NETWORK_FLAG is isolated but never listed in .env.example: a copied blank line would run a mainnet daemon.
        retired_internal = {'CLAMAV_IMAGE_DEFAULT', 'MONERO_WALLET_RPC_PASSWORD', 'MONERO_NETWORK_FLAG'}
        self.assertEqual(sorted(key for key in keys if key not in retired_internal), sorted(example_values()))


if __name__ == '__main__':
    unittest.main()
