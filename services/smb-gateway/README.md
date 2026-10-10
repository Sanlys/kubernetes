# smb-gateway

One SMB server for the cluster's storage:

- **S3 buckets** on Ceph RGW (read/write), via rclone with a persistent local cache
- **restic repositories** (read-only, browsable snapshots), via `restic mount`
- **Syncthing's data** (read/write), via the existing Syncthing pod's SFTP sidecar. The gateway
  is *not* another Syncthing node, so nothing is copied.

One sops-encrypted config file decides what is mounted where, who the users are, and who may
read or write which share ([`config.example.yaml`](config.example.yaml)).

Manifests: [`cluster/prod/apps/smb-gateway`](../../cluster/prod/apps/smb-gateway).
Image: `ghcr.io/sanlys/smb-gateway`, built by
[`.github/workflows/smb-gateway.yaml`](../../.github/workflows/smb-gateway.yaml).

## Design decisions

### NFS or FUSE under Samba

Samba needs a POSIX filesystem, so rclone has to present each remote either as a FUSE mount
(`rclone mount`) or as a loopback NFS server that the kernel mounts (`rclone serve nfs`).
Neither is better everywhere. I checked rclone 1.75 and go-nfs source, and this cluster's own
history, before choosing:

| | FUSE | loopback NFS |
|---|---|---|
| `open()` latency on these Talos nodes | tens of seconds on large files, measured (see `cluster/prod/apps/media/jellyfin.yaml`) | fine |
| rclone crashes / OOMs | mount dies; needs remount, open handles break | `hard` mount + on-disk handle cache: I/O pauses, then resumes on the restarted rclone |
| Kernel sees changes made by *others* | yes, attr/entry caches expire after 1s | **no, by default**: rclone reports a constant mtime for S3/SFTP directories, and the Linux NFS client only drops a cached listing when the directory's mtime changes |
| Knows when a client finished writing | yes (`release`) | no: every NFS WRITE is open+write+close; rclone uploads `write_back` after the last write |

The third row matters most. It also means the existing NFS-based mounts in this cluster
(jellyfin, immich, the *arr apps) probably never see new folder contents until the kernel
evicts the cached listing under memory pressure.

**Decision: NFS wherever there is a change feed, FUSE where there isn't.** With a feed, the
gateway fixes the NFS weakness directly. After rclone re-lists a changed directory, it sends
an NFS `SETATTR` (mtime = now) for that directory to rclone over a separate userspace NFS
connection, bypassing the kernel. On the next open the kernel sees a new mtime and re-reads
the listing. Doing this *through* the kernel mount wouldn't work, because the kernel would
update its own copy of the mtime and keep the stale listing. That leaves NFS with no
downside, and it keeps NFS's measured performance and crash behaviour. Without a feed, FUSE's
1s kernel caches are the only way to eventually see other writers, so mounts with
notifications turned off get FUSE. restic is always FUSE (`restic mount` has no other mode),
and it's read-only and immutable so that costs nothing. `transport:` per mount overrides the
choice.

### Change feeds and cache invalidation

- **S3**: the gateway creates a *persistent* RGW topic pushing to
  `smb-gateway-events:8090/rgw/<token>`, and adds a bucket notification
  (`s3:ObjectCreated:*`, `s3:ObjectRemoved:*`) to every mounted bucket. Existing notification
  configs on the bucket are preserved. Persistent means RGW queues and retries asynchronously.
  A non-persistent topic would make every S3 write in the cluster wait for this gateway.
- **Syncthing**: long-polls Syncthing's REST event API. `ItemFinished` covers files pulled
  from other devices; `LocalChangeDetected` covers files changed on disk.

Every event becomes a targeted refresh ([`internal/invalidate`](internal/invalidate/invalidate.go)):

1. Events are debounced (1s) and reduced to their parent directories, plus grandparents for
   deletions, because an S3 "folder" disappears with its last object.
2. A burst touching more than 64 directories is collapsed upwards: a few wider listings
   instead of thousands of narrow ones.
3. rclone re-lists each directory with `vfs/refresh`. This reuses existing nodes, which
   matters: `vfs/forget` would give the files new inode numbers, and the kernel NFS client
   answers that with ESTALE on open handles. If the directory doesn't exist (yet), the nearest
   existing ancestor is refreshed instead.
4. NFS mounts get the kernel kick described above.
5. Optional `prefetch_max_size`: small new files in a folder someone is currently browsing are
   read into the local cache ahead of time (e.g. new photos).

Changed file *contents* need no extra handling. The refresh updates size and mtime, the
kernel drops its page cache on the next attribute check (`actimeo=1`), and rclone's cache
fingerprint (`--vfs-fast-fingerprint`, size+mtime) discards the stale cached copy.

With a feed, `dir_cache_time` defaults to 1h (it only limits the damage of a lost event);
without one it's 1m.

### Other choices

- **Server modtimes on S3** (`--use-server-modtime`, unless `preserve_modtime: true`). Reading
  rclone's stored client mtime costs one HEAD request per file on every listing (checked in
  `backend/s3`). In Explorer, a folder of 2,000 photos would mean 2,000 HEADs.
- **Persistent cache on a PVC** (`rook-ceph-block-ssd`). Writes are acknowledged to SMB
  clients once they're in the cache, so they must survive a reschedule. A restart also starts
  warm.
- **`restic mount --no-lock`**. Otherwise the mount holds a repository lock for as long as it
  is mounted, and the weekly `restic-prune` CronJob would fail. Snapshots are immutable, and
  restic 0.19 reloads its index when the snapshot list changes (`internal/fuse/snapshots_dirstruct.go`),
  so new backups appear within a minute.
