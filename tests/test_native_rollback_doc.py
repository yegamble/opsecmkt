"""The native rollback procedure names only commands that exist (A-695).

UPGRADING.md "Rolling back a native installation" is a numbered command
sequence an operator follows during an incident. A step naming a deploy
subcommand, flag, host command or script the binary does not have strands the
operator half way. This reads the commands between the
`<!-- native-rollback-commands -->` markers and checks each against the Go
source (deploy usage, flag definitions, server host flags), the repository's
scripts and docs/install/cli-reference.md. No Go toolchain or database needed.
"""

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
UPGRADING = ROOT / "UPGRADING.md"
OPERATOR = ROOT / "internal" / "operator"
SERVER_MAIN = ROOT / "cmd" / "server" / "main.go"
CLI_REFERENCE = ROOT / "docs" / "install" / "cli-reference.md"

BLOCK = re.compile(r"<!-- native-rollback-commands -->\n(.*?)<!-- /native-rollback-commands -->", re.S)
FENCE = re.compile(r"^ *```sh\n(.*?)^ *```", re.S | re.M)
# Subcommands of a nested deploy command, read from its dispatcher
# (`args[0] != "set"` in endpoint.go, `args[0] != "replace"` in tls_replace.go).
NESTED = {"endpoint": "endpoint.go", "tls": "tls_replace.go"}


def rollback_commands():
    match = BLOCK.search(UPGRADING.read_text(encoding="utf-8"))
    if match is None:
        raise AssertionError("UPGRADING.md has no native-rollback-commands block")
    commands = []
    for body in FENCE.findall(match.group(1)):
        joined = re.sub(r"\\\n\s*", " ", body)
        commands.extend(line.strip() for line in joined.splitlines() if line.strip())
    return match.group(1), commands


def deploy_commands():
    source = (OPERATOR / "operator.go").read_text(encoding="utf-8")
    usage = re.search(r"const usage = `(.*?)`", source, re.S).group(1)
    return set(re.findall(r"^  ([a-z][a-z-]*) ", usage, re.M))


def operator_flags():
    names = set()
    for path in OPERATOR.glob("*.go"):
        if path.name.endswith("_test.go"):
            continue
        names.update(re.findall(r'\.(?:String|Bool|Int|Var)\(\s*(?:&\w+,\s*)?"([a-z][a-z-]*)"', path.read_text(encoding="utf-8")))
    return names


def server_flags():
    return set(re.findall(r'flag\.(?:Bool|String)\("([a-z][a-z-]*)"', SERVER_MAIN.read_text(encoding="utf-8")))


class NativeRollbackCommandsTest(unittest.TestCase):
    def setUp(self):
        self.block, self.commands = rollback_commands()

    def test_block_is_numbered_and_has_commands(self):
        steps = re.findall(r"^(\d+)\. ", self.block, re.M)
        self.assertEqual(steps, [str(i) for i in range(1, len(steps) + 1)], "steps must be numbered 1..n")
        self.assertGreaterEqual(len(steps), 5)
        self.assertTrue(any("deploy endpoint set" in c for c in self.commands))

    def test_every_deploy_subcommand_and_flag_exists(self):
        commands, flags = deploy_commands(), operator_flags()
        reference = CLI_REFERENCE.read_text(encoding="utf-8")
        seen = 0
        for line in self.commands:
            match = re.search(r"\./opsecmkt deploy ([a-z-]+)(?: ([a-z-]+))?", line)
            if not match:
                continue
            seen += 1
            command, sub = match.group(1), match.group(2)
            self.assertIn(command, commands, line)
            name = command
            if command in NESTED:
                dispatcher = (OPERATOR / NESTED[command]).read_text(encoding="utf-8")
                self.assertIn(f'args[0] != "{sub}"', dispatcher, line)
                name = f"{command} {sub}"
            self.assertIn(f"| `{name}", reference, f"cli-reference.md does not describe deploy {name}")
            for flag in re.findall(r"(?<!\S)--([a-z][a-z-]*)", line):
                self.assertIn(flag, flags, f"{line}: --{flag} is not a deploy flag")
        self.assertGreaterEqual(seen, 4)

    def test_host_commands_and_scripts_exist(self):
        host = server_flags()
        found = 0
        for line in self.commands:
            for flag in re.findall(r"\./opsecmkt -([a-z][a-z-]*)", line):
                found += 1
                self.assertIn(flag, host, f"{line}: -{flag} is not a server host flag")
            for script in re.findall(r"\./(scripts/[\w.-]+)", line):
                self.assertTrue((ROOT / script).is_file(), f"{line}: {script} does not exist")
        self.assertGreaterEqual(found, 2)

    def test_restore_script_names_the_native_command(self):
        text = (ROOT / "scripts" / "restore.sh").read_text(encoding="utf-8")
        self.assertIn("opsecmkt deploy endpoint set --dir DIR --database-url-file FILE", text)
        self.assertIn("RESTORE_DATABASE_URL", self.block)


if __name__ == "__main__":
    unittest.main()
