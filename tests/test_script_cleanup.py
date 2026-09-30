"""Static checks on container cleanup in scripts/*.sh (A-305); no Docker needed.

`docker rm -f` without -v leaves the anonymous volume of an image that declares a VOLUME (postgres keeps its data
directory in one), so every test run leaked a volume. Each removal passes -v, or says on the same line why not.
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
REMOVAL = re.compile(r'\bdocker\s+(?:container\s+)?rm\b((?:\s+-[-\w]+)*)')


def removes_volumes(options):
    for option in options.split():
        if option == '--volumes' or (not option.startswith('--') and 'v' in option[1:]):
            return True
    return False


class ScriptCleanupTests(unittest.TestCase):
    def test_docker_rm_removes_anonymous_volumes_or_says_why_not(self):
        found = 0
        for path in sorted((REPOSITORY / 'scripts').glob('*.sh')):
            for number, line in enumerate(path.read_text().splitlines(), 1):
                if line.lstrip().startswith('#'):
                    continue
                for match in REMOVAL.finditer(line):
                    found += 1
                    with self.subTest(f'{path.name}:{number}'):
                        reason = re.search(r'\s#\s*\S', line[match.end():])
                        self.assertTrue(removes_volumes(match.group(1)) or reason,
                                        f'{path.name}:{number}: pass -v or give the reason on the line: {line.strip()}')
        self.assertGreater(found, 0, 'no docker rm found in scripts/*.sh')

    def test_option_parsing(self):
        for options, want in ((' -f', False), (' -f -v', True), (' -fv', True), (' -vf', True),
                              (' --force --volumes', True), (' --force', False), ('', False)):
            with self.subTest(options):
                self.assertEqual(removes_volumes(options), want)


if __name__ == '__main__':
    unittest.main()
