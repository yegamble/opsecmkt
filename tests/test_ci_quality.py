"""Quality/security checks stay mandatory when CI is called by PRs or releases."""
from pathlib import Path
import os
import re
import subprocess
import tempfile
import textwrap
import unittest

from test_ci_go_split import job_block

ROOT = Path(__file__).resolve().parents[1]


class QualityWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.ci = (ROOT / '.github/workflows/ci.yaml').read_text()

    def test_independent_quality_jobs_are_required_without_event_conditions(self):
        required = job_block(self.ci, 'required')
        needs = re.search(r'^    needs: \[([^\]]+)\]$', required, re.M).group(1).split(',')
        for name in ('quality', 'workflow-safety', 'security'):
            self.assertIn(name, [value.strip() for value in needs])
            block = job_block(self.ci, name)
            self.assertNotRegex(block, r'(?m)^    (if|needs|continue-on-error):')
        self.assertIn('workflow_call:', self.ci)
        release = (ROOT / '.github/workflows/release.yaml').read_text()
        self.assertIn('uses: ./.github/workflows/ci.yaml', job_block(release, 'verify'))
        self.assertIn('needs: verify', job_block(release, 'package'))

    def test_scheduled_security_reuses_dependency_gates_without_the_integration_matrix(self):
        scheduled = (ROOT / '.github/workflows/security.yaml').read_text()
        self.assertRegex(scheduled, r"cron: '[^']+'")
        self.assertIn('workflow_dispatch:', scheduled)
        self.assertIn('uses: ./.github/workflows/dependency-security.yaml', scheduled)
        self.assertNotIn('workflows/ci.yaml', scheduled)
        dependencies = (ROOT / '.github/workflows/dependency-security.yaml').read_text()
        self.assertIn('workflow_call:', dependencies)
        self.assertIn('make vuln', job_block(dependencies, 'go-vulnerabilities'))
        self.assertIn('npm audit --audit-level=high', job_block(dependencies, 'npm-vulnerabilities'))
        self.assertIn('deploy/docs/cloudflare', job_block(dependencies, 'npm-vulnerabilities'))
        self.assertIn('--require-hashes --no-deps', job_block(dependencies, 'python-vulnerabilities'))
        self.assertNotIn('continue-on-error:', dependencies + scheduled)

    def test_real_built_images_and_release_archive_are_scanned(self):
        for job, image in (('container', 'opsecmkt:ci'), ('tor-image', 'opsecmkt-tor:ci')):
            block = job_block(self.ci, job)
            self.assertIn('uses: ./.github/actions/scan-image', block)
            self.assertIn('image-ref: ' + image, block)
        release = (ROOT / '.github/workflows/release.yaml').read_text()
        self.assertIn('image-archive: opsecmkt.oci.tar', release)
        scanner = (ROOT / '.github/actions/scan-image/action.yml').read_text()
        self.assertIn("version: 'v0.76.0'", scanner)
        self.assertIn('--exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL', scanner)
        self.assertIn('if: always()', scanner)
        self.assertNotIn('continue-on-error:', scanner)

    def test_auxiliary_toolchains_receive_dependency_updates(self):
        dependabot = (ROOT / '.github/dependabot.yml').read_text()
        self.assertIn('directory: /tools', dependabot)
        self.assertIn('directory: /deploy/docs/cloudflare', dependabot)
        self.assertIn('package-ecosystem: pip', dependabot)


class ImageGateRunTest(unittest.TestCase):
    def setUp(self):
        scanner = (ROOT / '.github/actions/scan-image/action.yml').read_text()
        self.script = textwrap.dedent(re.search(
            r'      run: \|\n(.*?)(?=^    - name:)', scanner, re.M | re.S).group(1))

    def run_gate(self, image='', archive='', scanner_exit=0):
        with tempfile.TemporaryDirectory() as scratch:
            root = Path(scratch)
            scanner = root / 'trivy'
            scanner.write_text('#!/bin/sh\nexit "$SCANNER_EXIT"\n')
            scanner.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       IMAGE_REF=image, IMAGE_ARCHIVE=archive, SCANNER_EXIT=str(scanner_exit))
            return subprocess.run(['bash', '-e', '-c', self.script], env=env,
                                  capture_output=True, text=True)

    def test_scanner_failure_is_not_hidden_for_images_or_archives(self):
        self.assertEqual(self.run_gate(image='example:ci', scanner_exit=7).returncode, 7)
        self.assertEqual(self.run_gate(archive='release image.oci.tar', scanner_exit=9).returncode, 9)

    def test_exactly_one_built_artifact_is_required(self):
        self.assertEqual(self.run_gate(image='example:ci').returncode, 0)
        self.assertEqual(self.run_gate(archive='release.oci.tar').returncode, 0)
        self.assertNotEqual(self.run_gate().returncode, 0)
        self.assertNotEqual(self.run_gate(image='example:ci', archive='release.oci.tar').returncode, 0)


if __name__ == '__main__':
    unittest.main()
