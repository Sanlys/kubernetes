#!/usr/bin/env bash
# End-to-end test of the gateway image against real components:
#   - rclone serve s3 as the S3 backend (its data dir is the "bucket" store,
#     so the test can make changes behind the gateway's back)
#   - a real restic repository in that S3
#   - an SFTP server + a fake Syncthing events API for the syncthing mount
#   - smbclient as the SMB client
#
# TRANSPORT=nfs needs a kernel with the NFS client (GitHub's runners have it);
# TRANSPORT=fuse runs anywhere with /dev/fuse.
#
# Usage: test/integration.sh [image]   (default smb-gateway:dev)
set -euo pipefail

IMAGE=${1:-smb-gateway:dev}
TRANSPORT=${TRANSPORT:-nfs}
RCLONE_IMAGE=${RCLONE_IMAGE:-rclone/rclone:1.75.2}
RESTIC_IMAGE=${RESTIC_IMAGE:-restic/restic:0.19.1}
SFTP_IMAGE=${SFTP_IMAGE:-atmoz/sftp:alpine}
CLIENT_BASE=${CLIENT_BASE:-alpine:3.24}
NET=smbgw-test
TOKEN=test-token-0123456789abcdef
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)

cleanup() {
  status=$?
  if [ $status -ne 0 ]; then
    echo "---- gateway logs ----"; docker logs gw 2>&1 | tail -150 || true
    echo "---- health ----"; docker exec gw wget -qO- http://127.0.0.1:8090/healthz 2>/dev/null || true
  fi
  if [ -n "${KEEP:-}" ]; then echo "KEEP set, leaving containers and $WORK"; exit $status; fi
  docker rm -f gw s3 sftp syncthing >/dev/null 2>&1 || true
  docker network rm $NET >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || sudo rm -rf "$WORK" 2>/dev/null || true
  exit $status
}
trap cleanup EXIT

pass() { echo "PASS: $*"; }
# The fake S3 (rclone serve s3) caches its own listings for 1s; RGW updates
# its bucket index before it sends the notification, so wait that out.
s3_settle() { sleep 2; }
fail() { echo "FAIL: $*"; exit 1; }

docker network create $NET >/dev/null
mkdir -p "$WORK"/s3/media "$WORK"/s3/photos "$WORK"/s3/restic "$WORK"/st/docs "$WORK"/cfg "$WORK"/cache "$WORK"/src
echo "hello from s3" > "$WORK"/s3/media/existing.txt
mkdir -p "$WORK"/s3/media/shows/s01 && echo e1 > "$WORK"/s3/media/shows/s01/e01.txt
echo "photo" > "$WORK"/s3/photos/p1.jpg
echo "synced doc" > "$WORK"/st/docs/readme.txt
chmod -R a+rwX "$WORK"/st

docker run -d --name s3 --network $NET -v "$WORK"/s3:/data "$RCLONE_IMAGE" \
  serve s3 /data --addr :9000 --auth-key testaccesskey,testsecretkey0123456789 --dir-cache-time 1s --vfs-cache-mode writes >/dev/null
docker run -d --name sftp --network $NET -v "$WORK"/st:/home/user/upload "$SFTP_IMAGE" user:pass:1000:1000 >/dev/null
docker run -d --name syncthing --network $NET -v "$HERE"/fake-syncthing.py:/app.py:ro \
  --entrypoint python3 "${PYTHON_IMAGE:-python:3.13-alpine}" /app.py >/dev/null

# Client image with smbclient.
docker build -q -t smbgw-client - >/dev/null <<EOF
FROM $CLIENT_BASE
RUN apk add --no-cache samba-client curl
EOF
client() { docker run --rm --network $NET -v "$WORK"/src:/src smbgw-client "$@"; }
smb() { # smb <user> <pass> <share> <commands>
  client smbclient "//gw/$3" -U "$1%$2" -m SMB3 -c "$4" 2>&1
}

