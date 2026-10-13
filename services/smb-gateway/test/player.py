"""Plays a file over SMB like a video player: prebuffers, then consumes at a
fixed bitrate while reading ahead in 1 MiB requests to keep its own buffer
topped up. Counts stalls (the buffer ran empty) during forward playback.

player.py SERVER SHARE FILE USER PASSWORD BITRATE_MBPS SECONDS
Prints one line: stalls total_stall_s worst_read_ms startup_ms"""
import sys
import time

import smbclient

server, share, name, user, password = sys.argv[1:6]
bitrate, seconds = float(sys.argv[6]) * 1e6, float(sys.argv[7])
CHUNK = 1 << 20
PREBUFFER = 2.0  # seconds of video before playback starts
TARGET = 3.0     # seconds buffered; players treat SMB like a local disk

smbclient.register_session(server, username=user, password=password)
f = smbclient.open_file(rf"\\{server}\{share}\{name}", mode="rb", buffering=0)

t0 = time.monotonic()
buffered = 0.0          # seconds of video in the player's buffer
played = 0.0
playing = False
stalls, stall_time, worst = 0, 0.0, 0.0
stall_started = None
startup = None
clock = time.monotonic()

while played < seconds:
    now = time.monotonic()
    if playing:
        dt = now - clock
        use = min(dt, buffered)
        buffered -= use
        played += use
        if buffered <= 0 and use < dt:
            playing = False
            stalls += 1
            stall_started = now
    clock = now
    if buffered < TARGET:
        s = time.monotonic()
        data = f.read(CHUNK)
        took = time.monotonic() - s
        if not data:
            break
        worst = max(worst, took)
        buffered += len(data) / bitrate
        if not playing and buffered >= PREBUFFER:
            playing = True
            clock = time.monotonic()
            if startup is None:
                startup = clock - t0
            elif stall_started is not None:
                stall_time += clock - stall_started
                stall_started = None
    else:
        time.sleep(0.05)

print(stalls, f"{stall_time:.1f}", int(worst * 1000), int((startup or 0) * 1000))
