"""The owner's rehearsal procedure stays runnable (A-743; DA104-1, DA104-E1).

docs/testnet-runbook.md "Fresh stagenet market and rehearsals" is what the owner follows on real hosts, where a wrong
flag is found only after the destructive part. No host is touched here. The checks read the section as text, and ask
the built binary only for help and, in an empty temporary directory, to generate keys:

* it is linked from the runbook's top, and it names A-562, A-698, A-699 and PM-3;
* its parts come in the order the steps must be done: retired, approval, preserved, re-init, legs;
* every `opsecmkt` command in its code blocks uses subcommands and flags that exist in the Go sources cmd/server runs,
  and each flag is one the built binary's help lists for that subcommand (DA110-3);
* every guide makes the parent directory of a key command's output first, and the key blocks run (DA110-4);
* every script it runs exists, every relative link resolves, and no command carries a private value or mainnet;
* each A-698 acceptance line is quoted by its text and that text is still in docs/acceptance.md (line numbers go stale).
"""
import functools
import itertools
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import subprocess
import tempfile
import unittest

BINARY_DIR = None
REPOSITORY = Path(__file__).resolve().parents[1]
RUNBOOK_PATH = REPOSITORY / 'docs' / 'testnet-runbook.md'
RUNBOOK = RUNBOOK_PATH.read_text()
ACCEPTANCE = ' '.join((REPOSITORY / 'docs' / 'acceptance.md').read_text().split())
HEADING = '## Fresh stagenet market and rehearsals'
ANCHOR = 'fresh-stagenet-market-and-rehearsals'

# The packages cmd/server dispatches each command to.
SOURCES = ('cmd/server', 'internal/operator', 'internal/hostbootstrap', 'internal/keymaterial', 'internal/onionidentity')

# docs/acceptance.md, by quoted text: A-698's rows carry line numbers (AC:366, 438, 990, 994) that have moved.
ACCEPTANCE_LINES = (
    'a restore rehearsal is recorded',
    'An actual app-only container redeployment preserves the saved access and registration settings',
    'Supported-host apply/confirm/rollback and pending boot are rehearsed',
    'A replacement production app host restores retained configuration, identities and current external retention state',
)


def section():
    start = RUNBOOK.index(HEADING)
    end = RUNBOOK.find('\n## ', start + 1)
    return RUNBOOK[start:] if end < 0 else RUNBOOK[start:end]


def code_blocks(text):
    return re.findall(r'^ *```(?:sh|text)\n(.*?)^ *```', text, re.M | re.S)


def commands(text):
    """Each logical line of the code blocks, with backslash continuations joined and comments dropped."""
    result = []
    for block in code_blocks(text):
        joined = re.sub(r'\\\n\s*', ' ', block)
        for line in joined.splitlines():
            line = line.strip()
            if line and not line.startswith('#'):
                result.append(line)
    return result


def opsecmkt_arguments(line):
    """The words after the opsecmkt program in a command line, or None when the line does not run it."""
    match = re.search(r'(?:^|[\s/])opsecmkt\s+(.*)$', line)
    if not match:
        return None
    return shlex.split(match.group(1))


def runbook_arguments(line):
    """opsecmkt_arguments for a command that runs the program: a repository path such as OWNER/opsecmkt does not."""
    return None if line.startswith('gh ') else opsecmkt_arguments(line)


def built_binary():
    """Build cmd/server once. CI's Python job has Go and fails on any skip, so this skip cannot hide there."""
    global BINARY_DIR
    if shutil.which('go') is None:
        raise unittest.SkipTest('this check runs the built binary and needs Go')
    if BINARY_DIR is None:
        BINARY_DIR = tempfile.TemporaryDirectory()
        output = os.path.join(BINARY_DIR.name, 'opsecmkt')
        subprocess.run(['go', 'build', '-o', output, './cmd/server'], cwd=REPOSITORY, check=True, capture_output=True, text=True)
    return os.path.join(BINARY_DIR.name, 'opsecmkt')


