"""Static checks on container cleanup in scripts/*.sh (A-305, A-311); no Docker needed.

`docker rm -f` without -v leaves the anonymous volume of an image that declares a VOLUME (postgres keeps its data
directory in one), so every test run leaked a volume. Each removal passes -v, or says on the same line why not, in a
comment starting "# no -v:".
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
REMOVAL = re.compile(r'\bdocker\s+(?:container\s+)?rm\b((?:\s+-[-\w]+)*)')
# The reason must say why not: "# -v: postgres keeps …" explains a -v, so it stays true when the -v is dropped (A-311).
WHY_NOT = re.compile(r'\s#\s*no -v\b')


def removes_volumes(options):
    for option in options.split():
        if option == '--volumes' or (not option.startswith('--') and 'v' in option[1:]):
            return True
    return False


def unexplained_removals(line):
    """Count the docker rm commands on a script line that neither pass -v nor say why not."""
    if line.lstrip().startswith('#'):
        return 0, 0
    found = bad = 0
    for match in REMOVAL.finditer(line):
        found += 1
        if not removes_volumes(match.group(1)) and not WHY_NOT.search(line[match.end():]):
            bad += 1
    return found, bad


class ScriptCleanupTests(unittest.TestCase):
    def test_docker_rm_removes_anonymous_volumes_or_says_why_not(self):
        total = 0
        for path in sorted((REPOSITORY / 'scripts').glob('*.sh')):
            for number, line in enumerate(path.read_text().splitlines(), 1):
                found, bad = unexplained_removals(line)
                total += found
                if found:
                    with self.subTest(f'{path.name}:{number}'):
                        self.assertEqual(bad, 0, f'{path.name}:{number}: pass -v or say "# no -v: <reason>" '
                                                 f'on the line: {line.strip()}')
        self.assertGreater(total, 0, 'no docker rm found in scripts/*.sh')

    def test_only_a_no_v_comment_excuses_a_removal_without_v(self):
        for line, want in (('docker rm -f x', 1),
                           ('docker rm -f x  # -v: reason', 1),
                           ('docker rm -f x  # removes the container', 1),
                           ('docker rm -f x  # no -v: reason', 0),
                           ('docker rm -f -v x  # -v: reason', 0),
                           ('docker rm -fv x', 0),
                           ('# docker rm -f x', 0),
                           ('docker rm -f a; docker rm -f -v b  # no -v: a has no VOLUME', 0)):
            with self.subTest(line):
                self.assertEqual(unexplained_removals(line)[1], want)

    def test_option_parsing(self):
        for options, want in ((' -f', False), (' -f -v', True), (' -fv', True), (' -vf', True),
                              (' --force --volumes', True), (' --force', False), ('', False)):
            with self.subTest(options):
                self.assertEqual(removes_volumes(options), want)


if __name__ == '__main__':
    unittest.main()
