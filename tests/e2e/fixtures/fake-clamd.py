#!/usr/bin/env python3
"""Loopback-only fake clamd for the database browser suite. Not a malware scanner.

Answers the NUL-terminated zPING, zVERSION and zINSTREAM commands as clamd does, one command per connection.
VERSION reports daily signatures built FAKE_CLAMD_AGE_DAYS (default 2) days before this process started, so
the app's staleness gate passes on any calendar day. A stream is "stream: OK" unless it contains the test
marker, which is FOUND (the Go tests' fakeClamd, internal/market/clamd_fake_test.go, behaves the same).
"""
import os
import socketserver
import struct
import time

PORT = int(os.environ.get('FAKE_CLAMD_PORT', '18086'))
AGE_DAYS = int(os.environ.get('FAKE_CLAMD_AGE_DAYS', '2'))
MARKER = b'OPSECMKT-CLAMD-TEST-MARKER'
SIGNATURE = 'Opsecmkt.Test.Marker.UNOFFICIAL'
_t = time.gmtime(time.time() - AGE_DAYS * 86400)
# ANSI C asctime in English whatever the locale: "Thu Sep  5 07:57:02 2026" (day padded with a space).
BUILT = '%s %s %2d %02d:%02d:%02d %d' % ('Mon Tue Wed Thu Fri Sat Sun'.split()[_t.tm_wday],
                                         'Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec'.split()[_t.tm_mon - 1],
                                         _t.tm_mday, _t.tm_hour, _t.tm_min, _t.tm_sec, _t.tm_year)
VERSION = 'ClamAV 1.4.3/27999/' + BUILT


class Handler(socketserver.BaseRequestHandler):
    def read_exact(self, n):
        data = b''
        while len(data) < n:
            chunk = self.request.recv(n - len(data))
            if not chunk:
                raise EOFError
            data += chunk
        return data

    def handle(self):
        self.request.settimeout(30)
        try:
            command = b''
            while not command.endswith(b'\0'):
                if len(command) > 64:
                    return
                command += self.read_exact(1)
            command = command[:-1]
            if command == b'zPING':
                reply = 'PONG'
            elif command == b'zVERSION':
                reply = VERSION
            elif command == b'zINSTREAM':
                stream = b''
                while True:
                    (size,) = struct.unpack('>I', self.read_exact(4))
                    if size == 0:
                        break
                    if size > 1 << 20 or len(stream) + size > 9 << 20:
                        self.request.sendall(b'INSTREAM size limit exceeded. ERROR\0')
                        return
                    stream += self.read_exact(size)
                reply = 'stream: %s FOUND' % SIGNATURE if MARKER in stream else 'stream: OK'
            else:
                reply = 'UNKNOWN COMMAND'
            self.request.sendall(reply.encode() + b'\0')
        except (EOFError, OSError):
            return


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


if __name__ == '__main__':
    with Server(('127.0.0.1', PORT), Handler) as server:
        server.serve_forever()
