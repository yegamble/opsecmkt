"""docs/install/replace-a-host.md stays grounded in the commands that exist (owner request, 2026-11-03).

The guide is what an operator follows when a host dies or is compromised, where a wrong flag is found only after
something has been lost. No host is touched here. Like tests/test_runbook_rehearsals.py, the checks read the guide as
text and ask the built binary only for its help:

* every `opsecmkt` subcommand and flag in its code blocks exists, and each flag is one the built binary's help lists
  for that subcommand;
* its parts come in the order the owner asked for: the decision, the four roles, the checklist, the honesty section;
* the compromised case names each secret to rotate, says which have no rotation command, and keeps destructive response
  behind the owner's explicit approval;
* the mirror is what compose.tor.yaml's `mirror` profile is, a second onion for the same instance, and no standby is
  claimed;
* no recovery time is claimed and the replacement legs are stated as unrehearsed;
* links resolve, scripts exist, and no command carries mainnet or a private value;
* mkdocs nav, the install index, recovery.md and ram-secrets-and-emergency.md link to it, and acceptance.md holds an
  unticked line for it.
"""
import itertools
import re
import unittest

from test_runbook_rehearsals import REPOSITORY, anchors, commands, go_sources, help_text, opsecmkt_arguments, subcommand_flags

GUIDE_PATH = REPOSITORY / 'docs' / 'install' / 'replace-a-host.md'
GUIDE = GUIDE_PATH.read_text() if GUIDE_PATH.exists() else ''

# `deploy storage prepare|unlock|verify` are dispatched to this package, which the runbook's commands never reach.
STORAGE_SOURCES = REPOSITORY / 'internal' / 'storageinit'

PENDING = {}

PARTS = (
    '## 1. Decide which case you are in',
    '## 2. Replace a host, role by role',
    '### Application host',
    '### Payment host',
    '### Gateway host',
    '### Database',
    '## 3. What you need before disaster',
    '## 4. What has been rehearsed',
)


def part(heading):
    start = GUIDE.index(heading)
    level = heading.split(' ')[0]
    following = re.search(r'^#{1,%d} ' % len(level), GUIDE[start + len(heading):], re.M)
    return GUIDE[start:] if following is None else GUIDE[start:start + len(heading) + following.start()]


def flat(text):
    return ' '.join(text.split())


def sources():
    storage = (p.read_text() for p in sorted(STORAGE_SOURCES.glob('*.go')) if not p.name.endswith('_test.go'))
    return go_sources() + '\n' + '\n'.join(storage)


def flags_for(path):
    """subcommand_flags, or for a command whose -h prints only its group's usage, the flags on its own usage line."""
    known = subcommand_flags(path)
    if known or len(path) < 2:
        return known
    line = re.search(r'(?m)^\s+%s (.*)$' % re.escape(path[-1]), help_text(*path, '-h'))
    return set(re.findall(r'--([a-z-]+)', line.group(1))) if line else set()


def targets(path):
    """Heading anchors plus explicit <a id> anchors, which the operator guide uses for some paragraphs."""
    return anchors(path) | set(re.findall(r'<a id="([^"]+)"></a>', path.read_text()))


