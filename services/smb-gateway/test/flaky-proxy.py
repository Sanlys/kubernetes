"""TCP proxy that models a backend which is fast enough on average but
stalls now and then: every PERIOD seconds it forwards nothing at all for
STALL seconds, and otherwise caps the total throughput (all connections
together) at RATE MB/s.

flaky-proxy.py LISTEN_PORT UPSTREAM_HOST UPSTREAM_PORT RATE_MBPS PERIOD STALL"""
import socket
import sys
import threading
import time

port, host, uport = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])
rate, period, stall = float(sys.argv[4]) * 1e6, float(sys.argv[5]), float(sys.argv[6])
start = time.monotonic()
lock = threading.Lock()
tokens, last = 0.0, start


def take(n):
    """Block until n bytes may pass."""
    global tokens, last
    while True:
        with lock:
            now = time.monotonic()
            phase = (now - start) % period
            if phase >= period - stall:
                wait = period - phase
            else:
                tokens = min(tokens + (now - last) * rate, rate / 10)
                last = now
                if tokens >= n:
                    tokens -= n
                    return
                wait = (n - tokens) / rate
        time.sleep(wait)


def pipe(a, b, limited):
    try:
        while data := a.recv(65536):
            if limited:
                take(len(data))
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
    upstream = socket.create_connection((host, uport))
    threading.Thread(target=pipe, args=(client, upstream, False), daemon=True).start()
    pipe(upstream, client, True)


srv = socket.create_server(("0.0.0.0", port))
while True:
    c, _ = srv.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
