"""Built-image CVE diagnostics remain visible without weakening Trivy's gate."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


RENDERER = Path(__file__).resolve().parents[1] / '.github/actions/scan-image/report.py'


class ImageScanReportTest(unittest.TestCase):
    def render(self, report):
        with tempfile.TemporaryDirectory() as scratch:
            report_path = Path(scratch) / 'report.json'
            summary_path = Path(scratch) / 'summary.md'
            report_path.write_text(json.dumps(report))
            result = subprocess.run(
                [sys.executable, str(RENDERER), str(report_path)],
                env=dict(os.environ, GITHUB_STEP_SUMMARY=str(summary_path)),
                capture_output=True, text=True)
            summary = summary_path.read_text() if summary_path.exists() else ''
            return result, summary

    def test_only_fixable_high_and_critical_findings_are_annotated(self):
        high = dict(VulnerabilityID='CVE-2026-1234', PkgName='libexample',
                    InstalledVersion='1.0', FixedVersion='1.1', Severity='HIGH')
        critical = dict(high, VulnerabilityID='CVE-2026-5678', Severity='CRITICAL')
        report = {'Results': [
            {'Target': 'image (debian 12)', 'Vulnerabilities': [
                high, high, dict(high, Severity='MEDIUM'),
                dict(high, FixedVersion=''), dict(high, FixedVersion=None)]},
            {'Target': 'app/server', 'Vulnerabilities': [critical]},
            {'Target': 'no findings', 'Vulnerabilities': None},
        ]}
        result, summary = self.render(report)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(result.stdout.splitlines()), 2)
        self.assertIn('HIGH CVE-2026-1234: libexample 1.0 -> 1.1', result.stdout)
        self.assertIn('CRITICAL CVE-2026-5678: libexample 1.0 -> 1.1', result.stdout)
        self.assertIn('2 fixable HIGH/CRITICAL', summary)

    def test_dependency_text_cannot_inject_workflow_commands_or_summary_markup(self):
        report = {'Results': [{'Target': 'image', 'Vulnerabilities': [{
            'VulnerabilityID': 'CVE-2026-1234', 'Severity': 'HIGH',
            'PkgName': 'pkg%\r\n::warning title=Injected::oops<script>',
            'InstalledVersion': '1', 'FixedVersion': '2',
        }]}]}
        result, summary = self.render(report)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(result.stdout.splitlines()), 1)
        self.assertIn('pkg%25%0D%0A::warning', result.stdout)
        self.assertNotIn('<script>', summary)
        self.assertIn('&lt;script&gt;', summary)

    def test_clean_report_does_not_emit_an_error(self):
        result, summary = self.render({'Results': []})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('::error', result.stdout)
        self.assertIn('0 fixable HIGH/CRITICAL', summary)

    def test_malformed_report_fails_instead_of_claiming_no_findings(self):
        result, _ = self.render({'Results': 'invalid'})
        self.assertNotEqual(result.returncode, 0)


if __name__ == '__main__':
    unittest.main()