class ReplaceAHostGuideTest(unittest.TestCase):
    def setUp(self):
        self.assertTrue(GUIDE, 'docs/install/replace-a-host.md is missing')

    def test_parts_come_in_order(self):
        at = [GUIDE.find(heading) for heading in PARTS]
        self.assertNotIn(-1, at, dict(zip(PARTS, at)))
        self.assertEqual(at, sorted(at))

    def test_decision_table_names_the_three_cases(self):
        decision = flat(part('## 1. Decide which case you are in'))
        for case in ('Dead host, not compromised', 'Compromised or suspected compromised', 'Mirror or standby'):
            self.assertIn('| **%s**' % case, decision)

    def test_compromise_names_each_secret_and_how(self):
        text = flat(part('### Compromised or suspected compromised'))
        for secret in ('`app_secret_key`', 'setup token', 'audit signing key', 'tls', 'mutual-tls', 'ca',
                       'administrator client', 'onion', 'database', 'wallet rpc', '`monero_wallet_secret`', 'seed'):
            self.assertIn(secret, text.lower())
        for command in ('deploy endpoint set', 'deploy tls replace', 'keys generate onion', 'keys generate audit',
                        'keys generate tls-ca', '-reset-admin-factors'):
            self.assertIn(command, text)
        # Where no command rotates a secret, the guide says so rather than inventing one.
        self.assertIn('no command', text)
        self.assertIn('never reused', text)
        self.assertIn('No tool re-keys existing wallets', text)

    def test_destructive_response_waits_for_the_owner(self):
        text = flat(part('### Compromised or suspected compromised'))
        for words in ('explicit approval', 'snapshot or disk image', 'timestamps', 'isolate'):
            self.assertIn(words, text)
        self.assertNotIn('erase-luks', ' '.join(commands(GUIDE)), 'a guide step never erases a disk')
        self.assertNotRegex(' '.join(commands(GUIDE)), r'docker volume rm|\brm -r', 'deletion is an owner decision')

    def test_mirror_is_the_same_instance_and_no_standby_is_claimed(self):
        mirror = flat(part('### Mirror or standby'))
        for words in ('`mirror` profile', '`tor-mirror`', 'same instance', 'one database', 'no hot standby'):
            self.assertIn(words, mirror)
        self.assertIn('profiles: [mirror]', (REPOSITORY / 'compose.tor.yaml').read_text())
        self.assertNotRegex(flat(GUIDE), r'(?i)fail ?over to the (mirror|standby)')

    def test_application_steps_follow_the_recovery_order(self):
        app = part('### Application host')
        lines = commands(app)
        order = [next((i for i, line in enumerate(lines) if needle in line), -1)
                 for needle in ('deploy recovery restore', 'deploy storage bind', 'deploy plan', 'deploy doctor',
                                'deploy install')]
        self.assertNotIn(-1, order, lines)
        self.assertEqual(order, sorted(order), lines)
        restore = next(line for line in lines if 'deploy recovery restore' in line)
        for words in ('--old-host-fenced', '--app-bind', '--backend-bind'):
            self.assertIn(words, restore)
        text = flat(app)
        self.assertIn('trusted source', text)
        self.assertIn('-request-deadline-recheck', text)

    def test_database_trusted_source_moves_to_the_new_app_host(self):
        text = flat(part('### Database'))
        self.assertIn("new application host's private address", text)
        self.assertIn('-set-restore-gate', text)
        self.assertIn('deploy endpoint set', text)

    def test_each_role_says_how_to_prove_and_retire(self):
        for heading in PARTS[2:6]:
            text = flat(part(heading))
            self.assertIn('**Prove it works.**', text, heading)
            self.assertIn('**Retire the old host.**', text, heading)

    def test_checklist_marks_what_is_unverified(self):
        text = flat(part('## 3. What you need before disaster'))
        for words in ('LUKS header', 'recovery export', 'host-key fingerprint', 'operator record', 'CA', 'tested restore'):
            self.assertIn(words, text)
        self.assertIn('UNVERIFIED', text)

    def test_honesty_states_what_ran_and_claims_no_recovery_time(self):
        text = flat(part('## 4. What has been rehearsed'))
        for words in ('A-698', 'None of the replacement legs', 'three stagenet hosts', 'No recovery time'):
            self.assertIn(words, text)
        self.assertNotRegex(text, r'\b\d+\s*(?:minutes?|hours?|days?)\b', 'a duration here would be a claim')

    def test_opsecmkt_commands_exist(self):
        go = sources()
        problems, seen = [], 0
        for line in commands(GUIDE):
            arguments = opsecmkt_arguments(line)
            if arguments is None:
                continue
            seen += 1
            flagged = False
            for word in arguments:
                if word.startswith('-'):
                    flagged = True
                    if '"%s"' % word.lstrip('-').split('=')[0] not in go:
                        problems.append('flag %s in: %s' % (word, line))
                elif not flagged and re.fullmatch(r'[a-z][a-z-]*', word) and '"%s"' % word not in go:
                    problems.append('subcommand %s in: %s' % (word, line))
        self.assertEqual(problems, [], '\n'.join(problems))
        self.assertGreater(seen, 20, 'the code blocks should hold the procedure commands')

    def test_each_flag_belongs_to_its_subcommand(self):
        problems, seen = [], 0
        for line in commands(GUIDE):
            arguments = opsecmkt_arguments(line)
            if arguments is None:
                continue
            path = list(itertools.takewhile(lambda word: not word.startswith('-'), arguments))
            used = [word.lstrip('-').split('=')[0] for word in arguments if word.startswith('-')]
            if not used:
                continue
            known = flags_for(path)
            seen += 1
            problems.extend('%s has no --%s: %s' % (' '.join(path) or 'opsecmkt', flag, line) for flag in used if flag not in known)
        self.assertEqual(problems, [], '\n'.join(problems))
        self.assertGreater(seen, 20)

    def test_the_checks_can_fail(self):
        self.assertIn('path', flags_for(['deploy', 'storage', 'verify']))
        self.assertNotIn('dir', flags_for(['deploy', 'storage', 'verify']))
        self.assertIn('database-ca', flags_for(['deploy', 'endpoint', 'set']))
        self.assertNotIn('app-bind', flags_for(['deploy', 'endpoint', 'set']))
        self.assertNotIn('monero-rpc-ca', flags_for(['deploy', 'tls', 'replace']), 'the guide says no command replaces it')
        self.assertIn('request-deadline-recheck', flags_for([]))
        self.assertNotIn('"no-such-flag"', sources())
        self.assertIn('after-an-outage', targets(REPOSITORY / 'docs' / 'operator-guide.md'))
        self.assertNotIn('no-such-anchor', targets(REPOSITORY / 'docs' / 'operator-guide.md'))

    def test_no_command_names_mainnet_or_a_private_value(self):
        problems = []
        for line in commands(GUIDE):
            if re.search(r'\bmainnet\b', line):
                problems.append('mainnet: ' + line)
            if re.search(r'[a-z+]+://[^/\s]*:[^/\s]*@', line) or re.search(r'(?i)password\s*=', line):
                problems.append('credential: ' + line)
            for word in ('--setup-token ', '--app-secret-key ', 'cat /srv/opsecmkt/'):
                if word in line:
                    problems.append('private value: ' + line)
        self.assertEqual(problems, [], '\n'.join(problems))

    def test_scripts_exist(self):
        for script in set(re.findall(r'\bscripts/[\w.-]+', GUIDE)):
            self.assertTrue((REPOSITORY / script).exists(), script)

    def test_links_resolve(self):
        broken = []
        for target in re.findall(r'\]\(([^)\s]+)\)', GUIDE):
            if '://' in target:
                continue
            name, _, anchor = target.partition('#')
            path = (GUIDE_PATH.parent / name) if name else GUIDE_PATH
            if not path.exists() and name in PENDING:
                if anchor and anchor not in PENDING[name]:
                    broken.append(target)
                continue
            if not path.exists():
                broken.append(target)
            elif anchor and path.suffix == '.md' and anchor not in targets(path):
                broken.append(target)
        self.assertEqual(broken, [], 'links that do not resolve')
        self.assertIn('](encrypted-hosts.md#', GUIDE, 'host creation links to the encrypted-host guide')

    def test_linked_from_nav_index_and_the_neighbouring_guides(self):
        nav = (REPOSITORY / 'mkdocs.yml').read_text()
        recovery = nav.index('install/recovery.md')
        here = nav.index('install/replace-a-host.md')
        self.assertLess(recovery, here)
        between = nav[recovery:here].splitlines()
        self.assertEqual(len(between), 2, 'directly after Application configuration recovery: %r' % between)
        for guide in ('README.md', 'recovery.md', 'ram-secrets-and-emergency.md'):
            self.assertIn('(replace-a-host.md', (REPOSITORY / 'docs' / 'install' / guide).read_text(), guide)

    def test_acceptance_has_an_unticked_line(self):
        acceptance = (REPOSITORY / 'docs' / 'acceptance.md').read_text()
        lines = [line for line in acceptance.splitlines() if 'install/replace-a-host.md' in line]
        self.assertTrue(lines)
        self.assertTrue(all(line.startswith('- [ ]') for line in lines), lines)


if __name__ == '__main__':
    unittest.main()
