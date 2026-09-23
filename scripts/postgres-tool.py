#!/usr/bin/env python3
"""Run a PostgreSQL client with URL credentials passed only through environment."""
import os
import sys
from urllib.parse import parse_qsl, unquote, urlsplit


def main():
    if len(sys.argv) < 3 or sys.argv[2] not in {'pg_dump', 'pg_restore'}:
        raise ValueError('Expected URL environment variable and pg_dump or pg_restore')
    parsed = urlsplit(os.environ[sys.argv[1]])
    if parsed.scheme not in {'postgres', 'postgresql'} or not parsed.hostname or not parsed.path.strip('/'):
        raise ValueError('Use a PostgreSQL URL containing an explicit host and database name')
    # Prevent inherited libpq defaults from redirecting the explicit target.
    env = {key: value for key, value in os.environ.items() if not key.startswith('PG')}
    env.update(PGHOST=unquote(parsed.hostname), PGPORT=str(parsed.port or 5432), PGDATABASE=unquote(parsed.path[1:]))
    if parsed.username is not None:
        env['PGUSER'] = unquote(parsed.username)
    if parsed.password is not None:
        env['PGPASSWORD'] = unquote(parsed.password)
    supported = {
        'sslmode': 'PGSSLMODE', 'sslrootcert': 'PGSSLROOTCERT',
        'sslcert': 'PGSSLCERT', 'sslkey': 'PGSSLKEY', 'sslcrl': 'PGSSLCRL',
        'sslcrldir': 'PGSSLCRLDIR', 'connect_timeout': 'PGCONNECT_TIMEOUT',
        'application_name': 'PGAPPNAME', 'options': 'PGOPTIONS',
        'channel_binding': 'PGCHANNELBINDING', 'gssencmode': 'PGGSSENCMODE',
        'target_session_attrs': 'PGTARGETSESSIONATTRS', 'passfile': 'PGPASSFILE',
    }
    for key, value in parse_qsl(parsed.query, keep_blank_values=True):
        if key not in supported:
            raise ValueError('Unsupported PostgreSQL URL query option: ' + key)
        env[supported[key]] = value
    if any('\x00' in value for value in env.values()):
        raise ValueError('Null bytes are not allowed in connection parameters')
    os.execvpe(sys.argv[2], sys.argv[2:], env)


if __name__ == '__main__':
    try:
        main()
    except (KeyError, ValueError) as exc:
        # Do not print raw URLs or library parsing errors that might contain passwords.
        print('Invalid PostgreSQL connection configuration; use a host/database URL with supported libpq options.', file=sys.stderr)
        sys.exit(2)
