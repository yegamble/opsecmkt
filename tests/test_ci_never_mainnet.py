"""Development, tests and CI never run the installer on mainnet (A-523, owner directive 2026-10-13).

`scripts/install.sh init` defaults to mainnet for its local Monero daemon, so every real invocation in
.github/, scripts/ or tests/ must say it is not one: `--local`, `--nodes none`, or an explicit test network (for a
Monero daemon its options do not disable) and no explicit mainnet. tests/test_installer.py is the one exception, and only because its Docker is a stub (checked
below): it checks the configuration strings a mainnet install writes and never starts a node.
"""
import ast
from pathlib import Path
import re
import shlex
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
SCANNED = ('.github', 'scripts', 'tests')
STUBBED = REPOSITORY / 'tests' / 'test_installer.py'
SELF = Path(__file__).resolve()
INSTALLER = REPOSITORY / 'scripts' / 'install.sh'  # the program itself: its messages name commands, it runs none
SUFFIXES = ('.py', '.sh', '.bash', '.yaml', '.yml', '.ts', '.js', '.mjs')
TEST_NETWORKS = {'stagenet'}
# Options that decide whether init runs a mainnet node: their names and values must be literal to be checked.
GOVERNING = {'--local', '--nodes', '--monero', '--monero-network'}
# install.sh as a word of a shell command (a path ending in it), not part of another name.
SHELL_INVOCATION = re.compile(r'''(?:^|[\s;&|(`'"])(?:[\w./$-]*/)?install\.sh(?=$|[\s;&|)'"])(.*)$''')
# install.sh sourced (`source` or `.`) for its functions (scripts/test-tor-identity-import.sh): it returns before its
# command dispatch then, so it runs no init (test_sourced_installer_runs_no_command checks the guard).
SOURCED = re.compile(r'''(?:^|[\s;&|(])(?:source|\.)\s+(?:[\w./$-]*/)?install\.sh(?=$|[\s;&|)])''')
# A Python string that is itself a shell command running the installer, e.g. sh('./scripts/install.sh check', ...).
COMMAND_STRING = re.compile(r'^\s*(?:bash\s+)?(?:\./)?scripts/install\.sh(?:\s|$)')
SCRIPT_PATH = re.compile(r'(?:^|/)install\.sh$')


def mainnet_problem(arguments):
    """Why ARGUMENTS (the words after install.sh; None for a word that is not a literal string) may run init on
    mainnet, or None when they cannot. A non-literal word is accepted only as the value of a non-governing option."""
    for index, word in enumerate(arguments):
        previous = arguments[index - 1] if index else None
        if word is None and not (isinstance(previous, str) and previous.startswith('--') and previous not in GOVERNING):
            return 'an argument is not a literal string, so it cannot be checked'
    if arguments and arguments[0] != 'init' and not arguments[0].startswith('-'):
        return None  # a subcommand (check, status, upgrade, ...), or a word install.sh refuses with its usage
    if arguments and arguments[0] in ('-h', '--help'):
        return None
    if arguments and arguments[0] == 'init':
        arguments = arguments[1:]
    if '--local' in arguments:
        return None
    options = {word: arguments[index + 1] for index, word in enumerate(arguments[:-1])
               if word is not None and word.startswith('--')}
    if options.get('--nodes') == 'none':
        return None
    if options.get('--monero') == 'disabled':
        return None
    if options.get('--monero-network') in TEST_NETWORKS:
        return None
    return f'no --local, --nodes none or explicit test network ({" ".join(map(str, arguments)) or "no options"})'


def shell_invocations(text):
    """(line number, words) for each command line in TEXT that runs install.sh, continuation lines joined."""
    logical, start, found = '', 0, []
    for number, line in enumerate(text.splitlines(), 1):
        if not logical:
            start = number
        if line.rstrip().endswith('\\'):
            logical += line.rstrip()[:-1] + ' '
            continue
        logical += line
        stripped = logical.strip()
        logical = ''
        if stripped.startswith('#'):
            continue
        match = SHELL_INVOCATION.search(stripped)
        if match and not SOURCED.search(stripped):
            rest = re.split(r'\s(?:&&|\|\||\||>)|[;)`]', match.group(1))[0]
            try:
                words = shlex.split(rest)
            except ValueError:
                words = rest.split()
            found.append((start, words))
    return found


