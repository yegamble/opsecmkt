"""The owner's rehearsal procedure stays runnable (A-743; DA104-1, DA104-E1).

docs/testnet-runbook.md "Fresh stagenet market and rehearsals" is what the owner follows on real hosts, where a wrong
flag is found only after the destructive part. No host is touched here. The checks read the section as text:

* it is linked from the runbook's top, and it names A-562, A-698, A-699 and PM-3;
* its parts come in the order the steps must be done: retired, approval, preserved, re-init, legs;
* every `opsecmkt` command in its code blocks uses subcommands and flags that exist in the Go sources cmd/server runs;
* every script it runs exists, every relative link resolves, and no command carries a private value or mainnet;
* each A-698 acceptance line is quoted by its text and that text is still in docs/acceptance.md (line numbers go stale).
"""
from pathlib import Path
import re
import shlex
import unittest

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


if __name__ == '__main__':
    unittest.main()
