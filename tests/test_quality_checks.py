"""Behavioral contracts for the shared local/CI checks, with tool processes stubbed."""
from pathlib import Path
import os
import shutil
import subprocess
import tempfile
import unittest


REPOSITORY = Path(__file__).resolve().parents[1]


class QualityChecksTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / 'scripts').mkdir()
        shutil.copyfile(REPOSITORY / 'scripts' / 'quality.sh', self.root / 'scripts' / 'quality.sh')
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.log = self.root / 'commands.log'
        go = self.bin / 'go'
        go.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$QUALITY_TEST_LOG"
case "$*" in
  *'actionlint -version'*) exit "${QUALITY_TEST_ACTIONLINT_STATUS:-0}" ;;
  *'goimports -h'*) printf '%s' "$QUALITY_TEST_IMPORTS_SMOKE_PREAMBLE"; printf 'usage: goimports [flags] [path ...]\\n'; exit "${QUALITY_TEST_IMPORTS_SMOKE_STATUS:-2}" ;;
  *'goimports -l'*) printf '%s' "$QUALITY_TEST_FORMATTED_FILES"; exit "${QUALITY_TEST_FORMAT_STATUS:-0}" ;;
  *'golangci-lint run'*) exit "${QUALITY_TEST_LINT_STATUS:-0}" ;;
  *'govulncheck -test'*) exit "${QUALITY_TEST_VULN_STATUS:-0}" ;;
esac
exit 0
''')
        go.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ['PATH'],
                        QUALITY_TEST_LOG=str(self.log), QUALITY_TEST_FORMATTED_FILES='')
        subprocess.run(['git', 'init', '-q', str(self.root)], check=True)
        (self.root / 'example.go').write_text('package example\n')
        subprocess.run(['git', '-C', str(self.root), 'add', 'example.go'], check=True)

    def run_check(self, name, *args, **variables):
        return subprocess.run(['bash', 'scripts/quality.sh', name, *args], cwd=self.root,
                              env=dict(self.env, **variables), capture_output=True, text=True)

    def test_clean_formatting_succeeds(self):
        result = self.run_check('fmt-check')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('tool -modfile=tools/go.mod goimports -l example.go', self.log.read_text())

    def test_unformatted_output_fails_without_changing_file_or_index(self):
        path = self.root / 'example.go'
        path.write_text('package example\n// unstaged edit\n')
        before = path.read_bytes()
        staged = subprocess.check_output(['git', 'show', ':example.go'], cwd=self.root)
        result = self.run_check('fmt-check', QUALITY_TEST_FORMATTED_FILES='example.go\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('example.go', result.stdout)
        self.assertEqual(path.read_bytes(), before)
        self.assertEqual(subprocess.check_output(['git', 'show', ':example.go'], cwd=self.root), staged)

    def test_formatter_process_failure_is_not_treated_as_clean(self):
        self.assertEqual(self.run_check('fmt-check', QUALITY_TEST_FORMAT_STATUS='17').returncode, 17)

    def test_explicit_format_paths_are_forwarded_without_shell_expansion(self):
        filename = 'file with spaces.go'
        (self.root / filename).write_text('package example\n')
        result = self.run_check('fmt-check', filename)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(filename, self.log.read_text())

    def test_linter_failure_propagates(self):
        result = self.run_check('lint', QUALITY_TEST_LINT_STATUS='19')
        self.assertEqual(result.returncode, 19)
        self.assertIn('golangci-lint run ./...', self.log.read_text())

    def test_quality_includes_explicit_vet_and_stops_on_format_failure(self):
        result = self.run_check('go-quality')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('vet ./...', self.log.read_text())
        self.log.write_text('')
        result = self.run_check('go-quality', QUALITY_TEST_FORMATTED_FILES='example.go\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('golangci-lint', self.log.read_text())

    def test_vulnerabilities_include_test_dependencies_and_fail(self):
        result = self.run_check('vuln', QUALITY_TEST_VULN_STATUS='23')
        self.assertEqual(result.returncode, 23)
        self.assertIn('govulncheck -test ./...', self.log.read_text())

    def test_secrets_scan_is_staged_and_redacted(self):
        result = self.run_check('secrets-staged')
        self.assertEqual(result.returncode, 0, result.stderr)
        command = self.log.read_text()
        self.assertIn('gitleaks git', command)
        self.assertIn('--staged', command)
        self.assertIn('--redact', command)

    def test_missing_go_fails_with_installation_guidance(self):
        result = self.run_check('lint', GO=str(self.root / 'missing-go'))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Go', result.stderr)

    def test_workflow_linter_uses_its_compatible_isolated_module(self):
        result = self.run_check('workflow-lint')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('tool -modfile=tools/actionlint/go.mod actionlint', self.log.read_text())

    def test_module_checks_include_the_isolated_workflow_tool(self):
        result = self.run_check('mod-tidy')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('-C tools/actionlint mod verify', self.log.read_text())
        self.assertIn('-C tools/actionlint mod tidy -diff', self.log.read_text())

    def test_smoke_executes_every_tool_and_propagates_bootstrap_failure(self):
        result = self.run_check('tools-smoke')
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = self.log.read_text()
        for command in ('goimports -h', 'golangci-lint version', 'lefthook version',
                        'gitleaks version', 'deadcode -help', 'govulncheck -version',
                        'actionlint -version'):
            self.assertIn(command, commands)
        result = self.run_check('tools-smoke', QUALITY_TEST_ACTIONLINT_STATUS='19')
        self.assertEqual(result.returncode, 19)

    def test_smoke_accepts_first_build_download_messages_and_rejects_compile_errors(self):
        result = self.run_check('tools-smoke', QUALITY_TEST_IMPORTS_SMOKE_PREAMBLE='go: downloading a pinned dependency\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.run_check('tools-smoke', QUALITY_TEST_IMPORTS_SMOKE_STATUS='1')
        self.assertEqual(result.returncode, 1)

    def test_unknown_command_fails(self):
        self.assertNotEqual(self.run_check('unknown-check').returncode, 0)
