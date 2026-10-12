"""TCP proxy that waits before connecting each client to the upstream, to
make an SSH handshake as slow as a real remote one: slow-proxy.py LISTEN_PORT
UPSTREAM_HOST UPSTREAM_PORT DELAY_SECONDS"""
import socket
import sys
import threading
import time

port, host, uport, delay = int(sys.argv[1]), sys.argv[2], int(sys.argv[3]), float(sys.argv[4])


def pipe(a, b):
    try:
        while data := a.recv(65536):
            b.sendall(data)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def handle(client):
    time.sleep(delay)
    upstream = socket.create_connection((host, uport))
    threading.Thread(target=pipe, args=(client, upstream), daemon=True).start()
    pipe(upstream, client)


srv = socket.create_server(("0.0.0.0", port))
while True:
    c, _ = srv.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
