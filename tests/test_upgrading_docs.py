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


if __name__ == '__main__':
    unittest.main()
