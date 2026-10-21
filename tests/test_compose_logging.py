"""Static checks on the Compose log rotation block (A-144, A-180); no Docker needed."""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
COMPOSE_FILES = ('compose.yaml', 'compose.tor.yaml', 'compose.nodes.yaml', 'compose.scanner.yaml', 'compose.nodes-tor.yaml')
# The operator picks the driver (LOG_DRIVER, default json-file); the rotation options suit json-file and local.
LOGGING = """x-logging: &logging
  driver: ${LOG_DRIVER:-json-file}
  options:
    max-size: 10m
    max-file: '3'
"""


class ComposeLoggingTests(unittest.TestCase):
    def test_driver_follows_log_driver_with_rotation(self):
        for name in COMPOSE_FILES:
            with self.subTest(name):
                self.assertIn(LOGGING, (REPOSITORY / name).read_text())

    def test_every_service_uses_the_block(self):
        for name in COMPOSE_FILES:
            text = (REPOSITORY / name).read_text()
            services = re.split(r'^\S', text.split('\nservices:\n', 1)[1], maxsplit=1, flags=re.M)[0]
            blocks = re.split(r'^  ([a-z][a-z0-9-]*):\n', services, flags=re.M)[1:]
            self.assertTrue(blocks, name)
            for service, body in zip(blocks[::2], blocks[1::2]):
                if not re.search(r'(?m)^    (image|build):', body):
                    continue  # an override of a service another file defines (compose.scanner.yaml's app)
                with self.subTest(file=name, service=service):
                    self.assertIn('    logging: *logging\n', body)

    def test_operator_guide_and_upgrading_name_the_setting(self):
        guide = (REPOSITORY / 'docs' / 'operator-guide.md').read_text()
        upgrading = (REPOSITORY / 'UPGRADING.md').read_text()
        for text in (guide, upgrading):
            self.assertIn('LOG_DRIVER', text)
        self.assertIn('!override', guide)


class BackupStdoutLoggingTests(unittest.TestCase):
    """A container whose stdout carries a backup (wallet files, onion keys) into age runs with --log-driver none:
    Docker's log driver records a container's stdout, so the secret would otherwise be kept in the daemon's logs."""

    def test_docker_run_piped_into_age_disables_logging(self):
        checked = 0
        for path in [REPOSITORY / 'UPGRADING.md', *sorted((REPOSITORY / 'docs').rglob('*.md')), *sorted((REPOSITORY / 'scripts').glob('*.sh'))]:
            text = path.read_text()
            # Join backslash-continued lines and a pipe ending a line, then split commands at ";", "&&" and newlines.
            joined = re.sub(r'(\\|\|)\n\s*', lambda m: ' ' if m.group(1) == '\\' else '| ', text)
            for line in joined.splitlines():
                for command in re.split(r'&&|;', line):
                    match = re.search(r'docker run\b(.*?)\|\s*age\b', command)
                    if not match:
                        continue
                    checked += 1
                    with self.subTest(file=str(path.relative_to(REPOSITORY)), command=command.strip()[:120]):
                        self.assertIn('--log-driver none', match.group(1))
        self.assertGreaterEqual(checked, 2)


if __name__ == '__main__':
    unittest.main()