@functools.lru_cache(maxsize=None)
def help_text(*arguments):
    # Only help is asked for. The program writes usage to either stream, and -help on the legacy flags exits nonzero.
    result = subprocess.run([built_binary(), *arguments], stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=60)
    return result.stdout + result.stderr


def subcommand_flags(path):
    """The flags the built binary's own help lists for this subcommand; path is a list such as ['deploy', 'plan']."""
    if path[:1] == ['keys']:
        # Each key command takes its flags from the one usage text, which has a line per action and type.
        action, kind = path[1], path[2]
        lines = re.findall(r'(?m)^keys (\S+) (\S+) (.*)$', help_text('keys', 'help'))
        return {flag for a, k, rest in lines if a == action and kind in k.split('|') for flag in re.findall(r'--([a-z-]+)', rest)}
    if path[:2] == ['deploy', 'recovery']:
        # One help text for both commands, with a line each.
        line = re.search(r'(?m)^\s+%s (.*)$' % path[2], help_text('deploy', 'recovery', 'help')).group(1)
        return set(re.findall(r'--([a-z-]+)', line))
    text = help_text(*path, '-h') if path else help_text('-help')
    return set(re.findall(r'(?m)^\s+-([a-z][a-z0-9-]*)', text))


def replacement_leg():
    return re.search(r'^### Leg: replacement application host.*?(?=^### )', section(), re.M | re.S).group(0)


def go_sources():
    text = []
    for package in SOURCES:
        text.extend(p.read_text() for p in sorted((REPOSITORY / package).glob('*.go')) if not p.name.endswith('_test.go'))
    return '\n'.join(text)


def slug(heading):
    text = re.sub(r'[^\w\- ]', '', heading.strip().lower())
    return text.replace(' ', '-')


def anchors(path):
    result, seen, in_code = set(), {}, False
    for line in path.read_text().splitlines():
        if line.lstrip().startswith('```'):
            in_code = not in_code
            continue
        match = None if in_code else re.match(r'^#{1,6} (.*)$', line)
        if match:
            base = slug(match.group(1))
            count = seen.get(base, 0)
            seen[base] = count + 1
            result.add(base if count == 0 else '%s-%d' % (base, count))
    return result


