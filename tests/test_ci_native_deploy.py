"""The native deployment controller meets a real Docker Engine in CI (A-697).

Static checks, as text: the container job runs scripts/test-native-deploy.sh once with Go and a Tor image built the
way the tor-image job builds it; the script renders with the Go test it names, uses digest-pinned images, drives the
commands the acceptance check names and cleans up only its own project. The run itself is CI's evidence.
"""
from pathlib import Path
import re
import unittest

from test_ci_go_split import job_block

ROOT = Path(__file__).resolve().parents[1]
CI = (ROOT / '.github' / 'workflows' / 'ci.yaml').read_text()
SCRIPT = (ROOT / 'scripts' / 'test-native-deploy.sh').read_text()
ROW = "if: matrix.database == 'external'"


def steps(block):
    """The job's steps, each as its text from `- ` to the next step."""
    body = block.split('\n    steps:\n', 1)[1]
    return re.split(r'\n(?=      - )', body)


class NativeDeployJobTest(unittest.TestCase):
    def setUp(self):
        self.job = job_block(CI, 'container')
        self.steps = steps(self.job)

    def step(self, needle):
        found = [s for s in self.steps if needle in s]
        self.assertEqual(len(found), 1, needle)
        return found[0]

    def test_runs_once_with_go_and_the_tor_image(self):
        run = self.step('run: bash scripts/test-native-deploy.sh')
        self.assertIn(ROW, run)
        self.assertRegex(run, r'\n        timeout-minutes: \d+\n')
        go = self.step('actions/setup-go@')
        self.assertIn(ROW, go)
        self.assertIn('go-version-file: go.mod', go)
        tor = self.step('file: deploy/Dockerfile.tor')
        self.assertIn(ROW, tor)
        self.assertIn('tags: opsecmkt-tor:ci', tor)
        self.assertIn('SECURITY_REFRESH=${{ steps.refresh.outputs.day }}', tor)
        self.assertIn('cache-from: type=gha,scope=tor', tor)
        # Only the tor-image job, which scans the image, writes its cache.
        self.assertNotIn('cache-to:', tor)
        order = [self.steps.index(s) for s in (go, tor, run)]
        self.assertEqual(order, sorted(order))
        self.assertLess(self.steps.index(self.step('tags: opsecmkt:ci')), order[0])
        self.assertIn('container', re.search(r'^    needs: \[([^\]]+)\]$', job_block(CI, 'required'), re.M).group(1))

    def test_job_timeout_covers_the_refused_redeploy(self):
        # A refused start failed after 1 s on Compose v2.38.2 (CI 40685808020); the timeout still allows --wait-timeout 600.
        self.assertIn('"--wait-timeout", "600"', (ROOT / 'internal' / 'operator' / 'operator.go').read_text())
        minutes = int(re.search(r'^    timeout-minutes: (\d+)$', self.job, re.M).group(1))
        step = int(re.search(r'timeout-minutes: (\d+)', self.step('run: bash scripts/test-native-deploy.sh')).group(1))
        self.assertGreater(step * 60, 900, 'the script bounds the refused redeploy at 900 seconds')
        self.assertGreaterEqual(minutes, step + 15)


class NativeDeployScriptTest(unittest.TestCase):
    def test_renders_dev_and_production_through_compose_config(self):
        name = re.search(r"-run '\^(Test\w+)\$' \./internal/operator", SCRIPT).group(1)
        source = (ROOT / 'internal' / 'operator' / 'compose_render_test.go').read_text()
        self.assertIn('func %s(t *testing.T)' % name, source)
        self.assertIn('OPSECMKT_RENDER_DIR', source)
        self.assertIn('productionInitArgs', source)
        self.assertIn('config --quiet', SCRIPT)
        for render in ('dev-full:', 'prod:app', 'prod-tor:'):
            self.assertIn(render, SCRIPT)

    def test_drives_the_operator_commands(self):
        for text in ('deploy init --mode dev', 'deploy install', 'deploy redeploy --app-image "$second"',
                     '"$bin" deploy redeploy --dir "$dir" --app-image "$first"', 'deploy doctor --start-check',
                     'deploy logs', 'deploy down', "VALUES (999,'from_a_newer_release')",
                     '"$(container db)" == "$db_id"', '"$(container tor)" == "$tor_id"',
                     '"$(setting app_image)" == "$first"', '"$(volumes)" == "$installed_volumes"',
                     "'site_access','members'", 'com.docker.compose.oneoff=True', "'-run-'"):
            self.assertIn(text, SCRIPT)

    def test_pins_images_and_cleans_up_only_its_project(self):
        self.assertRegex(SCRIPT, r"registry_image='registry:[0-9.]+@sha256:[0-9a-f]{64}'")
        self.assertIn('-p 127.0.0.1::5000', SCRIPT)
        self.assertIn('trap cleanup EXIT', SCRIPT)
        self.assertIn('project="opsecmkt-native-$$"', SCRIPT)
        self.assertIn('down --volumes --remove-orphans', SCRIPT)
        cleanup = SCRIPT[SCRIPT.index('cleanup() {'):SCRIPT.index('trap cleanup EXIT')]
        for line in cleanup.splitlines():
            if re.search(r'docker (rm|volume rm|network rm|"\$kind" rm)', line):
                self.assertRegex(line, r'"\$\{ids\[@\]\}"|"\$registry"', line)
        self.assertIn('label=com.docker.compose.project=$name', cleanup)
        self.assertNotRegex(SCRIPT, r'\bprune\b')

    def test_documented_with_its_boundary(self):
        guide = (ROOT / 'docs' / 'operations-tests.md').read_text()
        section = guide[guide.index('## Native deployment on a real Docker Engine'):]
        section = section[:section.index('\n## ', 1)]
        self.assertIn('./scripts/test-native-deploy.sh', section)
        self.assertIn('not covered', section.lower())
        self.assertIn('LUKS', section)
        acceptance = (ROOT / 'docs' / 'acceptance.md').read_text()
        item = acceptance[acceptance.index('An actual app-only container redeployment preserves'):]
        item = item[:item.index('\n\n')]
        self.assertIn('scripts/test-native-deploy.sh', item)


if __name__ == '__main__':
    unittest.main()
