"""Every container's privileges in the resolved Compose configuration (war room A-536, audit F4).

For every mode and profile combination that .github/workflows/ci.yaml's compose job validates (the same matrix as
tests/test_compose_exposure.py, each also with the malware scanner and with the Monero escrow wallet), each service
must drop every capability, set no-new-privileges, run with a read-only root filesystem and run as a named user.
The tables below list every service and say who it runs as; a service missing from them fails, so a new one is a
review. What each image does at startup, and why these settings start it, was proved by starting the images offline
(scripts/test-service-hardening.sh, in CI next to the monerod port probe).

The resolved checks skip only when `docker compose` is absent; the CI operations job fails on any skip.
"""
from pathlib import Path
import unittest

from test_compose_exposure import EXTRAS, combinations, compose_available, resolved

REPOSITORY = Path(__file__).resolve().parents[1]

# Who each service runs as: the `user:` Compose sets, or the image file and line that set it when Compose does not.
IMAGE_USER = 'image'
USERS = {
    'app': (IMAGE_USER, 'Dockerfile', 'USER 65532:65532'),
    'db': '70:70',
    'tor': (IMAGE_USER, 'deploy/Dockerfile.tor', 'USER debian-tor'),
    'tor-mirror': (IMAGE_USER, 'deploy/Dockerfile.tor', 'USER debian-tor'),
    'tor-socks': (IMAGE_USER, 'deploy/Dockerfile.tor', 'USER debian-tor'),
    'clamd': 'clamav',
    'escrow-signer': '65533:65532',
    'monero': '1000:1000',
    'monero-escrow-wallet': '1000:1000',
    # Starts as root: bitcoin/bitcoin's /entrypoint.sh chmods and chowns /data and /home/bitcoin on every start, then
    # runs bitcoind as bitcoin with gosu. A `user:` fails on a fresh volume, and a volume restored as root relies on that
    # chown. The documented root `sh` commands that append to /data/bitcoin.conf need DAC_OVERRIDE (CAP_ADD below).
    'bitcoin': None,
}
# The only capabilities a service may add back after dropping them all. gosu needs SETUID and SETGID (it is not
# setuid, so no-new-privileges does not stop it).
CAP_ADD = {'bitcoin': {'CHOWN', 'DAC_OVERRIDE', 'FOWNER', 'SETUID', 'SETGID'}}
# Services whose root filesystem stays writable, and why.
EXCEPTIONS = {
    'tor': 'the onion service; never started with a read-only root here (its CI offline start and the identity '
           'import and restore tests run it writable), so it stays as it was',
    'tor-mirror': 'the same image and torrc as tor, for the mirror onion',
}


def hardening_problems(config):
    """What a resolved configuration's services lack of the hardening above."""
    problems = []
    for name, service in sorted(config['services'].items()):
        if name not in USERS:
            problems.append(f'{name} is not in USERS: add it to the tables in tests/test_compose_hardening.py')
            continue
        if service.get('cap_drop') != ['ALL']:
            problems.append(f"{name}: cap_drop is {service.get('cap_drop')!r}, not ['ALL']")
        if 'no-new-privileges:true' not in (service.get('security_opt') or []):
            problems.append(f'{name}: security_opt lacks no-new-privileges:true')
        if service.get('read_only') is not True and name not in EXCEPTIONS:
            problems.append(f'{name}: the root filesystem is not read-only')
        extra = set(service.get('cap_add') or []) - CAP_ADD.get(name, set())
        if extra:
            problems.append(f'{name}: cap_add {sorted(extra)} is not allowed')
        user = USERS[name]
        if isinstance(user, str) and service.get('user') != user:
            problems.append(f"{name}: user is {service.get('user')!r}, not {user!r}")
        if user is None and service.get('user') is not None:
            problems.append(f"{name}: sets user {service['user']!r}, but its entrypoint must start as root")
    return problems