class RehearsalSectionTest(unittest.TestCase):
    def test_linked_from_the_top_of_the_runbook(self):
        head = RUNBOOK[:RUNBOOK.index('\n## ')]
        self.assertIn('(#%s)' % ANCHOR, head)

    def test_names_every_item(self):
        for item in ('A-562', 'A-698', 'A-699', 'PM-3', 'A-669', 'A-741'):
            self.assertIn(item, section())

    def test_parts_come_in_order(self):
        text = section()
        parts = ['### What is retired', '### What the owner must approve', '### What is preserved',
                 '### Re-initialize from the tagged build', '### Legs and their order',
                 '### Leg: production wallet topology (A-562)', '### Leg: public physical journey (PM-3)',
                 '### Leg: real outages (DA104-E1)', '### Leg: restore rehearsal (A-698)',
                 '### Leg: app-only redeployment (A-698)', '### Leg: host policy and pending boot (A-698)',
                 '### Leg: replacement application host (A-698)', '### Recording the results']
        at = [text.find(part) for part in parts]
        self.assertNotIn(-1, at, dict(zip(parts, at)))
        self.assertEqual(at, sorted(at))

    def test_opsecmkt_commands_exist(self):
        go = go_sources()
        seen = 0
        problems = []
        for line in commands(section()):
            arguments = opsecmkt_arguments(line)
            if arguments is None:
                continue
            seen += 1
            flagged = False
            for word in arguments:
                if word.startswith('-'):
                    flagged = True
                    name = word.lstrip('-').split('=')[0]
                    if '"%s"' % name not in go:
                        problems.append('flag %s in: %s' % (word, line))
                elif not flagged and re.fullmatch(r'[a-z][a-z-]*', word) and '"%s"' % word not in go:
                    problems.append('subcommand %s in: %s' % (word, line))
        self.assertEqual(problems, [], '\n'.join(problems))
        self.assertGreater(seen, 15, 'the code blocks should hold the procedure commands')

    def test_a_flag_check_can_fail(self):
        self.assertEqual(opsecmkt_arguments('sudo ./opsecmkt host confirm --config a'), ['host', 'confirm', '--config', 'a'])
        self.assertEqual(opsecmkt_arguments('DATABASE_URL_FILE=/x ./opsecmkt -status'), ['-status'])
        self.assertIsNone(opsecmkt_arguments('systemctl stop opsecmkt-gateway'))
        self.assertIsNone(opsecmkt_arguments('install -d -m 700 /srv/opsecmkt/market-stagenet'))
        self.assertNotIn('"no-such-flag"', go_sources())

    def test_init_is_a_stagenet_production_one_and_keeps_relay_open(self):
        init = [line for line in commands(section()) if ' deploy init ' in line + ' ']
        self.assertEqual(len(init), 1, init)
        for words in ('--mode prod', '--monero-network stagenet', '--escrow-xmr', '--exposure tor',
                      '--set ESCROW_XMR_TOR_RELAY=remote-tor', '--set RETENTION_ENABLED=on', '--retention-dir'):
            self.assertIn(words, init[0])

    def test_no_command_names_mainnet_or_a_private_value(self):
        problems = []
        for line in commands(section()):
            if re.search(r'\bmainnet\b', line):
                problems.append('mainnet: ' + line)
            if re.search(r'[a-z+]+://[^/\s]*:[^/\s]*@', line) or re.search(r'(?i)password\s*=', line):
                problems.append('credential: ' + line)
            for word in ('--setup-token ', '--app-secret-key ', 'cat /srv/'):
                if word in line:
                    problems.append('private value: ' + line)
        self.assertEqual(problems, [], '\n'.join(problems))

    def test_scripts_exist(self):
        for script in set(re.findall(r'\bscripts/[\w.-]+', section())):
            self.assertTrue((REPOSITORY / script).exists(), script)

    def test_links_resolve(self):
        broken = []
        for target in re.findall(r'\]\(([^)\s]+)\)', section()):
            if '://' in target:
                continue
            name, _, anchor = target.partition('#')
            path = (RUNBOOK_PATH.parent / name) if name else RUNBOOK_PATH
            if not path.exists():
                broken.append(target)
            elif anchor and path.suffix == '.md' and anchor not in anchors(path):
                broken.append(target)
        self.assertEqual(broken, [], 'links that do not resolve')

    def test_acceptance_lines_are_quoted_by_text_and_exist(self):
        text = ' '.join(section().split())
        for line in ACCEPTANCE_LINES:
            self.assertIn(line, ACCEPTANCE, 'docs/acceptance.md no longer says it')
            self.assertIn(line, text, 'the section does not quote it')
        self.assertNotRegex(text, r'AC:\d+', 'cite the text, not a line number')

    def test_says_where_no_runner_exists_and_what_each_leg_does_not_prove(self):
        text = section()
        self.assertIn('scripts/test-escrow-xmr-public-stagenet.py', text)
        self.assertIn('**No runner', text)
        legs = re.findall(r'^### Leg: .*?(?=^### |\Z)', text, re.M | re.S)
        self.assertEqual(len(legs), 7)
        for leg in legs:
            title = leg.splitlines()[0]
            self.assertIn('**Does not prove', leg, title)
            self.assertIn('**Record', leg, title)

    def test_real_durations_are_stated(self):
        text = ' '.join(section().split())
        for words in ('48-hour', '72-hour', 'real 48 hours', 'at least 7 days'):
            self.assertIn(words, text)

    def test_outage_legs_use_the_real_shell_command_and_admin_page(self):
        leg = re.search(r'^### Leg: real outages.*?(?=^### )', section(), re.M | re.S).group(0)
        self.assertIn('-request-deadline-recheck', leg)
        self.assertIn('/admin/payments', leg)
        # QA108-3: a buyer returning to the last request's remainder can have minutes left, so leg (b) ends with one more.
        self.assertIn('run the same command once more', ' '.join(leg.split()))
        self.assertNotIn('db_sql', leg)
        self.assertNotIn('docker compose exec', leg)

    def test_old_services_cannot_return_before_the_new_ones_start(self):
        # DA743-1: a reboot after step 1 must not bring back the old onion, which the gateway would forward to the new app.
        text = section()
        step = lambda n: re.search(r'^%d\. \*\*.*?(?=^\d+\. \*\*|^\*\*Record|\Z)' % n, text, re.M | re.S).group(0)
        first = step(1)
        for command in ('systemctl disable --now opsecmkt-gateway', 'systemctl disable --now YOUR_OLD_TOR_UNIT',
                        'systemctl disable --now YOUR_OLD_MONEROD_UNIT YOUR_OLD_WALLET_RPC_UNIT'):
            self.assertIn(command, first)
        self.assertNotIn('systemctl stop', first)
        self.assertIn('docker update --restart=no', first, 'the unit shape is unknown: say the container equivalent')
        self.assertIn('systemctl enable --now opsecmkt-gateway', step(6))
        self.assertNotIn('systemctl start', step(6))
        last = step(8)
        for command in ('systemctl is-active YOUR_OLD_TOR_UNIT YOUR_OLD_MONEROD_UNIT YOUR_OLD_WALLET_RPC_UNIT',
                        'systemctl is-enabled YOUR_OLD_TOR_UNIT YOUR_OLD_MONEROD_UNIT YOUR_OLD_WALLET_RPC_UNIT'):
            self.assertIn(command, last)
        self.assertLess(last.index('inactive and disabled'), last.index('enable public Tor'))

    def test_deleting_the_old_directory_removes_what_runs_against_it(self):
        # DA743-2: the source names these; the agent goes before the directory it checks.
        go = go_sources()
        self.assertIn('"opsecmkt-host-control-" + c.Project + ".service"', go)
        self.assertIn('"/etc/opsecmkt-host-control/" + project + ".json"', go)
        self.assertIn('"/var/lib/opsecmkt-host-control/" + project', go)
        self.assertIn('"50-opsecmkt-storage-"+c.Project+".conf"', go)
        text = section()
        for name in ('opsecmkt-host-control-OLD_PROJECT.service', '/etc/opsecmkt-host-control/OLD_PROJECT.json',
                     '/var/lib/opsecmkt-host-control/OLD_PROJECT'):
            self.assertIn(name, text[:text.index('### What is preserved')])
        lines = commands(text)
        disable = lines.index('systemctl disable --now opsecmkt-host-control-OLD_PROJECT.service')
        drop_in = lines.index('rm /etc/systemd/system/docker.service.d/50-opsecmkt-storage-OLD_PROJECT.conf')
        self.assertLess(disable, drop_in)
        flat = ' '.join(text.split())
        self.assertIn('whether systemd holds Docker behind its pending start job is unverified', flat)
        self.assertIn('`deploy down` does not stop this agent', flat)

    def test_inputs_step_says_where_the_tls_files_come_from(self):
        # DA743-3: every file step 5 passes has a stated source, and no private key is ever an argument.
        text = section()
        inputs = re.search(r'^3\. \*\*Prepare the new private inputs.*?(?=^4\. \*\*)', text, re.M | re.S).group(0)
        for name in ('database-ca.pem', 'app.pem', 'app.key', 'upload-ca.pem', 'backend.pem', 'backend.key', 'monero-ca.pem'):
            self.assertIn('`%s`' % name, inputs)
            self.assertIn('/srv/opsecmkt-inputs-stagenet/' + name, text)
        self.assertIn('`0600`', inputs)
        self.assertIn('never go on a command line or in a report', ' '.join(inputs.split()))

    def test_only_the_market_tables_must_be_empty(self):
        # DA743-N1: the old administrator is a user, so "any answer" would stop every run.
        flat = ' '.join(section().split())
        self.assertIn('orders, escrows and settlements must all be 0', flat)
        self.assertIn('`users` counts the old administrator', flat)

    def test_a_retired_identity_is_never_overwritten_or_reused(self):
        text = ' '.join(section().split())
        for words in ('never overwrite', 'populated identity', 'storage-check drop-in'):
            self.assertIn(words, text)
        self.assertNotIn('keys import onion', text, 'a fresh market imports no retired identity')

    def test_replacement_leg_gives_the_host_its_address_before_anything_reads_it(self):
        # DA110-1, DA110-2. TestRecoveryLegRestoreBindTLSReplaceRunsInWrittenOrder runs these same lines on the fake host.
        leg = replacement_leg()
        flat = ' '.join(leg.split())
        self.assertRegex(leg, r'(?m)^\d+\. \*\*Restore with the spare host\'s own address\.\*\*')
        lines = commands(leg)
        order = [next(i for i, line in enumerate(lines) if needle in line)
                 for needle in ('deploy down', 'install -d -m 700 /srv/opsecmkt', 'deploy recovery restore', 'deploy storage bind',
                                'deploy tls replace', 'deploy plan', 'deploy doctor', 'deploy install')]
        self.assertEqual(order, sorted(order), lines)
        restore = next(line for line in lines if 'deploy recovery restore' in line)
        for words in ('--old-host-fenced', '--app-bind 10.20.0.20:8080', '--backend-bind 10.20.0.20:8443'):
            self.assertIn(words, restore)
        init = next(line for line in commands(section()) if ' deploy init ' in line + ' ')
        self.assertNotIn('10.20.0.20', init, 'the spare host keeps its own address')
        # compose.json is excluded from the archive, so this refuses until install writes it.
        self.assertFalse([line for line in lines if '--start-check' in line])
        self.assertIn('`--start-check` needs the Compose file `install` writes', flat)
        # validate refuses a missing report directory at the first command that loads the configuration.
        self.assertLess(flat.index('dependency-report directory'), flat.index('--app-bind 10.20.0.20'))
        self.assertIn('never been run on a host', flat)

    def test_replacement_leg_says_what_the_isolated_restore_needs(self):
        first = re.search(r'(?m)^1\. .*$', replacement_leg()).group(0)
        for words in ('loopback', 'no address of this host', '`--old-host-fenced`'):
            self.assertIn(words, first)

    def test_replacement_leg_names_how_the_gateway_moves(self):
        flat = ' '.join(replacement_leg().split())
        for words in ('`--app-url`', '`--backend-url`', 'resolve to the spare host', 'restart'):
            self.assertIn(words, flat)
        self.assertNotIn('Point the gateway at the new private address', flat)

    def test_each_flag_belongs_to_its_subcommand(self):
        # DA110-3: test_opsecmkt_commands_exist accepts a flag that any command has, anywhere in the sources.
        problems, seen = [], 0
        for line in commands(section()):
            arguments = runbook_arguments(line)
            if arguments is None:
                continue
            path = list(itertools.takewhile(lambda word: not word.startswith('-'), arguments))
            used = [word.lstrip('-').split('=')[0] for word in arguments if word.startswith('-')]
            if not used:
                continue
            known = subcommand_flags(path)
            seen += 1
            problems.extend('%s has no --%s: %s' % (' '.join(path) or 'opsecmkt', flag, line) for flag in used if flag not in known)
        self.assertEqual(problems, [], '\n'.join(problems))
        self.assertGreater(seen, 15)

    def test_the_flag_check_can_fail(self):
        # The mutations DA110-3 made, which the older check let through.
        self.assertNotIn('monero-seed-key', subcommand_flags(['deploy', 'plan']))
        self.assertNotIn('cookie-secure', subcommand_flags(['deploy', 'plan']))
        self.assertNotIn('old-host-fenced', subcommand_flags(['deploy', 'status']))
        self.assertIn('monero-seed-key', subcommand_flags(['deploy', 'init']))
        self.assertIn('cookie-secure', subcommand_flags(['serve']))
        self.assertIn('old-host-fenced', subcommand_flags(['deploy', 'recovery', 'restore']))
        self.assertIn('app-bind', subcommand_flags(['deploy', 'recovery', 'restore']))
        self.assertNotIn('app-bind', subcommand_flags(['deploy', 'recovery', 'export']))
        self.assertIn('out-dir', subcommand_flags(['keys', 'generate', 'onion']))
        self.assertNotIn('out-dir', subcommand_flags(['keys', 'generate', 'tor-client']))
        self.assertIn('out', subcommand_flags(['keys', 'import', 'audit']))
        self.assertIn('clear-restore-gate', subcommand_flags([]))
        self.assertIsNone(runbook_arguments('gh release download T --repo OWNER/opsecmkt --dir d'))


