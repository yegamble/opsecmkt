"""Connection handling regressions without invoking PostgreSQL or a network."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

TOOL = Path(__file__).resolve().parents[1] / 'scripts' / 'postgres-tool.py'
spec = importlib.util.spec_from_file_location('postgres_tool', TOOL)
postgres_tool = importlib.util.module_from_spec(spec)
spec.loader.exec_module(postgres_tool)


class PostgresToolTests(unittest.TestCase):
    def invoke(self, url, command='pg_dump', arguments=None, extra_env=None):
        env = {'TEST_URL': url, 'PATH': '/usr/bin', **(extra_env or {})}
        with patch.dict(os.environ, env, clear=True), patch.object(
            sys, 'argv', [str(TOOL), 'TEST_URL', command, *(arguments or [])]
        ), patch.object(postgres_tool.os, 'execvpe') as execute:
            postgres_tool.main()
            execute.assert_called_once()
            return execute.call_args.args

    def test_encoded_credentials_database_and_explicit_port(self):
        secret = 'a secret@:/?%'
        url = 'postgresql://alice%40team:a%20secret%40%3A%2F%3F%25@db.example:5433/store%20one'
        executable, argv, env = self.invoke(url, arguments=['--format=custom'])
        self.assertEqual(executable, 'pg_dump')
        self.assertEqual(argv, ['pg_dump', '--format=custom'])
        self.assertEqual(env['PGHOST'], 'db.example')
        self.assertEqual(env['PGPORT'], '5433')
        self.assertEqual(env['PGDATABASE'], 'store one')
        self.assertEqual(env['PGUSER'], 'alice@team')
        self.assertEqual(env['PGPASSWORD'], secret)
        self.assertNotIn(secret, repr(argv))
        self.assertNotIn(url, repr(argv))

    def test_ipv6_and_default_port(self):
        _, _, env = self.invoke('postgres://reader@[::1]/market')
        self.assertEqual(env['PGHOST'], '::1')
        self.assertEqual(env['PGPORT'], '5432')
        self.assertNotIn('PGPASSWORD', env)

    def test_inherited_libpq_defaults_are_removed(self):
        inherited = {name: 'untrusted' for name in (
            'PGHOST', 'PGHOSTADDR', 'PGPORT', 'PGDATABASE', 'PGUSER',
            'PGPASSWORD', 'PGSERVICE', 'PGSERVICEFILE', 'PGPASSFILE',
            'PGOPTIONS', 'PGSSLMODE', 'PGTARGETSESSIONATTRS',
        )}
        _, _, env = self.invoke('postgresql://db/market', extra_env=inherited)
        self.assertEqual({key: value for key, value in env.items() if key.startswith('PG')},
                         {'PGHOST': 'db', 'PGPORT': '5432', 'PGDATABASE': 'market'})
        self.assertEqual(env['PATH'], '/usr/bin')

    def test_supported_query_values_and_restore_arguments(self):
        _, argv, env = self.invoke(
            'postgres://db/market?sslmode=verify-full&sslrootcert=%2Fcerts%2Froot.pem'
            '&connect_timeout=5&application_name=backup+test&options=-c%20statement_timeout%3D10'
            '&passfile=%2Fsecrets%2Fpgpass',
            command='pg_restore', arguments=['--dbname=', '--single-transaction'],
        )
        self.assertEqual(argv, ['pg_restore', '--dbname=', '--single-transaction'])
        self.assertEqual(env['PGSSLMODE'], 'verify-full')
        self.assertEqual(env['PGSSLROOTCERT'], '/certs/root.pem')
        self.assertEqual(env['PGCONNECT_TIMEOUT'], '5')
        self.assertEqual(env['PGAPPNAME'], 'backup test')
        self.assertEqual(env['PGOPTIONS'], '-c statement_timeout=10')
        self.assertEqual(env['PGPASSFILE'], '/secrets/pgpass')

    def test_target_overrides_and_unsupported_queries_are_rejected(self):
        for key in ('host', 'hostaddr', 'port', 'dbname', 'user', 'password', 'service', 'unknown'):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.invoke('postgres://db/market?' + key + '=elsewhere')

    def test_invalid_urls_and_nul_parameters_are_rejected(self):
        for url in (
            'https://db/market', 'postgres:///market', 'postgres://db',
            'postgres://db/', 'postgres://db:invalid/market', 'postgres://db:65536/market',
            'postgres://u:secret%00@db/market', 'postgres://db/market%00',
            'postgres://db%00/market', 'postgres://db/market?application_name=%00',
        ):
            with self.subTest(url=url), self.assertRaises(ValueError):
                self.invoke(url)

    def test_psql_for_restore_hold(self):
        executable, argv, env = self.invoke('postgres://u:pw@db/market', command='psql',
                                            arguments=['-X', '--single-transaction'])
        self.assertEqual(executable, 'psql')
        self.assertEqual(argv, ['psql', '-X', '--single-transaction'])
        self.assertEqual(env['PGPASSWORD'], 'pw')
        self.assertNotIn('pw', repr(argv))

    def test_arbitrary_executables_are_rejected(self):
        with self.assertRaises(ValueError):
            self.invoke('postgres://db/market', command='sh')

    def test_cli_errors_do_not_echo_connection_secrets(self):
        secret = 'regression-secret-never-print'
        for url in (f'postgres://u:{secret}@db:invalid/market',
                    f'postgres://db/market?{secret}=value'):
            result = subprocess.run(
                [sys.executable, str(TOOL), 'TEST_URL', 'pg_dump'],
                env={**os.environ, 'TEST_URL': url}, capture_output=True, text=True,
                check=False,
            )
            self.assertEqual(result.returncode, 2)
            self.assertEqual(result.stdout, '')
            self.assertIn('Invalid PostgreSQL connection configuration', result.stderr)
            self.assertNotIn(secret, result.stderr)
            self.assertNotIn(url, result.stderr)
            self.assertNotIn('Traceback', result.stderr)


if __name__ == '__main__':
    unittest.main()