class HardeningCheckTests(unittest.TestCase):
    """The check itself, on small configurations: it needs no Docker."""

    DB = {'user': '70:70', 'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true'], 'read_only': True}
    BITCOIN = {'cap_drop': ['ALL'], 'cap_add': ['CHOWN', 'DAC_OVERRIDE', 'FOWNER', 'SETUID', 'SETGID'],
               'security_opt': ['no-new-privileges:true'], 'read_only': True}

    def test_passes_a_hardened_service(self):
        self.assertEqual(hardening_problems({'services': {'db': self.DB, 'bitcoin': self.BITCOIN}}), [])
        tor = {'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true']}
        self.assertEqual(hardening_problems({'services': {'tor': tor}}), [])

    def test_fails_when_a_key_is_removed(self):
        for key in self.DB:
            with self.subTest(key=key):
                service = {k: v for k, v in self.DB.items() if k != key}
                self.assertTrue(hardening_problems({'services': {'db': service}}))

    def test_fails_on_weaker_values(self):
        for change in ({'cap_drop': ['NET_RAW']}, {'cap_drop': ['ALL', 'NET_RAW']}, {'read_only': False},
                       {'security_opt': ['label=disable']}, {'user': '0:0'}, {'cap_add': ['NET_BIND_SERVICE']}):
            with self.subTest(change=change):
                self.assertTrue(hardening_problems({'services': {'db': {**self.DB, **change}}}))

    def test_bitcoin_adds_only_its_allow_list_and_no_user(self):
        for change in ({'cap_add': [*self.BITCOIN['cap_add'], 'SYS_ADMIN']}, {'user': 'bitcoin'}, {'read_only': False}):
            with self.subTest(change=change):
                self.assertTrue(hardening_problems({'services': {'bitcoin': {**self.BITCOIN, **change}}}))

    def test_fails_for_an_unlisted_service(self):
        self.assertTrue(hardening_problems({'services': {'new-service': self.DB}}))

    def test_image_users_are_in_their_dockerfiles(self):
        for name, user in USERS.items():
            if isinstance(user, tuple):
                with self.subTest(name):
                    _, path, line = user
                    self.assertIn(line, (REPOSITORY / path).read_text().splitlines())

    def test_ci_starts_the_hardened_containers_next_to_the_monerod_probe(self):
        workflow = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
        job = workflow[workflow.index('\n  tor-image:\n'):workflow.index('\n  browser:\n') + 1]
        self.assertRegex(job, r'\n        timeout-minutes: \d+\n        run: bash scripts/test-service-hardening.sh\n')
        self.assertLess(job.index('run: bash scripts/test-monerod-ports.sh\n'),
                        job.index('run: bash scripts/test-service-hardening.sh\n'))
        self.assertIn('./scripts/test-service-hardening.sh', (REPOSITORY / 'docs' / 'operations-tests.md').read_text())


@unittest.skipUnless(compose_available(), 'docker compose is not available')
class ResolvedComposeHardeningTests(unittest.TestCase):
    def test_every_service_in_every_combination_is_hardened(self):
        seen, services = 0, set()
        for name, _, files, profiles in combinations():
            variants = [('', [], [], {})] + [(f' + {extra}', f, p, e) for extra, (f, p, e) in EXTRAS.items()]
            for suffix, extra_files, extra_profiles, environment in variants:
                with self.subTest(name + suffix):
                    config = resolved(files + extra_files, profiles + extra_profiles, environment)
                    self.assertEqual(hardening_problems(config), [])
                    services |= set(config['services'])
                    seen += 1
        self.assertEqual(seen, 10 * 3, 'the combinations enumerated no longer match the compose job matrix')
        self.assertEqual(services, set(USERS), 'USERS lists a service no combination has, or misses one')

    def test_tmpfs_for_what_the_read_only_images_write(self):
        """postgres writes its socket and lock to /var/run/postgresql; bitcoin's entrypoint chowns /home/bitcoin, and
        docs/testnet-runbook.md has bitcoind write wallet backups to /tmp."""
        files, profiles = dict((name, (f, p)) for name, _, f, p in combinations())['clearnet, internal database']
        services = resolved(files, profiles)['services']
        self.assertEqual(services['db']['tmpfs'], ['/var/run/postgresql', '/tmp'])
        self.assertEqual(services['bitcoin']['tmpfs'], ['/home/bitcoin', '/tmp'])


if __name__ == '__main__':
    unittest.main()