class KeyCommandParentsTest(unittest.TestCase):
    """DA110-4: key commands refuse a missing parent directory, so a guide that shows one also shows the directory made."""

    OUTPUTS = ('--out', '--out-dir', '--public-out', '--private-out', '--cert-out', '--key-out')

    def test_every_guide_creates_the_parents_of_its_key_outputs(self):
        problems, seen = [], 0
        guides = [REPOSITORY / 'README.md'] + sorted((REPOSITORY / 'docs').glob('*.md')) + sorted((REPOSITORY / 'docs' / 'install').glob('*.md'))
        for guide in guides:
            made = set()
            for line in commands(guide.read_text()):
                match = re.match(r'install -d\b(.*)$', line)
                if match:
                    made.update(os.path.normpath(word) for word in shlex.split(match.group(1)) if not re.fullmatch(r'-m|\d{3,4}', word))
                arguments = opsecmkt_arguments(line) if re.search(r'opsecmkt keys\b', line) else None
                if not arguments:
                    continue
                seen += 1
                for flag, value in zip(arguments, arguments[1:]):
                    parent = os.path.dirname(os.path.normpath(value))
                    if flag in self.OUTPUTS and parent and parent not in made:
                        problems.append('%s: %s is not created before: %s' % (guide.name, parent, line))
        self.assertEqual(problems, [], '\n'.join(problems))
        self.assertGreater(seen, 20)

    def test_key_blocks_run_in_an_empty_directory(self):
        binary = built_binary()
        cases = ((RUNBOOK_PATH, './identities-stagenet/storefront'), (REPOSITORY / 'docs/install/tor.md', './identities/administrator'),
                 (REPOSITORY / 'docs/install/tor.md', './admin-private/operator.key'),
                 (REPOSITORY / 'docs/install/cli-reference.md', './identities/administrator'),
                 (REPOSITORY / 'docs/install/cli-reference.md', './private/operator.key'))
        for guide, output in cases:
            with self.subTest(guide=guide.name, output=output), tempfile.TemporaryDirectory() as work:
                block = next(b for b in code_blocks(guide.read_text()) if 'keys generate' in b and output in b)
                os.symlink(binary, os.path.join(work, 'opsecmkt'))
                result = subprocess.run(['sh', '-e', '-c', block], cwd=work, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(os.path.exists(os.path.join(work, output)), output)
                parent = os.path.dirname(os.path.join(work, output))
                self.assertEqual(stat.S_IMODE(os.stat(parent).st_mode), 0o700)


if __name__ == '__main__':
    unittest.main()
