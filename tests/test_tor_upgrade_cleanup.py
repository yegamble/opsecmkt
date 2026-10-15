"""Execute the rehearsal's exit cleanup against version-skewed Compose resources."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


REPOSITORY = Path(__file__).resolve().parents[1]
SCRIPT = REPOSITORY / "scripts" / "test-tor-upgrade-rollback.sh"


class TorUpgradeCleanupTests(unittest.TestCase):
    def run_cleanup(self, *, fail_removal=False, original_status=0):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        work = root / "rehearsal"
        (work / "deployment").mkdir(parents=True)
        state = root / "volumes.json"
        owned = "a144-cleanup_retention_state"
        unrelated = "another-project_retention_state"
        state.write_text(json.dumps({owned: "a144-cleanup", unrelated: "another-project"}))
        calls = root / "docker-calls.jsonl"
        compose_calls = root / "compose-calls.txt"
        binaries = root / "bin"
        binaries.mkdir()
        docker = binaries / "docker"
        docker.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
with open(os.environ['DOCKER_CALLS'], 'a') as log:
    log.write(json.dumps(args) + '\\n')
state = pathlib.Path(os.environ['DOCKER_STATE'])
volumes = json.loads(state.read_text())
if args[:2] in (['volume', 'ls'], ['network', 'ls']) or args[:1] == ['ps']:
    assert args[-2:] == ['--filter', 'label=com.docker.compose.project=a144-cleanup'], args
    if args[:2] == ['volume', 'ls']:
        print('\\n'.join(name for name, project in volumes.items() if project == 'a144-cleanup'))
elif args[:2] == ['volume', 'rm']:
    assert len(args) == 3, args
    if os.environ['FAIL_VOLUME_REMOVE'] == '1':
        print('volume is in use', file=sys.stderr)
        sys.exit(1)
    del volumes[args[2]]
    state.write_text(json.dumps(volumes))
elif args[:2] == ['image', 'inspect']:
    sys.exit(1)
else:
    raise AssertionError(args)
""")
        docker.chmod(0o755)
        source = SCRIPT.read_text()
        cleanup = source[source.index("cleanup() {"):source.index("\ntrap cleanup EXIT INT TERM")]
        harness = """set -euo pipefail
work=$1
deploy=$work/deployment
runtime=$work/runtime
cleanup_log=$work/cleanup.log
project=a144-cleanup
alpha1_app_image_ref=alpha-image
current_app_image_ref=current-image
restore_runtime() { :; }
compose() { printf '%s\\n' "$*" >> "$COMPOSE_CALLS"; }
""" + cleanup + "\ntrap cleanup EXIT\nexit \"$ORIGINAL_STATUS\"\n"
        result = subprocess.run(
            ["bash", "-c", harness, "tor-cleanup-test", str(work)],
            text=True, capture_output=True, cwd=REPOSITORY,
            env=dict(os.environ, PATH=f"{binaries}{os.pathsep}{os.environ['PATH']}",
                     DOCKER_STATE=str(state), DOCKER_CALLS=str(calls), COMPOSE_CALLS=str(compose_calls),
                     FAIL_VOLUME_REMOVE=str(int(fail_removal)), ORIGINAL_STATUS=str(original_status)),
        )
        self.assertEqual(compose_calls.read_text(), "down --rmi local --volumes --remove-orphans\n")
        commands = [json.loads(line) for line in calls.read_text().splitlines()]
        self.assertEqual([args for args in commands if args[:2] == ["volume", "rm"]],
                         [["volume", "rm", owned]])
        self.assertEqual(json.loads(state.read_text()).get(unrelated), "another-project")
        return result, work, json.loads(state.read_text()), owned

    def test_removes_new_version_volume_after_old_compose_down(self):
        result, work, volumes, owned = self.run_cleanup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(owned, volumes)
        self.assertFalse(work.exists())

    def test_volume_removal_failure_keeps_evidence_and_fails(self):
        result, work, volumes, owned = self.run_cleanup(fail_removal=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(owned, volumes)
        self.assertTrue(work.exists())
        self.assertIn("cleanup left project resources", result.stderr)
        self.assertIn("volume is in use", (work / "cleanup.log").read_text())

    def test_successful_cleanup_preserves_original_failure(self):
        result, work, volumes, owned = self.run_cleanup(original_status=7)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertNotIn(owned, volumes)
        self.assertFalse(work.exists())


if __name__ == "__main__":
    unittest.main()
