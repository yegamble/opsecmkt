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
RESTORE = REPOSITORY / 'scripts' / 'restore.sh'


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
            # A-483: every client command takes its URL from an exported variable through postgres-tool.py.
            "python3 scripts/postgres-tool.py DATABASE_URL psql -c 'SHOW server_encoding'",
            'docker compose stop app',
            'python3 scripts/postgres-tool.py OLD_DATABASE_URL psql -X -f scripts/check-utf8.sql',
            "convert_from(convert(convert_to(body, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = '6f0c2a9e41d7'",
            # A-483: the tool strips PGCLIENTENCODING, so the typed text sets the client encoding in psql.
            '`python3 scripts/postgres-tool.py OLD_DATABASE_URL psql` after the psql command `\\encoding UTF8`',
            "SET title = convert_from(convert_to('Crème brûlée', 'UTF8'), 'SQL_ASCII') WHERE id = '0d3e7b5a92c4'",
            'BACKUP_DUMP_ENCODING=SQL_ASCII BACKUP_DATABASE_URL="$OLD_DATABASE_URL" AGE_RECIPIENT=age1... ./scripts/backup.sh',
            # QA5W6-3: through the maintenance database; opsecmkt_utf8 is created only in step 4.
            "python3 scripts/postgres-tool.py MAINTENANCE_DATABASE_URL psql -c \"CREATE DATABASE opsecmkt_latin1 ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0\"",
            # QA5W5B-8: the URL's password reaches pg_restore in PGPASSWORD, never in its argv.
            "python3 scripts/postgres-tool.py LATIN1_DATABASE_URL pg_restore --dbname='' --no-owner --no-acl --exit-on-error",
            # A-483: step 4 connects to the maintenance database, the only one that exists at that moment.
            "python3 scripts/postgres-tool.py MAINTENANCE_DATABASE_URL psql -c \"CREATE DATABASE opsecmkt_utf8 ENCODING 'UTF8' TEMPLATE template0\"",
            'RESTORE_DATABASE_URL="$NEW_DATABASE_URL" AGE_IDENTITY=... ./scripts/restore.sh backups/pre-utf8.dump.age',
            'BACKUP_DATABASE_URL="$NEW_DATABASE_URL" AGE_RECIPIENT=age1... ./scripts/backup.sh backups/utf8-check.dump.age',
            'python3 scripts/postgres-tool.py NEW_DATABASE_URL psql -c "CREATE DATABASE opsecmkt_utf8_check ENCODING \'UTF8\' TEMPLATE template0"',
            'RESTORE_DATABASE_URL="$CHECK_DATABASE_URL" AGE_IDENTITY=... ./scripts/restore.sh backups/utf8-check.dump.age',
            'python3 scripts/postgres-tool.py NEW_DATABASE_URL psql -c "DROP DATABASE opsecmkt_utf8_check"',
            'python3 scripts/postgres-tool.py NEW_DATABASE_URL psql -X -f scripts/check-utf8.sql',
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
                      'scripts/test-utf8-migration.sh',
                      # QA5W4B-2 to -4: the restore refusal and its way out, the hint, the key.
                      'Refusing: this backup was taken from a LATIN1 database and holds its text in LATIN1.',
                      # QA5W5B-1 and -3: the database's own encoding, for the way out and for the reading.
                      'never the encoding its text was written in',
                      "In a `WIN1252` database the line names `WIN1252`",
                      'with `OLD_DATABASE_URL="$LATIN1_DATABASE_URL"`',
                      'Never start the market on it',
                      '*it also holds valid multibyte UTF-8*',
                      'Use the row\'s key, not its ctid'):
            self.assertIn(claim, section)
        self.assertNotIn("WHERE ctid = '(0,1)';", section)
        self.assertNotIn('--dbname="$', section)
        # QA5W6-3: NEW_DATABASE_URL names opsecmkt_utf8, which does not exist in step 3 or before step 4 creates it.
        self.assertNotIn('postgres-tool.py NEW_DATABASE_URL psql -c "CREATE DATABASE opsecmkt_utf8 ', section)
        self.assertNotIn('Refusing: this backup holds its text in', section)