- **One unix identity for all file I/O.** rclone/restic have no per-user permissions, so
  access control is per share in Samba (`valid users`, `write list`). The mount tree outside
  the mounts is root-owned, so nothing can be created there.
- **Samba tuned for a network-backed filesystem**: no xattrs/ACLs/DOS attributes (they'd be
  silently dropped), no kernel oplocks or POSIX locks (Samba's own lock database is
  authoritative, since every client goes through this one server), SMB2 leases on. `.DS_Store`,
  `Thumbs.db`, Syncthing temp files and the like are vetoed.
- **NFS mount options**: `hard` (a soft mount can silently lose writes when rclone restarts),
  `nolock`, `lookupcache=positive` (never cache "doesn't exist"), 1 MiB r/wsize, mounted with
  mount(2) directly so the image needs no nfs-utils or rpcbind.

## Operating it

### Config

The config is the Secret `smb-gateway-config` (key `config.yaml`), stored sops-encrypted as
`cluster/prod/apps/smb-gateway/config-secret.enc.yaml` and applied by hand like the other
secrets (Argo excludes it):

```sh
cd cluster/prod/apps/smb-gateway
# first time: write it from the example
cat > config-secret.yaml <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: smb-gateway-config
  namespace: smb-gateway
stringData:
  config.yaml: |
$(sed 's/^/    /' ../../../../services/smb-gateway/config.example.yaml)
EOF
$EDITOR config-secret.yaml
sops -e config-secret.yaml > config-secret.enc.yaml && rm config-secret.yaml

# later edits
sops config-secret.enc.yaml
sops -d config-secret.enc.yaml | kubectl apply -f -
```

To validate before applying:
`sops -d config-secret.enc.yaml | yq '.stringData."config.yaml"' > /tmp/c.yaml && docker run --rm -v /tmp/c.yaml:/c.yaml ghcr.io/sanlys/smb-gateway:0.1.0 validate -config /c.yaml`
(`render` instead of `validate` prints the generated smb.conf and the transport chosen for
each mount).

**Live vs restart.** Kubelet updates the mounted Secret within about a minute. Changes to
`users`, `groups` and `shares` are applied live (`smbcontrol reload-config`). Anything else
(mounts, credentials, cache, notifications) makes the gateway exit cleanly so the container
restarts with the new config. An invalid new config is logged and ignored.

### Credentials it needs

- **S3**: a dedicated RGW user is cleanest. A system user can read/write every bucket and set
  their notifications:
  `kubectl -n rook-ceph exec deploy/rook-ceph-tools -- radosgw-admin user create --uid=smb-gateway --display-name=smb-gateway --system`.
  Per-mount `s3:` blocks can use a bucket's own OBC credentials instead.
- **restic**: `RESTIC_REPOSITORY` / `RESTIC_PASSWORD` from
  `cluster/prod/platform/syncthing-backups-restic/secret.enc.yaml`.
- **Syncthing**: the SFTP sidecar's user/password (`cluster/prod/apps/syncthing/deployment.yaml`),
  and the API key from Syncthing's GUI → Settings → API Key.
- `notifications.token`: anything random, e.g. `openssl rand -hex 24`.

### Connecting

`\\10.0.6.230\<share>` (Windows), `smb://10.0.6.230/<share>` (macOS/Linux). There is no
NetBIOS/WS-Discovery announcement, because multicast doesn't cross the LoadBalancer.

### Health

`kubectl -n smb-gateway exec deploy/smb-gateway -- wget -qO- 127.0.0.1:8090/healthz` shows
each mount's transport, health, restart count, change-feed counters (events, refreshes,
kicks, errors) and whether each bucket's RGW notification is set up.

### Releasing

Bump `VERSION` and the image tag in `cluster/prod/apps/smb-gateway/deployment.yaml` in the same
PR. CI publishes the version tag once on merge and never overwrites it.

### Removing it

The bucket notifications outlive the gateway. Delete them, or RGW keeps queueing events (up to
`topic_ttl`) for an endpoint that's gone:
`aws --endpoint-url $RGW s3api put-bucket-notification-configuration --bucket <b> --notification-configuration '{}'`
(this drops *all* notification configs on the bucket), then
`aws --endpoint-url $RGW sns delete-topic --topic-arn arn:aws:sns:<zonegroup>::<server.name>`.

## Known limits

- Explorer/Finder don't auto-refresh on remote changes. SMB change notifications come from
  inotify, which doesn't see changes made behind the mount, so press F5/⌘R. The *next*
  listing is fresh.
- Locks are only coordinated between SMB clients of this gateway, not with other S3 writers
  (S3 has no locks).
- No macOS resource forks/extended attributes (`fruit` needs xattrs). Finder copies still
  work; metadata like Finder tags is dropped.
- A file larger than the free cache space can't be written in one go (rclone stages writes in
  the cache).

## Development

```sh
go test ./...
docker build -t smb-gateway:dev .
TRANSPORT=fuse ./test/integration.sh smb-gateway:dev   # anywhere with /dev/fuse
TRANSPORT=nfs  ./test/integration.sh smb-gateway:dev   # needs the kernel NFS client (CI has it)
```

The integration test runs the real image against rclone `serve s3` (as the bucket store, so
the test can change things behind the gateway's back), a real restic repository, an SFTP
server and a fake Syncthing event API. It checks reads, writes, permissions, that listings
stay cached without an event and refresh with one (create, nested create, delete, overwrite,
Syncthing pull), recovery after rclone is killed, and live password changes.
