#!/usr/bin/env python3
"""Runs a second app process with a real ADMIN_SOCKET for the database browser suite, and a loopback bridge to it.

Browsers cannot dial a Unix socket; a deployment reaches this one through a client-authorized Tor service. The bridge
stands in for that service only: it copies bytes between PRIVATE_ADMIN_BRIDGE_PORT on loopback and the socket, adds no
header and has no route of its own, so every request reaches the real AdminHandler and every check on it still runs.
The app's public listener stays on PRIVATE_ADMIN_PUBLIC_ADDR, where the spec checks it refuses the same routes.
Test-only and never shipped: the deployment has no TCP administrator port.
"""
import atexit
import os
import shutil
import signal
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading

BRIDGE_PORT = int(os.environ['PRIVATE_ADMIN_BRIDGE_PORT'])
SOCKET_DIR = tempfile.mkdtemp(prefix='om-admin-')  # 0700, which listenAdmin requires
SOCKET_PATH = os.path.join(SOCKET_DIR, 'http.sock')


def pump(source, sink):
    try:
        while chunk := source.recv(65536):
            sink.sendall(chunk)
    except OSError:
        pass
    finally:
        try:
            sink.shutdown(socket.SHUT_WR)
        except OSError:
            pass


class Bridge(socketserver.BaseRequestHandler):
    def handle(self):
        upstream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            upstream.connect(SOCKET_PATH)
        except OSError:  # the app is still starting: the caller polls
            upstream.close()
            return
        back = threading.Thread(target=pump, args=(upstream, self.request), daemon=True)
        back.start()
        pump(self.request, upstream)
        back.join()
        upstream.close()


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    atexit.register(shutil.rmtree, SOCKET_DIR, ignore_errors=True)
    env = dict(os.environ, ADDR=os.environ['PRIVATE_ADMIN_PUBLIC_ADDR'], APP_MODE='tor', ADMIN_SOCKET=SOCKET_PATH)
    app = subprocess.Popen(['go', 'run', './cmd/server'], env=env)

    def stop(*_):
        app.terminate()
        sys.exit(0)

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    with Server(('127.0.0.1', BRIDGE_PORT), Bridge) as server:
        threading.Thread(target=server.serve_forever, daemon=True).start()
        sys.exit(app.wait())


if __name__ == '__main__':
    main()
