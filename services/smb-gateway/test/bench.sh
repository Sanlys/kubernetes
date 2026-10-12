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
NET=smbgw-bench
WORK=$(mktemp -d)
cleanup() {
  docker rm -f bgw bs3 >/dev/null 2>&1 || true
  docker network rm $NET >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || sudo rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

docker network create $NET >/dev/null
mkdir -p "$WORK"/s3/media "$WORK"/cfg
head -c "$((SIZE_MB * 1024 * 1024))" /dev/urandom > "$WORK"/s3/media/video.mkv
docker run -d --name bs3 --network $NET -v "$WORK"/s3:/data "$RCLONE_IMAGE" \
  serve s3 /data --addr :9000 --auth-key testaccesskey,testsecretkey0123456789 >/dev/null
docker build -q -t smbgw-client - >/dev/null <<EOF
FROM $CLIENT_BASE
RUN apk add --no-cache samba-client
EOF

# smbclient prints "(<n> KiloBytes/sec)"; report MB/s.
read_mbps() {
  docker run --rm --network $NET smbgw-client smbclient //bgw/media -U u%benchpass -m SMB3 \
    -c 'get video.mkv /dev/null' 2>&1 | sed -n 's/.*(\([0-9.]*\) KiloBytes\/sec).*/\1/p' | awk '{printf "%.0f", $1/1024}'
}

run_variant() { # name nfs_read_ahead read_chunk_streams
  docker rm -f bgw >/dev/null 2>&1 || true
  sudo rm -rf "$WORK"/cache 2>/dev/null || rm -rf "$WORK"/cache
  mkdir -p "$WORK"/cache
  cat > "$WORK"/cfg/config.yaml <<EOF
server: {name: bench}
s3: {endpoint: "http://bs3:9000", access_key_id: testaccesskey, secret_access_key: testsecretkey0123456789}
notifications: {enabled: true, manage: false, token: bench-token-0123456789}
cache: {max_size: 4G, min_free_space: 100M, nfs_read_ahead: $2, read_chunk_streams: $3}
mounts:
  - {path: /media, type: s3, bucket: media, transport: nfs}
users: [{name: u, password: benchpass}]
shares: [{name: media, path: /media, read: [u]}]
EOF
  docker run -d --name bgw --network $NET --privileged --device /dev/fuse \
    -v "$WORK"/cfg:/etc/smb-gateway -v "$WORK"/cache:/cache "$IMAGE" run >/dev/null
  for i in $(seq 1 60); do docker exec bgw smb-gateway health >/dev/null 2>&1 && break; sleep 1; done
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
