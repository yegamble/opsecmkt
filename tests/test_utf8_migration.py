"""UPGRADING.md section 42 (A-481; QA5W3B-1 and -2), offline: the moving of a non-UTF8 database as documented, the leg
that rehearses it (scripts/test-utf8-migration.sh, which needs Docker and runs in the CI operations job) and the pieces
it uses. No database here.

What is pinned: the section no longer claims that pg_restore stops at text that is not valid UTF-8 (it does not, for a
dump taken unconverted) nor that LATIN1 text only may come out double-encoded; it has the operator run
scripts/check-utf8.sql on the old database, back it up with BACKUP_DUMP_ENCODING=SQL_ASCII, and prove before the switch
that the new database's own backup restores. backup.sh refuses any other BACKUP_DUMP_ENCODING before writing anything;
check-utf8.sql is read-only and stops psql on a bad value; the leg exists, is run by CI and asserts what the section
promises.
"""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
LEG = REPOSITORY / 'scripts' / 'test-utf8-migration.sh'
CHECK = REPOSITORY / 'scripts' / 'check-utf8.sql'
BACKUP = REPOSITORY / 'scripts' / 'backup.sh'


def section_42():
    text = (REPOSITORY / 'UPGRADING.md').read_text()
    start = text.index('## 42. Database encoding must be UTF8 (A-481)')
    return text[start:text.index('\n## 43. ', start)]


def flat(text):
    return re.sub(r'\s+', ' ', text)


class Section42Tests(unittest.TestCase):
    def test_false_claims_are_gone(self):
        section = flat(section_42())
        self.assertNotIn('stops, changing nothing, at text that is not valid in UTF8', section)
        self.assertNotIn('may hold non-ASCII text stored double-encoded', section)

    def test_steps_in_order(self):
        section = flat(section_42())
        steps = [
            'docker compose stop app',
            'psql "$OLD_DATABASE_URL" -X -f scripts/check-utf8.sql',
            "convert_from(convert(convert_to(body, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE ctid = '(0,1)'",
            'BACKUP_DUMP_ENCODING=SQL_ASCII BACKUP_DATABASE_URL="$OLD_DATABASE_URL" AGE_RECIPIENT=age1... ./scripts/backup.sh',
            "CREATE DATABASE opsecmkt_utf8 ENCODING 'UTF8' TEMPLATE template0;",
            'RESTORE_DATABASE_URL="$NEW_DATABASE_URL" AGE_IDENTITY=... ./scripts/restore.sh backups/pre-utf8.dump.age',
            'BACKUP_DATABASE_URL="$NEW_DATABASE_URL" AGE_RECIPIENT=age1... ./scripts/backup.sh backups/utf8-check.dump.age',
            'RESTORE_DATABASE_URL="$CHECK_DATABASE_URL" AGE_IDENTITY=... ./scripts/restore.sh backups/utf8-check.dump.age',
            'DROP DATABASE opsecmkt_utf8_check',
            'Point `DATABASE_URL` (and `BACKUP_DATABASE_URL`) at the new database and start the app.',
        ]
        at = 0
        for step in steps:
            found = section.find(step, at)
            self.assertGreaterEqual(found, 0, f'missing or out of order: {step}')
            at = found + len(step)

    def test_promises(self):
        section = flat(section_42())
        for claim in ('**every non-ASCII character will be double-encoded**',
                      '`pg_restore` does not check the bytes of a dump taken unconverted',
                      'COPY failed for table "<table>": ... invalid byte sequence for encoding "UTF8"',
                      'do not switch',
                      'on a UTF8 database it changes nothing',
                      'scripts/test-utf8-migration.sh'):
            self.assertIn(claim, section)


