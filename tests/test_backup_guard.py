"""Exercise backup guard ordering without a database, dump, or encrypted archive."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
BACKUP = ROOT / 'scripts' / 'backup.sh'


def df_stub(available_kib=800, mount_with_spaces=False):
    mount = '/tmp/mock mount with spaces' if mount_with_spaces else '/tmp/mock'
    return f'''cat <<'EOF'
Filesystem 1024-blocks Used Available Capacity Mounted on
/dev/mock 10000 100 {available_kib} 1% {mount}
EOF'''


class BackupGuardTests(unittest.TestCase):
    def run_backup(self, headroom, database_size='1024', invalid_df=False, mount_with_spaces=False,
                   available_kib=800):
        work = tempfile.TemporaryDirectory()
        self.addCleanup(work.cleanup)
        bin_dir = Path(work.name) / 'bin'
        bin_dir.mkdir()
        stubs = {
            'psql': f'printf "{database_size}\\n"',
            'pg_dump': 'if [ "$1" = --version ]; then echo "pg_dump (PostgreSQL) 17.6"; else echo "archive pg_dump invoked" >&2; echo synthetic-archive; fi',
            'age': 'cat',
            'df': 'echo "unknown filesystem capacity"' if invalid_df else df_stub(available_kib, mount_with_spaces),
        }
        for name, body in stubs.items():
            tool = bin_dir / name
            tool.write_text(f'#!/bin/sh\n{body}\n')
            tool.chmod(0o700)
        secret = 'backup-credential-never-print'
        output = Path(work.name) / 'backups' / 'market.dump.age'
        env = dict(os.environ, PATH=f'{bin_dir}:{os.environ["PATH"]}',
                   AGE_RECIPIENT='age1test',
                   BACKUP_DATABASE_URL=f'postgres://user:{secret}@127.0.0.1:1/db',
                   BACKUP_SPACE_HEADROOM_BYTES=headroom)
        result = subprocess.run(['bash', str(BACKUP), str(output)], cwd=ROOT,
                                env=env, capture_output=True, text=True)
        return result, output, Path(work.name), secret

    def test_low_space_fails_before_archive_temp_or_pg_dump(self):
        result, output, work, secret = self.run_backup('1024', available_kib=1)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('Insufficient free space', result.stderr)
        self.assertIn('Backup refused before writing an archive', result.stderr)
        self.assertNotIn('archive pg_dump invoked', result.stderr)
        self.assertFalse(output.exists())
        self.assertEqual(list(work.glob('backups/market.dump.age.tmp.*')), [])
        self.assertNotIn(secret, result.stdout + result.stderr)
        self.assertNotIn('postgres://', result.stdout + result.stderr)

    def test_unknown_headroom_fails_closed_before_archive_temp_or_pg_dump(self):
        result, output, work, secret = self.run_backup('unknown')
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('Cannot safely verify PostgreSQL backup space', result.stderr)
        self.assertNotIn('archive pg_dump invoked', result.stderr)
        self.assertFalse(output.exists())
        self.assertEqual(list(work.glob('backups/market.dump.age.tmp.*')), [])
        self.assertNotIn(secret, result.stdout + result.stderr)

    def test_unknown_database_size_fails_closed_before_archive_temp_or_pg_dump(self):
        result, output, work, secret = self.run_backup('0', 'size-unknown')
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('Cannot safely verify PostgreSQL backup space', result.stderr)
        self.assertNotIn('archive pg_dump invoked', result.stderr)
        self.assertFalse(output.exists())
        self.assertEqual(list(work.glob('backups/market.dump.age.tmp.*')), [])
        self.assertNotIn(secret, result.stdout + result.stderr)

    def test_unknown_destination_capacity_fails_closed_before_archive_temp_or_pg_dump(self):
        result, output, work, secret = self.run_backup('0', invalid_df=True)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('Could not determine free space on the backup destination filesystem', result.stderr)
        self.assertNotIn('archive pg_dump invoked', result.stderr)
        self.assertFalse(output.exists())
        self.assertEqual(list(work.glob('backups/market.dump.age.tmp.*')), [])
        self.assertNotIn(secret, result.stdout + result.stderr)

    def test_posix_df_available_field_works_when_mount_path_has_spaces(self):
        result, output, _, secret = self.run_backup('0', mount_with_spaces=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(output.exists())
        self.assertIn('client: pg_dump (PostgreSQL) 17.6', result.stdout)
        self.assertNotIn(secret, result.stdout + result.stderr)

    def test_success_prints_actual_selected_dump_client(self):
        result, output, _, secret = self.run_backup('0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(output.exists())
        self.assertIn('client: pg_dump (PostgreSQL) 17.6', result.stdout)
        self.assertNotIn(secret, result.stdout + result.stderr)
        self.assertNotIn('postgres://', result.stdout + result.stderr)

    def test_internal_compose_path_checks_size_before_dump_and_reports_version(self):
        with tempfile.TemporaryDirectory() as work:
            bin_dir = Path(work) / 'bin'
            bin_dir.mkdir()
            log = Path(work) / 'docker.log'
            docker = bin_dir / 'docker'
            docker.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$DOCKER_LOG"
case " $* " in
  *" pg_dump --version "*) echo "pg_dump (PostgreSQL) 17.6" ;;
  *"pg_database_size(current_database())"*) echo 1024 ;;
  *"--format=custom"*) echo synthetic-archive ;;
  *) echo "unexpected docker invocation" >&2; exit 8 ;;
esac
''')
            docker.chmod(0o700)
            age = bin_dir / 'age'
            age.write_text('#!/bin/sh\ncat\n')
            age.chmod(0o700)
            df = bin_dir / 'df'
            df.write_text('#!/bin/sh\n' + df_stub() + '\n')
            df.chmod(0o700)
            output = Path(work) / 'backups' / 'internal.dump.age'
            env = dict(os.environ, PATH=f'{bin_dir}:{os.environ["PATH"]}', AGE_RECIPIENT='age1test',
                       BACKUP_INTERNAL_DATABASE='market', BACKUP_SPACE_HEADROOM_BYTES='0',
                       DOCKER_LOG=str(log))
            result = subprocess.run(['bash', str(BACKUP), str(output)], cwd=ROOT,
                                    env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('internal database market', result.stdout)
            self.assertIn('client: pg_dump (PostgreSQL) 17.6', result.stdout)
            invocations = log.read_text().splitlines()
            size_check = next(i for i, line in enumerate(invocations) if 'pg_database_size(current_database())' in line)
            archive_dump = next(i for i, line in enumerate(invocations) if '--format=custom' in line)
            self.assertLess(size_check, archive_dump)

    def test_internal_backup_refuses_exported_database_url_override(self):
        with tempfile.TemporaryDirectory() as work:
            root = Path(work)
            scripts = root / 'scripts'
            scripts.mkdir()
            for name in ('backup.sh', 'postgres-policy.py'):
                (scripts / name).write_bytes((ROOT / 'scripts' / name).read_bytes())
            (root / '.env').write_text(
                'DATABASE_URL=postgres://app:env-secret@db:5432/from_env?sslmode=disable\n')
            bin_dir = root / 'bin'
            bin_dir.mkdir()
            log = root / 'docker.log'
            docker = bin_dir / 'docker'
            docker.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$CALL_LOG"
case " $* " in
  *" pg_dump --version "*) echo "pg_dump (PostgreSQL) 17.6" ;;
  *"pg_database_size(current_database())"*) echo 1024 ;;
  *"--format=custom"*) echo synthetic-archive ;;
  *) echo "unexpected docker invocation" >&2; exit 8 ;;
esac
''')
            docker.chmod(0o700)
            age = bin_dir / 'age'
            age.write_text('#!/bin/sh\ncat\n')
            age.chmod(0o700)
            output = root / 'backup.age'
            env = dict(os.environ, PATH=f'{bin_dir}:{os.environ["PATH"]}', AGE_RECIPIENT='age1test',
                       DATABASE_URL='postgres://app:export-secret@db:5432/exported?sslmode=disable',
                       BACKUP_SPACE_HEADROOM_BYTES='0', CALL_LOG=str(log))
            env.pop('BACKUP_DATABASE_URL', None)
            env.pop('BACKUP_INTERNAL_DATABASE', None)
            result = subprocess.run(['bash', str(scripts / 'backup.sh'), str(output)], cwd=root,
                                    env=env, capture_output=True, text=True)
            calls = log.read_text() if log.exists() else ''
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertIn('differs from DATABASE_URL in .env', result.stderr)
            self.assertNotIn('pg_database_size(current_database())', calls)
            self.assertNotIn('--format=custom', calls)
            self.assertFalse(output.exists())
            self.assertNotIn('env-secret', result.stdout + result.stderr + calls)
            self.assertNotIn('export-secret', result.stdout + result.stderr + calls)
            self.assertNotIn('postgres://', result.stdout + result.stderr + calls)


if __name__ == '__main__':
    unittest.main()