class NoUrlOnArgvTests(unittest.TestCase):
    """A-483: a database URL on a command line, its password included, shows in the process list to every user of the
    host. Every client command of section 42 gets its URL from an exported variable through scripts/postgres-tool.py."""

    def test_no_client_command_takes_a_url_argument(self):
        section = flat(section_42())
        self.assertIsNone(re.search(r'\b(psql|pg_dump|pg_restore)\s+[\'"]?\$\{?\w*(URL|DATABASE)', section), 'a client command takes a URL argument')
        self.assertNotIn('postgres://', section)

    def test_every_tool_command_names_an_exported_variable(self):
        section = section_42()
        used = set(re.findall(r'postgres-tool\.py (\w+) (?:psql|pg_restore|pg_dump)\b', section))
        self.assertEqual(used, {'DATABASE_URL', 'OLD_DATABASE_URL', 'NEW_DATABASE_URL', 'MAINTENANCE_DATABASE_URL', 'LATIN1_DATABASE_URL'})
        for variable in used:
            self.assertRegex(section, rf'export [A-Z_0-9 ]*\b{variable}\b', f'{variable} is not exported before it is used')
        self.assertIn('in the process list', flat(section))


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


PG_RESTORE_STUB = r'''case " $* " in
*" --version "*) echo "pg_restore (PostgreSQL) ${RESTORE_TOOL_VERSION:-17.6}";;
*" --list "*) echo ";     Dumped by pg_dump version: ${DUMP_TOOL_VERSION:-17.6}";;
*" --use-list=/dev/null "*)
  read -r client source
  printf "SET client_encoding = '%s';\n" "$client"
  case " $* " in *" --create "*) [ -z "$source" ] || printf "CREATE DATABASE market WITH TEMPLATE = template0 ENCODING = '%s' LOCALE_PROVIDER = libc LOCALE = 'C';\n" "$source";; esac;;
*) cat >/dev/null; echo pg_restore ran >&2; exit 9;;
esac'''


