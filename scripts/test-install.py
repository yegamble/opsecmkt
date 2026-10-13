#!/usr/bin/env python3
"""Exercise the real local installer and HTTP setup wizard in disposable Compose projects.

Requires Docker/Compose, Python 3, Bash and OpenSSL. No existing .env or application data is used. The
installer prints the setup token (that is its job); the evidence log gets its output with every generated
secret redacted, so it holds no setup token, database URL or password.
"""
import http.cookiejar
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[1]
ADMIN_PASSWORD = 'terminal-bootstrap-test-password'


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(command, directory, environment, log, timeout=120, stdin=None):
    """Bound the whole process group, including the installer's Docker child."""
    with subprocess.Popen(command, cwd=directory, env=environment, stdin=stdin or subprocess.DEVNULL,
                          stdout=log, stderr=subprocess.STDOUT, start_new_session=True) as process:
        try:
            return process.wait(timeout=timeout)
        except BaseException:
            try:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=10)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait()
            raise


def env_values(directory):
    """The installer's single-quoted .env values."""
    path = directory / '.env'
    return dict(re.findall(r"^([A-Z0-9_]+)='([^']*)'$", path.read_text(), re.M)) if path.exists() else {}


def installer(directory, environment, log_path, arguments, timeout=900, stdin_text=None):
    """Run scripts/install.sh; return (exit code, its output). The output goes to the evidence log with every
    secret in .env (before and after the run) replaced by its key name."""
    private = directory.parent / f'installer-{uuid.uuid4().hex}.out'
    before = env_values(directory)
    stdin = None
    if stdin_text is not None:
        answers = directory.parent / f'installer-{uuid.uuid4().hex}.in'
        answers.write_text(stdin_text)
        stdin = answers.open()
    try:
        with private.open('w') as output:
            code = run(['bash', 'scripts/install.sh', *arguments], directory, environment, output, timeout, stdin)
    finally:
        if stdin:
            stdin.close()
            answers.unlink()
    text = private.read_text()
    private.unlink()
    redacted = text
    for values in (before, env_values(directory)):
        for key, value in values.items():
            secret = key in ('DATABASE_URL', 'APP_SECRET_KEY', 'AUDIT_SIGNING_KEY') or key.endswith(
                ('_PASSWORD', '_TOKEN', '_RPC_URL'))
            if secret and value:
                redacted = redacted.replace(value, f'<{key}>')
    with log_path.open('a') as log:
        log.write(f'$ install.sh {" ".join(arguments)} (exit {code})\n{redacted}')
    return code, text


def wait_for(check, description, seconds=90):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(1)
    raise RuntimeError(description)


def containers(directory, environment):
    """The IDs of the Compose project's containers; a recreated container gets a new ID."""
    listed = subprocess.run(['docker', 'compose', 'ps', '-aq'], cwd=directory, env=environment,
                            capture_output=True, text=True, timeout=60, check=True)
    return sorted(listed.stdout.split())


def expect_status(client, url, status, data=None):
    try:
        with client.open(url, data=data, timeout=5) as response:
            actual = response.status
    except urllib.error.HTTPError as error:
        actual = error.code
        error.close()
    require(actual == status, f'Expected HTTP {status}, received {actual} for {url}')


class Browser:
    """A script-free browser: a cookie jar and form posts, loopback only."""

    def __init__(self, base):
        self.base = base
        # Disable inherited HTTP proxies for these loopback-only checks.
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                                  urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def page(self, path):
        with self.client.open(self.base + path, timeout=5) as response:
            return response.read().decode()

    def csrf(self, path):
        match = re.search(r'name="csrf"\s+value="([^"]+)"', self.page(path))
        require(match, f'{path} has no CSRF token')
        return match[1]

    def post(self, path, fields, timeout=10):
        with self.client.open(self.base + path, data=urllib.parse.urlencode(fields).encode(), timeout=timeout) as response:
            return urllib.parse.urlsplit(response.url).path


