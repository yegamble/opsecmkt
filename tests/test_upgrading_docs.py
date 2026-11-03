"""UPGRADING.md's alpha.2 section is for a Monero-only release (A-696; QA95-4, DA95-7).

The next tag points operators at "Upgrading from v0.1.0-alpha.1 to v0.1.0-alpha.2". It used to tell them to set
Bitcoin and pooled-wallet variables that internal/market/config.go now refuses at startup (refuseBitcoinEnv,
refuseObsoletePooledEnv), so following it stopped the upgraded server. The refused names are read from config.go
itself, so a new refusal is checked here without editing this file:

* no line of the alpha.2 section assigns a refused variable a value, tells the reader to set or add one, or adds a
  refused Compose profile (or the installer-refused `monero-wallet` profile);
* section 3, which every alpha.1 upgrade reads, names every refused variable and profile to remove;
* the section numbers that code and scripts cite still exist, and every in-page or cross-document link into
  UPGRADING.md resolves to a heading (several sections were renamed).
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
UPGRADING = (REPOSITORY / 'UPGRADING.md').read_text()
CONFIG = (REPOSITORY / 'internal' / 'market' / 'config.go').read_text()
HEADING = '# Upgrading from v0.1.0-alpha.1 to `v0.1.0-alpha.2`'


def function_body(name):
    match = re.search(r'^func %s\(.*?^}\n' % re.escape(name), CONFIG, re.M | re.S)
    assert match, 'config.go has no func %s' % name
    return match.group(0)


def refused_names():
    names = set()
    for function in ('refuseBitcoinEnv', 'refuseObsoletePooledEnv'):
        listing = re.search(r'\[\]string\{(.*?)\}', function_body(function), re.S)
        assert listing, 'no name list in %s' % function
        names.update(re.findall(r'"([A-Z0-9_]+)"', listing.group(1)))
    return names


def refused_profiles():
    return set(re.findall(r'profile == "([a-z-]+)"', function_body('refuseBitcoinEnv')))


def alpha2_section():
    start = UPGRADING.index(HEADING)
    end = UPGRADING.find('\n# ', start + 1)
    return UPGRADING[start:] if end < 0 else UPGRADING[start:end]


def numbered_section(number):
    text = alpha2_section()
    match = re.search(r'^## %d\. .*?(?=^## |\Z)' % number, text, re.M | re.S)
    assert match, 'the alpha.2 section has no "## %d." heading' % number
    return match.group(0)


def slug(heading):
    """GitHub's heading anchor: lower case, punctuation other than - and _ dropped, spaces to hyphens."""
    text = re.sub(r'[^\w\- ]', '', heading.strip().lower())
    return text.replace(' ', '-')


def anchors(text):
    seen, result = {}, set()
    in_code = False
    for line in text.splitlines():
        if line.startswith('```'):
            in_code = not in_code
            continue
        match = None if in_code else re.match(r'^#{1,6} (.*)$', line)
        if match:
            base = slug(match.group(1))
            count = seen.get(base, 0)
            seen[base] = count + 1
            result.add(base if count == 0 else '%s-%d' % (base, count))
    return result


EMPTY = {'', "''", '""'}
AFTER_UPGRADE_ANCHORS = (
    'staff-need-a-second-factor-before-staff-actions-a-706',
    'the-image-captcha-now-appears-only-under-attack-migration-155-a-725',
    'digital-escrow-orders-get-a-delivery-deadline-and-a-dispute-window-migration-153-a-709',
    'required-pgp-access-and-vendor-privacy',
    'site-access-and-registration',
    'vendor-onboarding',
    'market-branding',
)
POOLER_ANCHOR = 'staff-pages-no-longer-wait-for-postgresqls-jit-compiler-mod72-8'