class RestoreArchiveEncodingTests(unittest.TestCase):
    """QA5W4B-2, QA5W5B-1: restore.sh reads the dump's client_encoding and the encoding its DATABASE entry records for
    the database it was taken from, and before the restore runs refuses every dump but one written in SQL_ASCII (the
    bytes stored) or in UTF8 from a UTF8 or SQL_ASCII database. Stand-ins: age prints the "backup" (here "CLIENT
    SOURCE", SOURCE empty for a dump without a DATABASE entry); pg_restore with an empty list prints the SET
    client_encoding header for it and, with --create, the DATABASE entry's CREATE DATABASE, and otherwise reports that
    the restore ran; psql must never run."""

    def run_restore(self, encoding, source=None):
        with tempfile.TemporaryDirectory() as work:
            bin_dir = Path(work) / 'bin'
            bin_dir.mkdir()
            stubs = {'age': 'shift 3; cat "$1"', 'pg_restore': PG_RESTORE_STUB, 'psql': 'echo psql ran >&2; exit 9'}
            for tool, body in stubs.items():
                (bin_dir / tool).write_text(f'#!/bin/sh\n{body}\n')
                (bin_dir / tool).chmod(0o700)
            backup = Path(work) / 'market.dump.age'
            backup.write_text(f'{encoding} {encoding if source is None else source}\n' if encoding else '')
            env = dict(os.environ, PATH=f'{bin_dir}:{os.environ["PATH"]}', AGE_IDENTITY=str(Path(work) / 'identity'),
                       RESTORE_DATABASE_URL='postgres://user@127.0.0.1:1/db',
                       DUMP_TOOL_VERSION='17.6', RESTORE_TOOL_VERSION='17.6')
            env.pop('RESTORE_INTERNAL_DATABASE', None)
            return subprocess.run(['bash', str(RESTORE), str(backup)], env=env, input='RESTORE\n', capture_output=True, text=True)

    def assert_refused(self, result):
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('Nothing was changed.', result.stderr)
        self.assertIn('see UPGRADING.md, section 42.', result.stderr)
        self.assertNotIn('pg_restore ran', result.stderr)
        self.assertNotIn('psql ran', result.stderr)

    def test_other_encodings_refused_before_anything_runs(self):
        for encoding in ('LATIN1', 'WIN1252', 'EUC_JP'):
            with self.subTest(encoding):
                result = self.run_restore(encoding)
                self.assert_refused(result)
                self.assertIn(f'Refusing: this backup was taken from a {encoding} database and holds its text in {encoding}.', result.stderr)
                self.assertIn('every non-ASCII character the market stored would be double-encoded', result.stderr)
                self.assertIn(f'restore it into a temporary {encoding} database', result.stderr)

    def test_utf8_dump_of_a_database_in_another_encoding_refused(self):
        # QA5W5B-1: a LATIN1 database given ALTER DATABASE ... SET client_encoding='UTF8' is dumped as UTF8 by default,
        # the market's UTF-8 bytes converted from LATIN1: restored, they would come out double-encoded. The refusal and
        # its way out name the database's own encoding.
        for source in ('LATIN1', 'WIN1252', 'EUC_JP'):
            with self.subTest(source):
                result = self.run_restore('UTF8', source)
                self.assert_refused(result)
                self.assertIn(f'Refusing: this backup was taken from a {source} database and holds its text in UTF8.', result.stderr)
                self.assertIn('would be double-encoded', result.stderr)
                self.assertIn(f'restore it into a temporary {source} database', result.stderr)
                self.assertNotIn('temporary UTF8 database', result.stderr)

    def test_client_only_encoding_advice_names_the_database_encoding(self):
        # QA5W5B-1: a UTF8 database with client_encoding SJIS gives an SJIS dump. PostgreSQL creates no database in a
        # client-only encoding, so the way out names the database's own encoding, never the dump's.
        for client in ('SJIS', 'BIG5', 'GBK', 'UHC', 'GB18030', 'JOHAB'):
            with self.subTest(client):
                result = self.run_restore(client, 'UTF8')
                self.assert_refused(result)
                self.assertIn(f'Refusing: this backup was taken from a UTF8 database and holds its text in {client}.', result.stderr)
                self.assertIn(f'{client} is an encoding for clients only', result.stderr)
                self.assertIn('Back up the old database again with BACKUP_DUMP_ENCODING=SQL_ASCII', result.stderr)
                self.assertIn('restore it into a temporary UTF8 database', result.stderr)
                self.assertNotIn(f'temporary {client} database', result.stderr)
                self.assertNotIn('double-encoded', result.stderr)

    def test_dump_without_a_database_entry(self):
        # Every dump pg_dump 11 or later writes, scripts/backup.sh's included, records its database's encoding. Without
        # it a UTF8 dump cannot be told from a LATIN1 database's dumped as UTF8, so only SQL_ASCII (the bytes stored) is
        # restored, and the advice names no database encoding.
        for client in ('UTF8', 'LATIN1', 'SJIS'):
            with self.subTest(client):
                result = self.run_restore(client, '')
                self.assert_refused(result)
                self.assertIn(f'Refusing: this backup holds its text in {client} but does not record the encoding of the database it was taken from', result.stderr)
                self.assertIn('Take a new backup of that database with scripts/backup.sh', result.stderr)
                self.assertNotIn('temporary', result.stderr)
        result = self.run_restore('SQL_ASCII', '')
        self.assertNotIn('Refusing', result.stderr)
        self.assertIn('pg_restore ran', result.stderr)

    def test_unreadable_dump_refused(self):
        result = self.run_restore('')
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('Cannot read the database dump in this backup', result.stderr)
        self.assertNotIn('pg_restore ran', result.stderr)

    def test_utf8_and_sql_ascii_restored(self):
        for client, source in (('UTF8', 'UTF8'), ('UTF8', 'SQL_ASCII'), ('SQL_ASCII', 'SQL_ASCII'), ('SQL_ASCII', 'LATIN1'),
                               ('SQL_ASCII', 'WIN1252'), ('SQL_ASCII', 'UTF8')):
            with self.subTest(client=client, source=source):
                result = self.run_restore(client, source)
                self.assertNotIn('Refusing', result.stderr)
                self.assertIn('pg_restore ran', result.stderr)

    def test_older_restore_client_refused_before_confirmation_or_database_write(self):
        with tempfile.TemporaryDirectory() as work:
            bin_dir = Path(work) / 'bin'
            bin_dir.mkdir()
            stubs = {
                'age': 'shift 3; cat "$1"',
                'pg_restore': PG_RESTORE_STUB,
                'psql': 'echo psql ran >&2; exit 9',
            }
            for tool, body in stubs.items():
                (bin_dir / tool).write_text(f'#!/bin/sh\n{body}\n')
                (bin_dir / tool).chmod(0o700)
            backup = Path(work) / 'market.dump.age'
            backup.write_text('UTF8 UTF8\n')
            secret = 'credential-must-not-appear'
            env = dict(os.environ, PATH=f'{bin_dir}:{os.environ["PATH"]}',
                       AGE_IDENTITY=str(Path(work) / 'identity'),
                       RESTORE_DATABASE_URL=f'postgres://user:{secret}@127.0.0.1:1/db',
                       DUMP_TOOL_VERSION='17.6', RESTORE_TOOL_VERSION='16.9')
            result = subprocess.run(['bash', str(RESTORE), str(backup)], env=env,
                                    input='', capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertIn('selected pg_restore is older than the pg_dump client', result.stderr)
            self.assertNotIn('Restore into', result.stderr)
            self.assertNotIn('pg_restore ran', result.stderr)
            self.assertNotIn('psql ran', result.stderr)
            self.assertNotIn(secret, result.stdout + result.stderr)
            env['RESTORE_TOOL_VERSION'] = '17.6beta1'
            unknown = subprocess.run(['bash', str(RESTORE), str(backup)], env=env,
                                     input='', capture_output=True, text=True)
            self.assertEqual(unknown.returncode, 1, unknown.stderr)
            self.assertIn('Cannot safely verify PostgreSQL backup space or client-version compatibility', unknown.stderr)
            self.assertNotIn('Restore into', unknown.stderr)
            self.assertNotIn('pg_restore ran', unknown.stderr)
            self.assertNotIn('psql ran', unknown.stderr)
            self.assertNotIn(secret, unknown.stdout + unknown.stderr)


class CheckScriptTests(unittest.TestCase):
    def test_read_only_and_stops_psql(self):
        text = CHECK.read_text()
        code = '\n'.join(line for line in text.splitlines() if not line.lstrip().startswith('--'))
        self.assertIn('\\set ON_ERROR_STOP on', code)
        self.assertIsNone(re.search(r'\b(INSERT|UPDATE|DELETE|ALTER|DROP|CREATE|TRUNCATE|GRANT)\b', code, re.I))
        self.assertIn("convert(convert_to(r.val, 'SQL_ASCII'), 'UTF8', 'UTF8')", code)
        self.assertIn('RAISE EXCEPTION', code)
        # QA5W4B-3 and -4, QA5W5B-3: each bad value says whether reading it as the database's own single-byte encoding
        # (LATIN1 for SQL_ASCII) is safe, and names its row by its key.
        self.assertIn("no valid multibyte UTF-8 in it, so reading its bytes as %s, this database''s encoding, is a safe correction", code)
        self.assertIn('it also holds valid multibyte UTF-8, which reading it as %s would double-encode: type the text it should hold', code)
        self.assertIn("is multibyte, so no reading of its bytes is a safe correction: type the text it should hold", code)
        self.assertIn("pg_encoding_max_length(pg_char_to_encoding(server_encoding)) = 1", code)
        self.assertIn("shown_key := 'where ' || r.key;", code)
        for typ in ("'text'::regtype", "'varchar'::regtype", "'bpchar'::regtype"):
            self.assertIn(typ, code)
        # QA5W5B-2 and -8: arrays (by element) and domains are read; names are printed quoted as SQL needs.
        self.assertIn("t.typcategory = 'A'", code)
        self.assertIn("t.typtype = 'd'", code)
        self.assertIn("format('%I.%I', col.nspname, col.relname), format('%I', col.attname) || r.pos", code)


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
                       "WHERE id = 'utf8mixed'",
                       'mixed_hex=4372c3a86d6520ff206272c3bb6cc3a965',
                       'it also holds valid multibyte UTF-8, which reading it as LATIN1 would double-encode',
                       'check ascii_old || fail \'check-utf8.sql failed after one round of corrections\'',
                       "title='Crème brûlée — 東京 🔒'",
                       'if restore "$work/latin1.dump.age" latin1_plain; then fail',
                       'Refusing: this backup was taken from a LATIN1 database and holds its text in LATIN1.',
                       # QA5W5B-1 to -3, -6 and -8.
                       "ALTER DATABASE latin1_old SET client_encoding = 'UTF8'",
                       'if restore "$work/latin1-set.dump.age" latin1_set; then fail',
                       'Refusing: this backup was taken from a LATIN1 database and holds its text in UTF8.',
                       "ALTER DATABASE sjis_src SET client_encoding = 'SJIS'",
                       "if grep -q 'temporary SJIS database'",
                       "column tags[2]", "column labels[1]", "row where a = 'x' AND b = '1'",
                       'table public.\\"Odd Table\\", column \\"Odd Col\\"',
                       'e69db1e4baac20ff', '$type_it',
                       "reading its bytes as WIN1252, this database's encoding, is a safe correction",
                       "this database's encoding, EUC_JP, is multibyte",
                       # QA5W6-3: the way out runs as printed, before step 4 creates opsecmkt_utf8.
                       'bash -euo pipefail -c "$escape_block"',
                       'NEW_DATABASE_URL=$(url opsecmkt_utf8)',
                       'restore_into "$work/latin1-again.dump.age" opsecmkt_utf8',
                       'if BACKUP_DUMP_ENCODING=LATIN1 backup',
                       'BACKUP_DUMP_ENCODING=SQL_ASCII backup latin1_old',
                       'BACKUP_DUMP_ENCODING=SQL_ASCII round_trip latin1_new',
                       # A-483: the section's own client commands run, not copies of them.
                       'doc_command show_encoding', 'doc_command check_old', 'doc_command create_utf8', 'doc_command check_new',
                       'doc_command type_in', 'doc_lines round_trip', 'status == 3',
                       'docker rm -f -v "$container"'):
            self.assertIn(needle, text)


if __name__ == '__main__':
    unittest.main()