# A restic repository with one snapshot.
echo "backed up file" > "$WORK"/src/backed-up.txt
for i in $(seq 1 30); do
  docker run --rm --network $NET -e AWS_ACCESS_KEY_ID=testaccesskey -e AWS_SECRET_ACCESS_KEY=testsecretkey0123456789 -e RESTIC_PASSWORD=rpw \
    "$RESTIC_IMAGE" -r s3:http://s3:9000/restic init >/dev/null 2>&1 && break
  sleep 1
done
docker run --rm --network $NET -e AWS_ACCESS_KEY_ID=testaccesskey -e AWS_SECRET_ACCESS_KEY=testsecretkey0123456789 -e RESTIC_PASSWORD=rpw \
  -v "$WORK"/src:/src "$RESTIC_IMAGE" -r s3:http://s3:9000/restic backup /src --host testhost >/dev/null

write_config() { # $1 = sander's password
cat > "$WORK"/cfg/config.yaml <<EOF
server: {name: testgw, log_level: 1}
s3: {endpoint: "http://s3:9000", access_key_id: testaccesskey, secret_access_key: testsecretkey0123456789}
notifications: {enabled: true, manage: false, token: $TOKEN}
cache: {max_size: 1G, min_free_space: 10M, write_back: 2s, debounce: 200ms}
mounts:
  - {path: /media, type: s3, bucket: media, transport: $TRANSPORT}
  - {path: /photos, type: s3, bucket: photos, read_only: true, transport: $TRANSPORT}
  - {path: /backups/restic, type: restic, repository: "s3:http://s3:9000/restic", password: rpw}
  - path: /syncthing
    type: syncthing
    transport: $TRANSPORT
    sftp: {host: sftp, user: user, password: pass, path: upload}
    syncthing: {url: "http://syncthing:8384", api_key: test-api-key, data_root: /data}
users:
  - {name: sander, password: "$1"}
  - {name: reader, password: readerpw}
groups: {everyone: [sander, reader]}
shares:
  - {name: files, path: /, read: ["@everyone"], write: [sander]}
  - {name: media, path: /media, read: [reader], write: [sander]}
EOF
}
write_config sanderpw

docker run -d --name gw --network $NET --privileged --device /dev/fuse \
  -v "$WORK"/cfg:/etc/smb-gateway -v "$WORK"/cache:/cache "$IMAGE" run --debug >/dev/null

for i in $(seq 1 60); do
  docker exec gw smb-gateway health >/dev/null 2>&1 && break
  [ "$(docker inspect -f '{{.State.Running}}' gw)" = true ] || fail "gateway exited"
  sleep 1
done
docker exec gw smb-gateway health >/dev/null || fail "gateway never became healthy"
docker exec gw grep -E " /srv/smb/(media|syncthing) " /proc/mounts
pass "gateway healthy ($TRANSPORT)"

# --- reads ---------------------------------------------------------------
out=$(smb sander sanderpw files 'get media/existing.txt /dev/stdout')
grep -q "hello from s3" <<<"$out" || fail "read s3 file: $out"
pass "read file from s3 mount"

out=$(smb reader readerpw files 'ls backups/restic/hosts/testhost/latest/src/*')
grep -q backed-up.txt <<<"$out" || fail "restic listing: $out"
out=$(smb reader readerpw files 'get backups/restic/hosts/testhost/latest/src/backed-up.txt /dev/stdout')
grep -q "backed up file" <<<"$out" || fail "restic read: $out"
pass "read file from restic snapshot"

out=$(smb sander sanderpw files 'get syncthing/docs/readme.txt /dev/stdout')
grep -q "synced doc" <<<"$out" || fail "syncthing read: $out"
pass "read file from syncthing (sftp)"