def app_status_line(directory, environment):
    """The running app's -status line: installed=... schema=NNN keyroot=... (no secret)."""
    return subprocess.run(['docker', 'compose', 'exec', '-T', 'app', '/app/server', '-status'], cwd=directory,
                          env=environment, capture_output=True, text=True, timeout=120, check=True).stdout.strip()


def compose_logs(directory, environment):
    return subprocess.run(['docker', 'compose', 'logs', '--no-color'], cwd=directory, env=environment,
                          capture_output=True, text=True, timeout=120, check=True).stdout


def exercise(repo, environment, second, second_environment, third, third_environment, log_path):
    result = {}
    code, output = installer(repo, environment, log_path, ['--local'])
    require(code == 0, 'Local installer failed')
    browser = Browser('http://127.0.0.1:' + environment['APP_PORT'])
    wait_for(lambda: browser.page('/healthz') == 'ok', 'Application did not become healthy')
    configuration = env_values(repo)
    token = configuration['SETUP_TOKEN']
    # The installer printed the token from .env once, in its final block, with the setup URL; nothing else secret.
    printed = re.findall(r'^Setup token: ([0-9a-f]{64})$', output, re.M)
    require(printed == [token], 'The installer did not print the setup token from .env exactly once')
    require(browser.base + '/setup' in output, 'Installer printed the wrong setup URL')
    for key in ('APP_SECRET_KEY', 'POSTGRES_PASSWORD', 'AUDIT_SIGNING_KEY', 'DATABASE_URL'):
        require(configuration[key] not in output, f'The installer printed {key}')
    require(configuration['APP_SECRET_KEY'] != token, 'APP_SECRET_KEY and SETUP_TOKEN are the same value')
    code, output = installer(repo, environment, log_path, ['status'], 120)
    require(code == 0 and 'Setup: pending' in output, 'status did not report setup pending')
    fields = dict(csrf=browser.csrf('/setup'), token='wrong-token', handle='wizard_admin',
                  password='local-wizard-test-password', site_name='Wizard Review Market')
    expect_status(browser.client, browser.base + '/setup', 403, urllib.parse.urlencode(fields).encode())
    fields['token'] = token  # the printed token
    require(browser.post('/setup', fields) == '/admin', 'Setup with the printed token did not lead to admin')
    admin = browser.page('/admin')
    require('Get your marketplace ready' in admin, 'Admin onboarding is missing')
    require(re.search(r'<title>[^<]*Wizard Review Market', admin), 'Configured site title is missing')
    expect_status(browser.client, browser.base + '/setup', 403)
    logs = compose_logs(repo, environment)
    for key in ('SETUP_TOKEN', 'APP_SECRET_KEY', 'POSTGRES_PASSWORD'):
        require(configuration[key] not in logs, f'{key} appears in docker compose logs')
    # The app reports its key root (APP_SECRET_KEY, which init wrote), and so status may retire the token.
    line = app_status_line(repo, environment)
    require(re.fullmatch(r'installed=true schema=\d{3} keyroot=app_secret_key', line), f'unexpected -status line: {line}')
    for key in ('SETUP_TOKEN', 'APP_SECRET_KEY', 'POSTGRES_PASSWORD'):
        require(configuration[key] not in line, f'-status printed {key}')
    # status reports the completed setup and retires the token; setup-token then has nothing to show.
    code, output = installer(repo, environment, log_path, ['status'], 120)
    require(code == 0 and 'Setup: complete' in output, 'status did not report setup complete')
    require('SETUP_TOKEN' not in env_values(repo), 'status left SETUP_TOKEN in .env after setup')
    code, output = installer(repo, environment, log_path, ['setup-token'], 120)
    require(code == 1 and token not in output, 'setup-token did not refuse after setup')
    configuration = (repo / '.env').read_bytes()
    code, _ = installer(repo, environment, log_path, ['--local'], 30)
    require(code != 0, 'Installer accepted an existing configuration')
    require((repo / '.env').read_bytes() == configuration, 'Installer changed existing configuration')
    with log_path.open('a') as log:
        require(run(['docker', 'compose', 'up', '-d', '--wait'], repo, environment, log, 180) == 0,
                'Recreating the app without SETUP_TOKEN failed')
        require(run(['docker', 'compose', 'restart', 'app'], repo, environment, log) == 0, 'Application restart failed')
    wait_for(lambda: 'Get your marketplace ready' in browser.page('/admin'),
             'Admin session or installation did not survive restart')
    expect_status(browser.client, browser.base + '/setup', 403)
    result.update(installer='passed', printed_token_opens_setup='passed', token_absent_from_compose_logs='passed',
                  wrong_setup_token='rejected', admin_onboarding='passed', configured_title='passed',
                  status_pending_then_complete='passed', status_keyroot='app_secret_key', setup_token_after_install='exit 1',
                  existing_configuration='preserved', restart_admin_session='passed')

    # A second checkout on the same host. Under the same project name it is refused before writing anything;
    # under its own name, `init --admin` creates the first administrator from the terminal (no token), and its
    # `down --volumes` leaves the first installation alone.
    first_containers = containers(repo, environment)
    same_project = dict(environment, APP_PORT=second_environment['APP_PORT'])
    code, output = installer(second, same_project, log_path, ['--local'], 60)
    require(code != 0, 'A second checkout was installed under the same Compose project name')
    require(not (second / '.env').exists(), 'The refused second checkout wrote a configuration')
    require('already has containers created from another directory' in output
            and (str(repo) in output or str(repo.resolve()) in output), 'The refusal did not name the first checkout')
    code, output = installer(second, second_environment, log_path,
                             ['--local', '--admin', 'terminal_admin', '--site-name', 'Terminal Market'], 900,
                             stdin_text=f'{ADMIN_PASSWORD}\n{ADMIN_PASSWORD}\n')
    require(code == 0, 'init --admin failed for a second checkout with its own COMPOSE_PROJECT_NAME')
    require(ADMIN_PASSWORD not in output and 'Setup token' not in output, 'init --admin printed a secret')
    second_values = env_values(second)
    require('SETUP_TOKEN' not in second_values, 'init --admin left SETUP_TOKEN in .env')
    require(ADMIN_PASSWORD not in (second / '.env').read_text(), 'The administrator password reached .env')
    require(second_values['COMPOSE_PROJECT_NAME'] == second_environment['COMPOSE_PROJECT_NAME'],
            'The project name was not saved in .env')
    second_browser = Browser('http://127.0.0.1:' + second_environment['APP_PORT'])
    wait_for(lambda: second_browser.page('/healthz') == 'ok', 'The second installation did not become healthy')
    expect_status(second_browser.client, second_browser.base + '/setup', 403)
    # No COMPOSE_PROJECT_NAME in the environment: the saved name alone must select the second project.
    unnamed = {key: value for key, value in second_environment.items() if key != 'COMPOSE_PROJECT_NAME'}
    with log_path.open('a') as log:
        # Migration 010 turns the CAPTCHA on; switch it off as an operator would, so this script can sign in.
        require(run(['docker', 'compose', 'exec', '-T', 'db', 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-U', 'opsecmkt',
                     '-d', 'opsecmkt', '-c', "UPDATE settings SET value='false' WHERE key='captcha_required'"],
                    second, unnamed, log) == 0, 'Could not turn the CAPTCHA off')
    signed_in = second_browser.post('/login', dict(csrf=second_browser.csrf('/login'), handle='terminal_admin',
                                                   password=ADMIN_PASSWORD))
    require(signed_in != '/login', 'The terminal-created administrator could not sign in')
    admin = second_browser.page('/admin')
    require(re.search(r'<title>[^<]*Terminal Market', admin), 'The terminal-created site name is missing')
    logs = compose_logs(second, unnamed)
    require(ADMIN_PASSWORD not in logs, 'The administrator password appears in docker compose logs')
    with log_path.open('a') as log:
        require(run(['docker', 'compose', 'down', '--volumes', '--remove-orphans'], second, unnamed, log) == 0,
                'Second checkout cleanup failed')
    require(containers(repo, environment) == first_containers, 'The second checkout recreated the first one')
    wait_for(lambda: 'Get your marketplace ready' in browser.page('/admin'),
             'The first installation did not survive a second checkout')
    expect_status(browser.client, browser.base + '/setup', 403)
    result.update(second_checkout_same_project='refused', init_admin_terminal_bootstrap='passed',
                  init_admin_no_token_no_password='passed', second_checkout_own_project_down_volumes='first_intact')

    # A third checkout runs from an .env in the v0.1.0-alpha.1 layout (SETUP_TOKEN as the only key root, no
    # APP_SECRET_KEY, APP_PORT or COMPOSE_PROJECT_NAME, a :latest wallet image). After setup, `upgrade` must
    # keep the signed-in session and the CSRF token issued before it.
    password = uuid.uuid4().hex + uuid.uuid4().hex[:16]
    alpha_token = uuid.uuid4().hex + uuid.uuid4().hex
    (third / '.env').write_text(
        f"DATABASE_URL='postgres://opsecmkt:{password}@db:5432/opsecmkt?sslmode=disable'\n"
        f"POSTGRES_PASSWORD='{password}'\nSETUP_TOKEN='{alpha_token}'\nAPP_MODE='clearnet'\nCOOKIE_SECURE='false'\n"
        "MONERO_WALLET_IMAGE='ghcr.io/sethforprivacy/simple-monero-wallet-rpc:latest'\n"
        "COMPOSE_FILE='compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml'\n"
        "COMPOSE_PROFILES='internal-db'\n")
    (third / '.env').chmod(0o600)
    with log_path.open('a') as log:
        require(run(['docker', 'compose', 'up', '-d', '--build', '--wait'], third, third_environment, log, 900) == 0,
                'The alpha-layout installation did not start')
    third_browser = Browser('http://127.0.0.1:' + third_environment['APP_PORT'])
    require(third_browser.post('/setup', dict(csrf=third_browser.csrf('/setup'), token=alpha_token, handle='alpha_admin',
                                              password='alpha-layout-test-password', site_name='Alpha Layout Market'))
            == '/admin', 'Setup on the alpha-layout installation failed')
    csrf_before = third_browser.csrf('/admin')
    code, output = installer(third, third_environment, log_path, ['upgrade'], 60)
    require(code == 1 and 'AGE_RECIPIENT' in output, 'upgrade ran without a backup or --no-backup')
    # Without APP_PORT in the shell, upgrade records the port the running app container publishes.
    unset_port = {key: value for key, value in third_environment.items() if key != 'APP_PORT'}
    code, output = installer(third, unset_port, log_path, ['upgrade', '--no-backup'], 900)
    require(code == 0, 'upgrade failed on the alpha-layout installation')
    require(alpha_token not in output and password not in output, 'upgrade printed a secret')
    upgraded = env_values(third)
    require(upgraded.get('APP_SECRET_KEY') == alpha_token, 'upgrade did not copy SETUP_TOKEN to APP_SECRET_KEY')
    require('SETUP_TOKEN' not in upgraded, 'upgrade kept SETUP_TOKEN after setup')
    require(upgraded.get('COMPOSE_PROJECT_NAME') == third_environment['COMPOSE_PROJECT_NAME'],
            "upgrade did not record the project its containers use")
    require(upgraded.get('APP_PORT') == third_environment['APP_PORT'], 'upgrade did not record the running port')
    # A-523: default node images are pinned by tag and digest.
    require(re.fullmatch(r'ghcr\.io/sethforprivacy/simple-monero-wallet-rpc:v0\.18\.5\.1@sha256:[0-9a-f]{64}',
                         upgraded.get('MONERO_WALLET_IMAGE', '')) is not None, 'upgrade did not pin the wallet image')
    require(len(list(third.glob('.env.pre-upgrade-*'))) == 1, 'upgrade wrote no pre-upgrade copy')
    wait_for(lambda: 'Get your marketplace ready' in third_browser.page('/admin'),
             'The session did not survive the upgrade')
    require(third_browser.csrf('/admin') == csrf_before, 'The CSRF token changed across the upgrade')
    require(third_browser.post('/logout', dict(csrf=csrf_before)) == '/login',
            'The CSRF token issued before the upgrade was refused')
    after = (third / '.env').read_bytes()
    code, output = installer(third, third_environment, log_path, ['upgrade', '--no-backup'], 900)
    require(code == 0 and '.env is already up to date.' in output, 'A second upgrade was not a no-op')
    require((third / '.env').read_bytes() == after and len(list(third.glob('.env.pre-upgrade-*'))) == 1,
            'A second upgrade changed .env')
    result.update(upgrade_requires_backup_choice='passed', upgrade_alpha_layout='passed',
                  upgrade_keeps_session_and_csrf='passed', upgrade_second_run='no-op')
    return result


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def copy_inputs(repo):
    # Copy only deployment/build inputs. This excludes .env, Git/Claude worktrees,
    # node_modules, reports, backups and other local artifacts by construction.
    repo.mkdir()
    for name in ('Dockerfile', '.dockerignore', 'go.mod', 'go.sum'):
        shutil.copy2(ROOT / name, repo / name)
    for compose in ROOT.glob('compose*.yaml'):
        shutil.copy2(compose, repo / compose.name)
    for name in ('cmd', 'internal', 'web', 'scripts'):
        shutil.copytree(ROOT / name, repo / name, ignore=shutil.ignore_patterns(
            '.env', '.env.*', '*.dump', '*.age', '__pycache__', '*.test', '.DS_Store'))