def is_script(node):
    return isinstance(node, ast.Constant) and isinstance(node.value, str) and SCRIPT_PATH.search(node.value)


def literal(node):
    return node.value if isinstance(node, ast.Constant) and isinstance(node.value, str) else None


def wrapper_names(trees):
    """Functions, in any scanned Python file, that run install.sh with the arguments they are given: their body holds a
    list such as ['bash', 'scripts/install.sh', *arguments]."""
    names = set()
    for tree in trees:
        for function in ast.walk(tree):
            if isinstance(function, ast.FunctionDef):
                for node in ast.walk(function):
                    if (isinstance(node, ast.List) and any(map(is_script, node.elts))
                            and any(isinstance(item, ast.Starred) for item in node.elts)):
                        names.add(function.name)
    return names


def python_invocations(tree, wrappers):
    """(line number, words) for each place in TREE that runs install.sh: a call to a wrapper, a list holding the script
    path (as subprocess takes it), or a string that is a shell command running it. A word that is not a literal string
    is None. A wrapper's own pass-through list is skipped; its callers are checked."""
    found, elements = [], set()
    for node in ast.walk(tree):
        if isinstance(node, ast.List):
            elements.update(id(item) for item in node.elts)
            if any(map(is_script, node.elts)) and not any(isinstance(item, ast.Starred) for item in node.elts):
                index = next(i for i, item in enumerate(node.elts) if is_script(item))
                found.append((node.lineno, [literal(item) for item in node.elts[index + 1:]]))
        elif isinstance(node, ast.Call):
            name = node.func.id if isinstance(node.func, ast.Name) else getattr(node.func, 'attr', None)
            if name in wrappers:
                values = [*node.args, *(keyword.value for keyword in node.keywords)]
                lists = [value for value in values if isinstance(value, ast.List)]
                found.append((node.lineno, [literal(item) for item in lists[0].elts] if len(lists) == 1 else [None]))
    for node in ast.walk(tree):
        if (isinstance(node, ast.Constant) and isinstance(node.value, str) and id(node) not in elements
                and COMMAND_STRING.match(node.value)):
            found.extend((node.lineno + offset - 1, words) for offset, words in shell_invocations(node.value))
    return found


def scanned_files():
    for directory in SCANNED:
        for path in sorted((REPOSITORY / directory).rglob('*')):
            if (path.is_file() and path.suffix in SUFFIXES and path.resolve() not in (STUBBED, SELF, INSTALLER)
                    and 'node_modules' not in path.parts):
                yield path


def repository_problems():
    """(path, line, problem) for every invocation that may run init on mainnet, and every (path, line, words) checked."""
    trees, problems, checked = {}, [], []
    for path in scanned_files():
        text = path.read_text(errors='replace')
        if 'install.sh' not in text:
            continue
        if path.suffix == '.py':
            trees[path] = ast.parse(text, str(path))
            continue
        for line, words in shell_invocations(text):
            checked.append((path, line, words))
    wrappers = wrapper_names(trees.values())
    for path, tree in trees.items():
        checked.extend((path, line, words) for line, words in python_invocations(tree, wrappers))
    for path, line, words in checked:
        problem = mainnet_problem(words)
        if problem:
            problems.append((path, line, problem))
    return problems, checked