class UpgradingAlpha2Test(unittest.TestCase):
    def test_config_refusals_are_read(self):
        names = refused_names()
        for name in ('ESCROW_BTC', 'BITCOIN_RPC_URL', 'BITCOIN_RPC_PASSWORD', 'ESCROW_SIGNER_SEED_FILE',
                     'MONERO_RPC_URL', 'MONERO_WALLET_RPC_URL'):
            self.assertIn(name, names)
        self.assertEqual(refused_profiles(), {'bitcoin', 'escrow-signer'})

    def test_no_instruction_sets_a_refused_variable(self):
        names = sorted(refused_names(), key=len, reverse=True)
        alternation = '|'.join(map(re.escape, names))
        assignment = re.compile(r'(?<![A-Z0-9_])(%s)=([^\s`|,)]*)' % alternation)
        prose = re.compile(r'\b(?:[Ss]et|[Aa]dd|[Pp]ut|[Gg]ive|[Pp]oint|[Ww]rite|[Ee]nable)\b[^.;:]{0,40}?`(%s)`' % alternation)
        profiles = '|'.join(map(re.escape, sorted(refused_profiles() | {'monero-wallet'})))
        profile_value = re.compile(r'COMPOSE_PROFILES=[\'"]?([^\'"\s`]*)')
        profile_prose = re.compile(r'\b(?:[Aa]dd|[Ee]nable|[Kk]eep)\b[^.;:]{0,30}?`(%s)`' % profiles)
        problems = []
        for number, line in enumerate(alpha2_section().splitlines(), 1):
            for name, value in assignment.findall(line):
                value = value.rstrip("'\".")
                if value + "''" in EMPTY or value in EMPTY or (name == 'ESCROW_BTC' and value in ('off', 'false')):
                    continue
                problems.append('line %d sets %s=%s: %s' % (number, name, value, line.strip()))
            for name in prose.findall(line):
                problems.append('line %d tells the reader to set %s: %s' % (number, name, line.strip()))
            for value in profile_value.findall(line):
                for profile in value.split(','):
                    if profile in refused_profiles() | {'monero-wallet'}:
                        problems.append('line %d adds the %s profile: %s' % (number, profile, line.strip()))
            for profile in profile_prose.findall(line):
                problems.append('line %d adds the %s profile: %s' % (number, profile, line.strip()))
        self.assertEqual(problems, [], '\n'.join(problems))

    def test_section_3_names_everything_to_remove(self):
        section = numbered_section(3)
        missing = sorted(n for n in refused_names() | {'`%s`' % p for p in refused_profiles()} if n not in section)
        self.assertEqual(missing, [], 'section 3 does not name these refused settings')
        self.assertIn('docker compose config --quiet', section)

    def test_summary_describes_a_monero_only_release(self):
        summary = alpha2_section()[:alpha2_section().index('## Migrations and changes in detail')]
        flat = ' '.join(summary.split())
        self.assertIn('Monero (XMR) is the only payment currency', flat)
        self.assertIn('Bitcoin is removed', flat)
        self.assertNotIn('Bitcoin uses 2-of-3', flat)
        self.assertIn('historical', flat)

    def test_summary_names_what_to_do_after_upgrading(self):
        # A-742 (OPS104-2, DA104-6): a reader told to read "this summary and the sections it names" must be sent to
        # each action a section requires, and alpha.1 has no second factor to enrol before upgrading.
        summary = alpha2_section()[:alpha2_section().index('## Migrations and changes in detail')]
        self.assertIn('**After you upgrade**', summary)
        after = summary[summary.index('**After you upgrade**'):summary.index('**Rollback rule:**')]
        targets = anchors(UPGRADING)
        for anchor in AFTER_UPGRADE_ANCHORS:
            self.assertIn('](#%s)' % anchor, after)
            self.assertIn(anchor, targets)
        flat = ' '.join(after.split())
        self.assertIn('From alpha.1, enrol after upgrading, not before', flat)
        self.assertIn('password-only window', flat)
        self.assertNotIn('described above this heading', summary)
        # A pooler stops the upgraded server at startup, so its section is named before the upgrade.
        before = summary[summary.index('**Before you upgrade**'):summary.index('**After you upgrade**')]
        self.assertIn('](#%s)' % POOLER_ANCHOR, before)
        self.assertIn(POOLER_ANCHOR, targets)

    def test_staff_factor_section_does_not_ask_alpha1_to_enrol_first(self):
        start = UPGRADING.index('## Staff need a second factor before staff actions (A-706)')
        section = ' '.join(UPGRADING[start:UPGRADING.index('\n## ', start + 1)].split())
        self.assertNotIn('Before or right after upgrading', section)
        self.assertIn('alpha.1 has no second factor', section)
        self.assertIn('`./scripts/install.sh upgrade` and `status` print the same warning', section)

    def test_cited_section_numbers_still_exist(self):
        # Cited by internal/market (sections 6, 14, 18, 42, 59), scripts/ and cmd/server (section 6, "Rolling back").
        for number in (1, 2, 3, 4, 5, 6, 14, 18, 42, 59):
            numbered_section(number)
        self.assertIn('## 5. Rolling back', alpha2_section())

    def test_links_into_upgrading_resolve(self):
        targets = anchors(UPGRADING)
        broken = ['#' + a for a in re.findall(r'\]\(#([^)]+)\)', UPGRADING) if a not in targets]
        for path in [REPOSITORY / 'README.md', *sorted((REPOSITORY / 'docs').rglob('*.md'))]:
            if 'evidence' in path.parts:
                continue
            for anchor in re.findall(r'UPGRADING\.md#([A-Za-z0-9_-]+)', path.read_text()):
                if anchor not in targets:
                    broken.append('%s: #%s' % (path.relative_to(REPOSITORY), anchor))
        self.assertEqual(broken, [], 'links to UPGRADING.md headings that do not exist')

    def test_outage_recheck_has_a_command_and_one_story(self):
        # OPS104-1, DA104-3: db_sql is bundled-database only and a mistyped value reads as a restore's, so no guide tells
        # an operator to write the recheck's request by hand; each gives the host command for Compose and for a native
        # executable, and all three tell the same story of the time Monero escrow is off.
        main = (REPOSITORY / 'cmd' / 'server' / 'main.go').read_text()
        self.assertIn('flag.Bool("request-deadline-recheck"', main)
        for path in [REPOSITORY / 'README.md', REPOSITORY / 'UPGRADING.md', *sorted((REPOSITORY / 'docs').rglob('*.md'))]:
            if 'evidence' in path.parts:
                continue
            self.assertNotRegex(path.read_text(), r"INSERT INTO settings\(key,value\) VALUES \('escrow_xmr_release_recheck'",
                                path.relative_to(REPOSITORY))
        for name in ('UPGRADING.md', 'docs/operator-guide.md', 'docs/testnet-runbook.md'):
            flat = ' '.join((REPOSITORY / name).read_text().split())
            for want in ('docker compose run --rm --no-deps app -request-deadline-recheck',
                         'DATABASE_URL_FILE=/srv/opsecmkt-inputs/database.url ./opsecmkt -request-deadline-recheck',
                         'for the database the installation uses now',
                         '/srv/opsecmkt-inputs/database-rollback.url',
                         'Do not use `DIR/secrets/database-url`',
                         'A native development installation',
                         'this command is not available there',
                         'confirm on **Admin → Payments**',
                         'Repeat it within every 72 hours until ingress returns',
                         'run it once more when ingress returns',
                         'the request stays pending until the first pass after it is on again',
                         "a buyer's no-ship cancel past its time waits until then",
                         'the jump plus 72 hours',
                         # PAY113S-1: a clock left ahead also pays early; a recheck alone only delays.
                         'a deadline also falls due early by true time',
                         # PAY108-1: the wait for the clocks, its tolerance and how it ends.
                         'more than 5 minutes',
                         '72 hours have passed on both clocks',
                         'market-wide',
                         'no form undoes it',
                         'It cannot undo a completion',
                         "A cancel is accepted unless a pending recheck holds it or the vendor's grace from an earlier recheck has not ended"
                         if name != 'docs/testnet-runbook.md' else
                         "a cancel is accepted unless a pending recheck holds it or the vendor's grace from an earlier recheck has not ended",
                         'marking an order shipped answers 404',
                         'if it was off for longer than the threshold, the recheck runs on the first pass after it is on again'
                         if name == 'docs/testnet-runbook.md' else
                         'If it was off for longer than the threshold, the recheck runs on the first pass after it is on again'):
                self.assertIn(want, flat, name)
            self.assertNotIn('The recheck runs on the first pass after it is on again', flat, name)
            # QA108-1: a false recheck is not bounded by 72 hours, and A-763 leaves cases where a cancel can still relay.
            self.assertNotIn('a delay of up to 72 hours', flat, name)
            self.assertNotIn('with no coins moved', flat, name)
            self.assertNotIn('./private/database.url ./opsecmkt -request-deadline-recheck', flat, name)
            # PM-3, DA108-3, SRE110-3, SRE110-N1, QA108-2, PAY109-2, DA108-1.
            for want in ('one audit row with no actor', 'Monero deadline recheck requested from the host',
                         "docker compose logs app | grep 'recheck'", 'monero escrow deadline recheck: cause=outage moved=N',
                         'keeps no application logs', '/srv/opsecmkt-inputs/database-restored.url',
                         'Each repeat that moves a deadline notifies', 'no request has been made',
                         # PAY113R-1, PAY113R-2: the margin, and a clock that stepped back.
                         'an order overdue since more than the outage threshold before the gap began',
                         "The database's clock found more than 5 minutes behind the stored step drops any recorded start",
                         # QA112-5: a pause of one host is an outage too, and the claim is reasoning.
                         "A pause of only the application or database host is also an outage, and its recheck can wait until that host's clock is corrected; this is reasoning, not measured (A-765)"):
                self.assertIn(want, flat, name)
            # QA112-1: no guide says a peerless daemon stops nothing without the restricted RPC's exception.
            self.assertNotIn('not synced or without peers', flat, name)
            if name != 'docs/testnet-runbook.md':
                self.assertIn("A restricted RPC", flat, name)
                self.assertIn("cannot show its peers, so a synced one with an old tip waits, at most 72 hours: check the daemon's peers", flat, name)
                self.assertIn("the administrators' notice", flat, name)
            self.assertNotIn('writes the request and nothing else', flat, name)
            self.assertNotIn('So that one can be seen', flat, name)
        # OPS105-1: after a native rollback database.url still names the upgraded database.
        rollback = ' '.join(UPGRADING[UPGRADING.index('### Rolling back a native installation'):
                                      UPGRADING.index('## Native production encryption prerequisite')].split())
        self.assertIn('Until you roll forward, `/srv/opsecmkt-inputs/database-rollback.url` is the file for the database the installation uses now', rollback)
        self.assertIn('-request-deadline-recheck', rollback)
        self.assertIn('-request-deadline-recheck', (REPOSITORY / 'docs' / 'install' / 'cli-reference.md').read_text())
        # SRE110-5: the install guides reach the paragraph itself. PM-N3: the section table names the recheck.
        guide = (REPOSITORY / 'docs' / 'operator-guide.md').read_text()
        self.assertIn('<a id="after-an-outage"></a>', guide)
        for name in ('cli-reference.md', 'production.md', 'tor.md'):
            self.assertIn('../operator-guide.md#after-an-outage', (REPOSITORY / 'docs' / 'install' / name).read_text(), name)
        self.assertNotIn('writes that request and nothing else', (REPOSITORY / 'docs' / 'install' / 'cli-reference.md').read_text())
        self.assertRegex(guide, r'\| Payments \| `/admin/payments` \|[^\n]*\*Monero deadline recheck\*')

    def test_never_enrolled_warning_has_a_section(self):
        # A-782 SRE113-4: an upgrade is when the warning first appears, so UPGRADING.md says what it means. The phrases the
        # guide loop of scripts/test-backup-restore.sh greps for are on one line each there, and the section links its guide.
        match = re.search(r'^## [^\n]*A-772[^\n]*\n(.*?)(?=^#)', UPGRADING, re.M | re.S)
        self.assertTrue(match, 'UPGRADING.md has no A-772 section')
        section = match.group(1)
        for line_phrase in ('cannot reopen once the original database is lost', 'no other path exists today', '-enroll-retention'):
            self.assertRegex(section, r'(?m)^.*%s.*$' % re.escape(line_phrase))
        self.assertIn('docs/retention.md#before-enabling', section)
        self.assertIn('warning: this database has never enrolled retention', section)
        self.assertIn('earlier backups', ' '.join(section.split()))
        self.assertIn('RETENTION_ENABLED=on', section)

    def test_module_download_limit_has_a_section(self):
        # A-786 SRE114-2: an upgrade that fails in go-fetch says "the message above says why"; this entry names the setting.
        match = re.search(r'^## [^\n]*A-782[^\n]*\n(.*?)(?=^#)', UPGRADING, re.M | re.S)
        self.assertTrue(match, 'UPGRADING.md has no A-782 section')
        section = match.group(1)
        self.assertIn('GO_FETCH_TIMEOUT=3600', section)
        self.assertIn('(docs/install/troubleshooting.md#first-install-failed)', section)

    def test_recheck_fallback_finds_a_wait(self):
        # A-782 SRE113-1: with the panel behind the failed ingress, each guide's fallback must see the wait as well as
        # the run. The grep is read from the guide and tried on the log formats themselves, and the SQL names the third
        # key; cmd/server/status_test.go TestStatusNamesRecheckWait runs that SQL against a wait row.
        source = (REPOSITORY / 'internal' / 'market' / 'escrow_xmr_deadline.go').read_text()
        lines = re.findall(r'log\.Printf\("(monero escrow [^"]*recheck[^"]*)"', source)
        self.assertTrue(any(line.startswith('monero escrow recheck deferred: ') for line in lines), lines)
        self.assertTrue(any(line.startswith('monero escrow deadline recheck: cause=') for line in lines), lines)
        for name in ('UPGRADING.md', 'docs/operator-guide.md', 'docs/testnet-runbook.md'):
            flat = ' '.join((REPOSITORY / name).read_text().split())
            match = re.search(r"`docker compose logs app \| grep '([^']+)'` shows `monero escrow deadline recheck", flat)
            self.assertTrue(match, name)
            for line in lines:
                self.assertIn(match.group(1), line, name)
            self.assertIn('`monero escrow recheck deferred: …`', flat, name)
            self.assertIn("'escrow_xmr_release_recheck','escrow_xmr_last_recheck','escrow_xmr_recheck_wait')`", flat, name)
            self.assertIn('`-status`', flat, name)

    def test_smoke_runs_the_documented_recheck_request(self):
        # SRE110-4: the literal Compose form, its prefix and no URL, and the setting does not stay behind.
        smoke = (REPOSITORY / 'scripts' / 'ci-smoke.sh').read_text()
        self.assertIn('"${compose[@]}" run --rm --no-deps app -request-deadline-recheck', smoke)
        self.assertIn('"Deadline recheck now pending (outage)."*', smoke)
        self.assertIn('postgres(ql)?://', smoke)
        self.assertIn("DELETE FROM settings WHERE key='escrow_xmr_release_recheck'", smoke)


if __name__ == '__main__':
    unittest.main()
