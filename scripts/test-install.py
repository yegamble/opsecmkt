#!/usr/bin/env python3
"""Exercise the real local installer and HTTP setup wizard in a disposable Compose project.

Requires Docker/Compose, Python 3, Bash, OpenSSL and curl. No existing .env or
application data is used. Logs contain no generated setup token or database URL.
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


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(command, directory, environment, log, timeout=120):
    """Bound the whole process group, including the installer's Docker child."""
    with subprocess.Popen(command, cwd=directory, env=environment, stdin=subprocess.DEVNULL,
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


def exercise(repo, environment, second, second_environment, log_path):
    with log_path.open('a') as log:
        require(run(['bash', 'scripts/install.sh', '--local'], repo, environment, log, 900) == 0,
                'Local installer failed')
    base = 'http://127.0.0.1:' + environment['APP_PORT']
    # Disable inherited HTTP proxies for these loopback-only checks.
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def page(path):
        with client.open(base + path, timeout=5) as response:
            return response.read().decode()

    wait_for(lambda: page('/healthz') == 'ok', 'Application did not become healthy')
    configuration = (repo / '.env').read_bytes()
    token = re.search(r"^SETUP_TOKEN='([^']+)'$", configuration.decode(), re.M)[1]
    csrf = re.search(r'name="csrf"\s+value="([^"]+)"', page('/setup'))[1]
    fields = dict(csrf=csrf, token='wrong-token', handle='wizard_admin',
                  password='local-wizard-test-password', site_name='Wizard Review Market')
    expect_status(client, base + '/setup', 403, urllib.parse.urlencode(fields).encode())
    fields['token'] = token
    with client.open(base + '/setup', data=urllib.parse.urlencode(fields).encode(), timeout=10) as response:
        require(urllib.parse.urlsplit(response.url).path == '/admin', 'Setup did not lead to admin')
    admin = page('/admin')
    require('Get your marketplace ready' in admin, 'Admin onboarding is missing')
    require(re.search(r'<title>[^<]*Wizard Review Market', admin), 'Configured site title is missing')
    require(base + '/setup' in log_path.read_text(), 'Installer printed the wrong setup URL')
    require(token not in log_path.read_text(), 'Installer unexpectedly printed the setup token')
    expect_status(client, base + '/setup', 403)
    with log_path.open('a') as log:
        require(run(['bash', 'scripts/install.sh', '--local'], repo, environment, log, 30) != 0,
                'Installer accepted an existing configuration')
        require((repo / '.env').read_bytes() == configuration, 'Installer changed existing configuration')
        require(run(['docker', 'compose', 'restart', 'app'], repo, environment, log) == 0,
                'Application restart failed')
    wait_for(lambda: 'Get your marketplace ready' in page('/admin'),
             'Admin session or installation did not survive restart')
    expect_status(client, base + '/setup', 403)
    # A second checkout on the same host. Under the same project name it is refused before writing
    # anything; under its own name it installs, and its `down --volumes` leaves this installation alone.
    first_containers = containers(repo, environment)
    same_project = dict(environment, APP_PORT=second_environment['APP_PORT'])
    with log_path.open('a') as log:
        require(run(['bash', 'scripts/install.sh', '--local'], second, same_project, log, 60) != 0,
                'A second checkout was installed under the same Compose project name')
    require(not (second / '.env').exists(), 'The refused second checkout wrote a configuration')
    text = log_path.read_text()
    require('already has containers created from another directory' in text
            and (str(repo) in text or str(repo.resolve()) in text), 'The refusal did not name the first checkout')
    with log_path.open('a') as log:
        require(run(['bash', 'scripts/install.sh', '--local'], second, second_environment, log, 900) == 0,
                'Installer failed for a second checkout with its own COMPOSE_PROJECT_NAME')
        saved = "COMPOSE_PROJECT_NAME='" + second_environment['COMPOSE_PROJECT_NAME'] + "'"
        require(saved in (second / '.env').read_text().splitlines(), 'The project name was not saved in .env')
        # No COMPOSE_PROJECT_NAME in the environment: the saved name alone must select the second project.
        unnamed = {key: value for key, value in second_environment.items() if key != 'COMPOSE_PROJECT_NAME'}
        require(run(['docker', 'compose', 'down', '--volumes', '--remove-orphans'], second, unnamed, log) == 0,
                'Second checkout cleanup failed')
    require(containers(repo, environment) == first_containers, 'The second checkout recreated the first one')
    wait_for(lambda: 'Get your marketplace ready' in page('/admin'),
             'The first installation did not survive a second checkout')
    expect_status(client, base + '/setup', 403)
    return dict(installer='passed', wrong_setup_token='rejected', setup='passed',
                admin_onboarding='passed', configured_title='passed', setup_lockout='passed',
                existing_configuration='preserved', restart_admin_session='passed',
                second_checkout_same_project='refused', second_checkout_own_project_down_volumes='first_intact')


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
    for executable in ('docker', 'bash', 'openssl', 'curl'):
        require(shutil.which(executable), 'Missing executable: ' + executable)
    project = 'opsecmkt-install-test-' + uuid.uuid4().hex[:12]
    logs = ROOT / 'artifacts' / 'install-test' / project
    logs.mkdir(parents=True, mode=0o700)
    log_path = logs / 'install.log'
    environment = {key: value for key, value in os.environ.items()
                   if key in ('PATH', 'HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG')}
    second_environment = dict(environment, COMPOSE_PROJECT_NAME=project + '-second', APP_PORT=str(free_port()))
    environment.update(COMPOSE_PROJECT_NAME=project, APP_PORT=str(free_port()))
    failure = None
    cleanup_failures = []
    result = None
    with tempfile.TemporaryDirectory(prefix=project + '-') as temporary:
        repo, second = Path(temporary) / 'repo', Path(temporary) / 'second'
        copy_inputs(repo)
        copy_inputs(second)
        try:
            result = exercise(repo, environment, second, second_environment, log_path)
        except BaseException as error:
            failure = error
        finally:
            with log_path.open('a') as log:
                # Every cleanup is attempted even if another one times out or fails.
                for directory, env in ((second, second_environment), (repo, environment)):
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
    print('Local installer, setup authorization, admin onboarding, restart persistence and '
          'second-checkout isolation passed.')


def interrupted(_signal, _frame):
    raise KeyboardInterrupt('Installer test interrupted')


if __name__ == '__main__':
    # Ensure hosted-runner cancellation enters the same cleanup path as Ctrl-C.
    signal.signal(signal.SIGTERM, interrupted)
    main()
