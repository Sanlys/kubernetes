#!/usr/bin/env bash
# Streaming-read benchmark through the whole path: smbclient -> smbd ->
# kernel NFS -> rclone (VFS cache) -> S3 (rclone serve s3 on local disk).
# Compares settings that affect sequential reads; each variant starts with
# an empty cache (cold: data comes from S3) and then reads again (warm:
# served from the gateway's cache).
#
# Needs the kernel NFS client, like the integration test's nfs mode.
# Usage: test/bench.sh [image] [size_mb]
set -euo pipefail

IMAGE=${1:-smb-gateway:dev}
SIZE_MB=${2:-1024}
RCLONE_IMAGE=${RCLONE_IMAGE:-rclone/rclone:1.75.2}
CLIENT_BASE=${CLIENT_BASE:-alpine:3.24}
PYTHON_IMAGE=${PYTHON_IMAGE:-python:3.13-alpine}
NET=smbgw-bench
WORK=$(mktemp -d)
cleanup() {
  docker rm -f bgw bs3 bstream bflaky >/dev/null 2>&1 || true
  docker network rm $NET >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || sudo rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

docker network create $NET >/dev/null
mkdir -p "$WORK"/s3/media "$WORK"/cfg
head -c "$((SIZE_MB * 1024 * 1024))" /dev/urandom > "$WORK"/s3/media/video.mkv
start_s3() { # [bwlimit]
  docker rm -f bs3 >/dev/null 2>&1 || true
  docker run -d --name bs3 --network $NET -v "$WORK"/s3:/data "$RCLONE_IMAGE" \
    serve s3 /data --addr :9000 --auth-key testaccesskey,testsecretkey0123456789 ${1:+--bwlimit $1} >/dev/null
  sleep 1
}
start_s3
head -c $((8 * 1024 * 1024)) /dev/urandom > "$WORK"/s3/media/small.bin
docker build -q -t smbgw-client - >/dev/null <<EOF
FROM $CLIENT_BASE
RUN apk add --no-cache samba-client coreutils
EOF
docker build -q -t smbgw-player - >/dev/null <<EOF
FROM $PYTHON_IMAGE
RUN pip install --no-cache-dir smbprotocol==1.15.0
EOF
HERE=$(cd "$(dirname "$0")" && pwd)

# smbclient prints "(<n> KiloBytes/sec)"; report MB/s.
read_mbps() {
  docker run --rm --network $NET smbgw-client smbclient //bgw/media -U u%benchpass -m SMB3 \
    -c 'get video.mkv /dev/null' 2>&1 | sed -n 's/.*(\([0-9.]*\) KiloBytes\/sec).*/\1/p' | awk '{printf "%.0f", $1/1024}'
}

start_gw() { # nfs_read_ahead read_chunk_streams nfs_connections transport [read_ahead] [s3 host]
  docker rm -f bgw >/dev/null 2>&1 || true
  sudo rm -rf "$WORK"/cache 2>/dev/null || rm -rf "$WORK"/cache
  mkdir -p "$WORK"/cache
  cat > "$WORK"/cfg/config.yaml <<EOF
server: {name: bench}
s3: {endpoint: "http://${6:-bs3}:9000", access_key_id: testaccesskey, secret_access_key: testsecretkey0123456789}
notifications: {enabled: true, manage: false, token: bench-token-0123456789}
cache: {max_size: 4G, min_free_space: 100M, nfs_read_ahead: $1, read_chunk_streams: $2, nfs_connections: ${3:-8}, read_ahead: ${5:-64M}}
mounts:
  - {path: /media, type: s3, bucket: media, transport: ${4:-nfs}}
users: [{name: u, password: benchpass}]
shares: [{name: media, path: /media, read: [u]}]
EOF
  docker run -d --name bgw --network $NET --privileged --device /dev/fuse \
    -v "$WORK"/cfg:/etc/smb-gateway -v "$WORK"/cache:/cache "$IMAGE" run >/dev/null
  for i in $(seq 1 60); do docker exec bgw smb-gateway health >/dev/null 2>&1 && break; sleep 1; done
}

run_variant() { # name nfs_read_ahead read_chunk_streams
  start_gw "$2" "$3"
  local ra cold warm
  ra=$(docker exec bgw wget -qO- http://127.0.0.1:8090/metrics | sed -n 's/^smbgw_nfs_read_ahead_bytes{mount="\/media"} //p')
  cold=$(read_mbps)
  warm=$(read_mbps)
  printf '| %s | %s | %s | %s | %s |\n' "$1" "$(awk -v b="${ra:-0}" 'BEGIN{printf "%d KiB", b/1024}')" "$3" "$cold" "$warm"
}

{
  echo "### smb-gateway streaming read, ${SIZE_MB} MiB file over SMB3"
  echo
  echo "| variant | readahead in effect | chunk streams | cold MB/s | warm MB/s |"
  echo "|---|---|---|---|---|"
  run_variant "kernel default readahead" 128K 0
  run_variant "nfs_read_ahead 16M (default)" 16M 0
  run_variant "16M + 4 chunk streams" 16M 4
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"

# Head-of-line blocking: one client streams a cold file from a slow backend
# (20 MB/s) while another keeps re-reading a small file that is already
# cached. rclone's NFS server answers one connection's requests strictly in
# order, so cached reads can queue behind reads waiting on S3; FUSE serves
# requests concurrently. The baselines show the cost without the stream.
small_read_ms() {
  docker run --rm --network $NET smbgw-client sh -c '
    for i in $(seq 1 20); do
      s=$(date +%s%N); smbclient //bgw/media -U u%benchpass -m SMB3 -c "get small.bin /dev/null" >/dev/null 2>&1
      echo $(( ($(date +%s%N) - s) / 1000000 )); sleep 0.5
    done' | sort -n | awk '{a[NR]=$1} END {printf "%d | %d", a[int(NR/2)+1], a[NR]}'
}
hol_variant() { # label connections transport stream(yes/no)
  start_s3 20M
  start_gw 16M 0 "$2" "$3"
  docker run --rm --network $NET smbgw-client smbclient //bgw/media -U u%benchpass -m SMB3 -c 'get small.bin /dev/null' >/dev/null 2>&1
  if [ "$4" = yes ]; then
    docker run -d --name bstream --network $NET smbgw-client smbclient //bgw/media -U u%benchpass -m SMB3 -c 'get video.mkv /dev/null' >/dev/null
    sleep 3
  fi
  printf '| %s | %s |\n' "$1" "$(small_read_ms)"
  docker rm -f bstream >/dev/null 2>&1 || true
}
{
  echo
  echo "### Cached 8 MiB reads while another client streams from a 20 MB/s backend"
  echo
  echo "| variant | median ms | worst ms |"
  echo "|---|---|---|"
  hol_variant "nfs, no other stream (baseline)" 8 nfs no
  hol_variant "nfs, 1 connection" 1 nfs yes
  hol_variant "nfs, 8 connections" 8 nfs yes
  hol_variant "fuse, no other stream (baseline)" 8 fuse no
  hol_variant "fuse" 8 fuse yes
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"

# Forward playback from a backend that is fast enough on average but stalls
# now and then: 25 MB/s, except that every 12 s nothing arrives for 6 s
# (12.5 MB/s on average). The player consumes 8 MB/s (a high-bitrate 4K
# file), starts after 2 s and keeps only 3 s buffered, as players do for
# what looks like a local disk. A stall is its buffer running empty - a
# visible stutter; "worst read" is the longest single 1 MiB read.
start_s3
docker run -d --name bflaky --network $NET -v "$HERE"/flaky-proxy.py:/p.py:ro \
  "$PYTHON_IMAGE" python -u /p.py 9000 bs3 9000 25 12 6 >/dev/null
play_variant() { # label transport read_ahead
  start_gw 16M 0 8 "$2" "$3" bflaky
  printf '| %s | %s |\n' "$1" "$(docker run --rm --network $NET -v "$HERE"/player.py:/player.py:ro \
    smbgw-player python /player.py bgw media video.mkv u benchpass 8 90 | awk '{printf "%s | %s | %s | %s", $1, $2, $3, $4}')"
}
{
  echo
  echo "### Playback at 8 MB/s from a flaky backend (12.5 MB/s on average), 90 s"
  echo
  echo "| variant | stalls | stalled s | worst read ms | startup ms |"
  echo "|---|---|---|---|---|"
  play_variant "nfs, read_ahead 64M" nfs 64M
  play_variant "nfs, read_ahead 512M" nfs 512M
  play_variant "fuse, read_ahead 64M" fuse 64M
  play_variant "fuse, read_ahead 512M" fuse 512M
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