class BackupDumpEncodingTests(unittest.TestCase):
    def run_backup(self, encoding):
        with tempfile.TemporaryDirectory() as work:
            # A stand-in age, so the refusal is reached without age installed; pg_dump must never run.
            bin_dir = Path(work) / 'bin'
            bin_dir.mkdir()
            for tool, body in (('age', 'cat >/dev/null'), ('pg_dump', 'echo pg_dump ran >&2; exit 9')):
                (bin_dir / tool).write_text(f'#!/bin/sh\n{body}\n')
                (bin_dir / tool).chmod(0o700)
            output = Path(work) / 'out.dump.age'
            env = dict(os.environ, PATH=f'{bin_dir}:{os.environ["PATH"]}', AGE_RECIPIENT='age1test',
                       BACKUP_DATABASE_URL='postgres://user@127.0.0.1:1/db', BACKUP_DUMP_ENCODING=encoding)
            result = subprocess.run(['bash', str(BACKUP), str(output)], env=env, capture_output=True, text=True)
            return result, output.exists(), list(Path(work).glob('out.dump.age.tmp.*'))

    def test_other_values_refused_before_anything_is_written(self):
        for encoding in ('LATIN1', 'UTF8', 'sql_ascii', 'SQL_ASCII ', '-E SQL_ASCII'):
            with self.subTest(encoding):
                result, written, temporary = self.run_backup(encoding)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('BACKUP_DUMP_ENCODING may only be SQL_ASCII', result.stderr)
                self.assertNotIn('pg_dump ran', result.stderr)
                self.assertFalse(written)
                self.assertEqual(temporary, [])

    def test_sql_ascii_reaches_pg_dump(self):
        text = BACKUP.read_text()
        self.assertIn('SQL_ASCII) dump_encoding=(--encoding=SQL_ASCII) ;;', text)
        # Both the external and the internal-db dump take it; the empty-array form keeps bash 3.2 under set -u.
        self.assertEqual(text.count('--no-acl ${dump_encoding[@]+"${dump_encoding[@]}"} | age'), 2)


class CheckScriptTests(unittest.TestCase):
    def test_read_only_and_stops_psql(self):
        text = CHECK.read_text()
        code = '\n'.join(line for line in text.splitlines() if not line.lstrip().startswith('--'))
        self.assertIn('\\set ON_ERROR_STOP on', code)
        self.assertIsNone(re.search(r'\b(INSERT|UPDATE|DELETE|ALTER|DROP|CREATE|TRUNCATE|GRANT)\b', code, re.I))
        self.assertIn("convert(convert_to(r.val, 'SQL_ASCII'), 'UTF8', 'UTF8')", code)
        self.assertIn('RAISE EXCEPTION', code)
        for typ in ("'text'::regtype", "'varchar'::regtype", "'bpchar'::regtype"):
            self.assertIn(typ, code)


class LegTests(unittest.TestCase):
    def test_leg_present_executable_and_run_by_ci(self):
        self.assertTrue(LEG.is_file())
        self.assertTrue(os.access(LEG, os.X_OK))
        subprocess.run(['bash', '-n', str(LEG)], check=True)
        workflow = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
        operations = workflow[workflow.index('  operations:'):workflow.index('\n  compose:')]
        self.assertIn('run: bash scripts/test-utf8-migration.sh', operations)

    def test_leg_asserts_what_section_42_promises(self):
        text = LEG.read_text()
        for needle in ('postgres:17-alpine',
                       "old_database ascii_old SQL_ASCII \"$bad_hex\"",
                       'bad_hex=477265617420fffe2073656c6c6572',
                       "if check ascii_old; then fail",
                       'COPY failed for table "reviews": ERROR:  invalid byte sequence for encoding "UTF8": 0xff',
                       "if round_trip ascii_new; then fail",
                       "WHERE ctid = '(0,1)'",
                       "title='Crème brûlée — 東京 🔒'",
                       '== "$double_hex" ]]',
                       'if BACKUP_DUMP_ENCODING=LATIN1 backup',
                       'BACKUP_DUMP_ENCODING=SQL_ASCII backup latin1_old',
                       'BACKUP_DUMP_ENCODING=SQL_ASCII round_trip latin1_new',
                       'docker rm -f -v "$container"'):
            self.assertIn(needle, text)


if __name__ == '__main__':
    unittest.main()