# --- writes and permissions -------------------------------------------------
echo "written over smb" > "$WORK"/src/up.txt
smb sander sanderpw media 'put /src/up.txt shows/up.txt' >/dev/null
for i in $(seq 1 30); do [ -f "$WORK"/s3/media/shows/up.txt ] && break; sleep 1; done
grep -q "written over smb" "$WORK"/s3/media/shows/up.txt 2>/dev/null || fail "write did not reach the bucket"
pass "write via smb uploaded to s3"

out=$(smb reader readerpw media 'put /src/up.txt denied.txt' || true)
grep -q "NT_STATUS_ACCESS_DENIED" <<<"$out" || fail "reader could write: $out"
pass "read-only user cannot write"

out=$(smb sander sanderpw files 'put /src/up.txt photos/nope.txt' || true)
[ ! -f "$WORK"/s3/photos/nope.txt ] && grep -q NT_STATUS <<<"$out" || fail "write to read-only mount: $out"
pass "read-only mount rejects writes"

out=$(smb sander sanderpw files 'put /src/up.txt rootfile.txt' || true)
grep -q NT_STATUS <<<"$out" || fail "write outside mounts allowed: $out"
pass "tree root outside mounts is read-only"

out=$(smb sander wrongpw files 'ls' || true)
grep -q NT_STATUS_LOGON_FAILURE <<<"$out" || fail "bad password accepted: $out"
pass "wrong password rejected"

echo "to syncthing" > "$WORK"/src/st.txt
smb sander sanderpw files 'put /src/st.txt syncthing/docs/from-smb.txt' >/dev/null
for i in $(seq 1 30); do [ -f "$WORK"/st/docs/from-smb.txt ] && break; sleep 1; done
[ -f "$WORK"/st/docs/from-smb.txt ] || fail "write to syncthing did not land"
pass "write via smb landed in syncthing's folder"

# --- change-feed driven invalidation ----------------------------------------
# Prime the caches, then change the bucket behind the gateway's back.
smb sander sanderpw files 'ls media/shows/s01/*' >/dev/null
echo e2 > "$WORK"/s3/media/shows/s01/e02.txt
echo e3 > "$WORK"/s3/media/shows/s01/e03.txt
sleep 3
out=$(smb sander sanderpw files 'ls media/shows/s01/*')
if grep -q e02.txt <<<"$out"; then fail "change visible without an event - caching is not in effect"; fi
pass "listing is cached (no event, no change visible)"

# RGW-format notification for e02 only: the whole folder listing refreshes.
client curl -fsS -X POST "http://gw:8090/rgw/$TOKEN" -H 'Content-Type: application/json' -d '{"Records":[{"eventSource":"ceph:s3","eventName":"ObjectCreated:Put","s3":{"bucket":{"name":"media"},"object":{"key":"shows/s01/e02.txt","size":3}}}]}' >/dev/null
ok=
for i in $(seq 1 15); do
  out=$(smb sander sanderpw files 'ls media/shows/s01/*')
  if grep -q e02.txt <<<"$out" && grep -q e03.txt <<<"$out"; then ok=1; break; fi
  sleep 1
done
[ -n "$ok" ] || fail "change not visible after RGW event: $out"
pass "RGW event made new files visible"

# A new folder two levels deep (refresh walks up to the nearest known dir).
mkdir -p "$WORK"/s3/media/new-show/s01 && echo x > "$WORK"/s3/media/new-show/s01/e01.txt
s3_settle
client curl -fsS -X POST "http://gw:8090/rgw/$TOKEN" -d '{"Records":[{"eventSource":"ceph:s3","eventName":"ObjectCreated:Put","s3":{"bucket":{"name":"media"},"object":{"key":"new-show/s01/e01.txt","size":2}}}]}' >/dev/null
ok=
for i in $(seq 1 15); do
  out=$(smb sander sanderpw files 'ls media/*')
  grep -q new-show <<<"$out" && { ok=1; break; }
  sleep 1
done
[ -n "$ok" ] || fail "new folder not visible: $out"
pass "RGW event for a new nested folder"