def main():
    for executable in ('docker', 'bash', 'openssl'):
        require(shutil.which(executable), 'Missing executable: ' + executable)
    project = 'opsecmkt-install-test-' + uuid.uuid4().hex[:12]
    logs = ROOT / 'artifacts' / 'install-test' / project
    logs.mkdir(parents=True, mode=0o700)
    log_path = logs / 'install.log'
    environment = {key: value for key, value in os.environ.items()
                   if key in ('PATH', 'HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG')}
    second_environment = dict(environment, COMPOSE_PROJECT_NAME=project + '-second', APP_PORT=str(free_port()))
    third_environment = dict(environment, COMPOSE_PROJECT_NAME=project + '-alpha', APP_PORT=str(free_port()))
    environment.update(COMPOSE_PROJECT_NAME=project, APP_PORT=str(free_port()))
    failure = None
    cleanup_failures = []
    result = None
    with tempfile.TemporaryDirectory(prefix=project + '-') as temporary:
        repo, second, third = (Path(temporary) / name for name in ('repo', 'second', 'third'))
        for directory in (repo, second, third):
            copy_inputs(directory)
        try:
            result = exercise(repo, environment, second, second_environment, third, third_environment, log_path)
        except BaseException as error:
            failure = error
        finally:
            with log_path.open('a') as log:
                # Every cleanup is attempted even if another one times out or fails.
                for directory, env in ((third, third_environment), (second, second_environment), (repo, environment)):
                    commands = [['docker', 'compose', 'down', '--volumes', '--remove-orphans'],
                                ['docker', 'image', 'rm', env['COMPOSE_PROJECT_NAME'] + '-app']]
                    for command in commands:
                        if command[1] == 'compose' and not (directory / '.env').exists():
                            continue
                        try:
                            code = run(command, directory, env, log, 120)
                            # No image exists if installation failed before building.
                            if code and command[1] == 'compose':
                                cleanup_failures.append('Compose cleanup failed')
                        except BaseException as error:
                            cleanup_failures.append(type(error).__name__ + ' during cleanup')
    print('Installer test log: ' + str(log_path))
    if failure:
        raise RuntimeError('Installer test failed: ' + str(failure)) from failure
    require(not cleanup_failures, '; '.join(cleanup_failures))
    (logs / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Local installer, printed setup token, status and setup-token, terminal bootstrap (init --admin), '
          'second-checkout isolation and upgrade from an alpha-layout .env passed.')


def interrupted(_signal, _frame):
    raise KeyboardInterrupt('Installer test interrupted')


if __name__ == '__main__':
    # Ensure hosted-runner cancellation enters the same cleanup path as Ctrl-C.
    signal.signal(signal.SIGTERM, interrupted)
    main()
