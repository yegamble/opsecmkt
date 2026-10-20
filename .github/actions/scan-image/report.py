"""Expose actionable Trivy findings in check annotations before its blocking gate."""
import html
import json
import os
from pathlib import Path
import sys


def fixable_findings(report):
    results = report.get('Results') or []
    if not isinstance(results, list):
        raise ValueError('Trivy Results must be a list')
    findings = set()
    for result in results:
        for vulnerability in result.get('Vulnerabilities') or []:
            severity = vulnerability.get('Severity', '')
            fixed = vulnerability.get('FixedVersion') or ''
            if severity not in ('HIGH', 'CRITICAL') or not fixed:
                continue
            findings.add(tuple(str(value) for value in (
                severity, vulnerability.get('VulnerabilityID', ''),
                vulnerability.get('PkgName', ''), vulnerability.get('InstalledVersion', ''),
                fixed, result.get('Target', ''))))
    return sorted(findings)


def escape_command(message):
    # GitHub workflow command data must escape '%' first, then CR and LF.
    return message.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A')


def main():
    findings = fixable_findings(json.loads(Path(sys.argv[1]).read_text()))
    messages = [f'{severity} {cve}: {package} {installed} -> {fixed} ({target})'
                for severity, cve, package, installed, fixed, target in findings]
    for message in messages:
        print('::error title=Fixable image vulnerability::' + escape_command(message))
    if not messages:
        print('No fixable HIGH/CRITICAL vulnerabilities found in this image report.')

    summary_path = os.environ.get('GITHUB_STEP_SUMMARY')
    if summary_path:
        with Path(summary_path).open('a') as summary:
            summary.write(f'### Image vulnerability gate\n\n{len(findings)} fixable HIGH/CRITICAL findings.\n\n')
            for message in messages[:50]:
                visible = message.replace('\r', r'\r').replace('\n', r'\n')
                summary.write(f'- <code>{html.escape(visible)}</code>\n')
            if len(messages) > 50:
                summary.write('\nThe summary shows the first 50 findings; the JSON artifact contains all findings.\n')
    # Annotations provide detail; the following Trivy command remains the authority
    # for the gate's exit status, including scanner/network errors.


if __name__ == '__main__':
    main()