class NeverMainnetTests(unittest.TestCase):
    def test_classifier_accepts_only_non_mainnet_inits(self):
        accepted = (['--local'], ['init', '--local', '--admin', 'a'], ['--tor', '--nodes', 'none'], ['check'],
                    ['upgrade', '--no-backup'], ['status'], ['--help'],
                    ['--tor', '--monero', 'disabled'],
                    ['--tor', '--monero', 'local', '--monero-network', 'stagenet'],
                    ['--tor', '--monero-image', None, '--nodes', 'none'],
                    ['--tor', '--nodes', 'local', '--monero-network', 'stagenet'])
        refused = ([], ['init'], ['--tor'], ['--tor', '--nodes', 'local'], ['--nodes', 'custom'],
                   ['--tor', '--nodes', 'local', '--monero-network', 'mainnet'],
                   ['--tor', '--monero', 'local'], [None], [None, '--local'], ['--tor', '--nodes', None],
                   ['--tor', '--monero-network', None], ['--tor', None, None, '--nodes', 'none'])
        for arguments in accepted:
            with self.subTest(accepted=arguments):
                self.assertIsNone(mainnet_problem(arguments))
        for arguments in refused:
            with self.subTest(refused=arguments):
                self.assertIsNotNone(mainnet_problem(arguments))

    def test_scanner_finds_shell_and_python_invocations(self):
        shell = ('# ./scripts/install.sh --tor (a comment)\n'
                 'bash scripts/install.sh --tor \\\n  --nodes local && echo done\n'
                 '(cd "$work" && ./scripts/install.sh check) || fail\n'
                 'cp scripts/install.sh "$work/scripts/"\n'
                 '    ./scripts/install.sh --local\n'
                 'run: ./scripts/install.sh\n'
                 '(source scripts/install.sh; onion_key_check "$dir")\n'
                 '. ./scripts/install.sh\n')
        found = shell_invocations(shell)
        self.assertEqual(found, [(2, ['--tor', '--nodes', 'local']), (4, ['check']), (5, ['$work/scripts/']),
                                 (6, ['--local']), (7, [])])
        self.assertEqual([line for line, words in found if mainnet_problem(words)], [2, 7])
        source = ('def installer(directory, arguments):\n'
                  "    return run(['bash', 'scripts/install.sh', *arguments])\n"
                  "installer(repo, ['--tor', '--nodes', 'local'])\n"
                  "installer(repo, ['--local'])\n"
                  "subprocess.run(['./scripts/install.sh', '--clearnet'])\n"
                  'installer(repo, options)\n'
                  "sh('./scripts/install.sh check', repo)\n"
                  "require(ok, 'install.sh init with a local regtest node failed')\n")
        tree = ast.parse(source)
        found = sorted(python_invocations(tree, wrapper_names([tree])), key=lambda item: item[0])
        self.assertEqual(found, [(3, ['--tor', '--nodes', 'local']), (4, ['--local']), (5, ['--clearnet']), (6, [None]),
                                 (7, ['check'])])

    def test_no_installer_init_in_ci_scripts_or_tests_can_run_mainnet(self):
        problems, checked = repository_problems()
        self.assertEqual(problems, [], '\n'.join(f'{path.relative_to(REPOSITORY)}:{line}: {problem}'
                                                 for path, line, problem in problems))
        # Not vacuous: the real installs CI runs are among those checked.
        inits = {path.relative_to(REPOSITORY).as_posix() for path, _, words in checked
                 if not words or words[0] == 'init' or (words[0] or '').startswith('--')}
        self.assertIn('scripts/test-install.py', inits)

    def test_sourced_installer_runs_no_command(self):
        # The exception for a sourced install.sh above holds only while it returns before it dispatches any command.
        text = INSTALLER.read_text()
        guard = text.index('\nif [[ ${BASH_SOURCE[0]} != "$0" ]]; then return 0; fi\n')
        self.assertLess(guard, text.index('\nisolate_env\ncase ${1-} in\n'))
        self.assertEqual(text.count('\ncase ${1-} in\n'), 1)

    def test_installer_unit_tests_stub_docker(self):
        # The exception above holds only while tests/test_installer.py runs the installer against its Docker stub.
        text = STUBBED.read_text()
        self.assertIn("'PATH': str(self.bin) + os.pathsep + os.defpath", text)
        self.assertIn("self.stub('docker', '''", text)
        self.assertIn("sys.exit('Unexpected docker command')", text)


if __name__ == '__main__':
    unittest.main()
