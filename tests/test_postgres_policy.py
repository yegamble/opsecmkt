"""Offline boundaries for backup space estimates and custom-archive tool versions."""
import importlib.util
from pathlib import Path
import unittest

POLICY = Path(__file__).resolve().parents[1] / 'scripts' / 'postgres-policy.py'
spec = importlib.util.spec_from_file_location('postgres_policy', POLICY)
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


class PostgresPolicyTests(unittest.TestCase):
    def test_space_estimate_uses_ceiling_and_headroom(self):
        # 25% reserve rounds up: 1 byte requires 2 plus the configured headroom.
        self.assertEqual(policy.required_backup_bytes(1, 7), 9)
        self.assertEqual(policy.required_backup_bytes(4096, 0), 5120)
        self.assertEqual(policy.required_backup_bytes(4096, 2048), 7168)

    def test_space_values_reject_unknown_or_overflow(self):
        for value in ('', '-1', '+1', '1.0', '9223372036854775808'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                policy.positive_integer(value, 'database size')
        with self.assertRaises(ValueError):
            policy.required_backup_bytes(policy.MAX_BYTES, 1)

    def test_client_version_order_covers_old_and_current_numbering(self):
        self.assertTrue(policy.restore_is_new_enough('9.6.25', 'pg_restore (PostgreSQL) 9.6.26'))
        self.assertFalse(policy.restore_is_new_enough('9.6.25', 'pg_restore (PostgreSQL) 9.6.24'))
        self.assertFalse(policy.restore_is_new_enough('10.0', 'pg_restore (PostgreSQL) 9.6.99'))
        self.assertTrue(policy.restore_is_new_enough('17.6', 'pg_restore (PostgreSQL) 17.6'))
        self.assertTrue(policy.restore_is_new_enough(
            '17.6 (Debian 17.6-1.pgdg13+1)',
            'pg_restore (PostgreSQL) 17.6 (Ubuntu 17.6-0ubuntu0.24.04.1)',
        ))
        self.assertTrue(policy.restore_is_new_enough(
            'pg_dump (PostgreSQL) 16.13 (Homebrew)',
            'pg_restore (PostgreSQL) 17.6 (Debian 17.6-1.pgdg13+1)',
        ))
        self.assertTrue(policy.restore_is_new_enough('17.5', 'pg_restore (PostgreSQL) 17.6'))
        self.assertFalse(policy.restore_is_new_enough('17.7', 'pg_restore (PostgreSQL) 17.6'))

    def test_unknown_prerelease_or_header_formats_fail_closed(self):
        for value in ('17.6beta1', '17.6rc1', '17.6devel', 'PostgreSQL 17.6', '17',
                      'pg_restore (PostgreSQL) 17.6beta1 (Ubuntu)'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                policy.parse_version(value)


if __name__ == '__main__':
    unittest.main()