# Deletion.
rm "$WORK"/s3/media/shows/s01/e03.txt
s3_settle
client curl -fsS -X POST "http://gw:8090/rgw/$TOKEN" -d '{"Records":[{"eventSource":"ceph:s3","eventName":"ObjectRemoved:Delete","s3":{"bucket":{"name":"media"},"object":{"key":"shows/s01/e03.txt"}}}]}' >/dev/null
ok=
for i in $(seq 1 15); do
  out=$(smb sander sanderpw files 'ls media/shows/s01/*')
  grep -q e03.txt <<<"$out" || { ok=1; break; }
  sleep 1
done
[ -n "$ok" ] || fail "deleted file still listed: $out"
pass "RGW delete event"

# Overwrite with different content (size changes).
smb sander sanderpw files 'get media/existing.txt /dev/null' >/dev/null
echo "hello from s3, but changed" > "$WORK"/s3/media/existing.txt
s3_settle
client curl -fsS -X POST "http://gw:8090/rgw/$TOKEN" -d '{"Records":[{"eventSource":"ceph:s3","eventName":"ObjectCreated:Put","s3":{"bucket":{"name":"media"},"object":{"key":"existing.txt","size":27}}}]}' >/dev/null
ok=
for i in $(seq 1 15); do
  out=$(smb sander sanderpw files 'get media/existing.txt /dev/stdout')
  grep -q "but changed" <<<"$out" && { ok=1; break; }
  sleep 1
done
[ -n "$ok" ] || fail "overwritten file still served stale: $out"
pass "overwritten object served fresh after event"

code=$(client curl -s -o /dev/null -w '%{http_code}' -X POST http://gw:8090/rgw/wrong-token -d '{}')
[ "$code" = 403 ] || fail "bad token accepted ($code)"
pass "event endpoint rejects wrong token"

# Syncthing event.
smb sander sanderpw files 'ls syncthing/docs/*' >/dev/null
echo new > "$WORK"/st/docs/pulled-from-peer.txt
sleep 2
out=$(smb sander sanderpw files 'ls syncthing/docs/*')
if grep -q pulled-from-peer <<<"$out"; then fail "syncthing change visible without an event"; fi
client curl -fsS -X POST http://syncthing:8384/test/emit -d '{"type":"ItemFinished","data":{"folder":"docs","item":"pulled-from-peer.txt","action":"update","type":"file"}}' >/dev/null
ok=
for i in $(seq 1 20); do
  out=$(smb sander sanderpw files 'ls syncthing/docs/*')
  grep -q pulled-from-peer <<<"$out" && { ok=1; break; }
  sleep 1
done
[ -n "$ok" ] || fail "syncthing event did not refresh: $out"
pass "Syncthing event made a pulled file visible"

# --- rclone crash recovery ---------------------------------------------------
docker exec gw pkill -KILL -f "rclone (serve nfs|mount) m_media" || true
sleep 1
ok=
for i in $(seq 1 30); do
  out=$(smb sander sanderpw files 'get media/existing.txt /dev/stdout' || true)
  grep -q "but changed" <<<"$out" && { ok=1; break; }
  sleep 1
done
[ -n "$ok" ] || fail "mount did not recover after rclone was killed: $out"
pass "mount recovers after rclone is killed"

# --- live config reload ---------------------------------------------------------
write_config newpass
ok=
for i in $(seq 1 60); do
  smb sander newpass files 'ls' >/dev/null 2>&1 && { ok=1; break; }
  sleep 1
done
[ -n "$ok" ] || fail "password change not applied"
out=$(smb sander sanderpw files 'ls' || true)
grep -q NT_STATUS_LOGON_FAILURE <<<"$out" || fail "old password still works"
[ "$(docker inspect -f '{{.RestartCount}}' gw)" = 0 ] || fail "container restarted for a user change"
pass "password change applied live"

echo "ALL TESTS PASSED ($TRANSPORT)"
