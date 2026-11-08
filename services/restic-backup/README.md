# restic-backup

`restic` + `kubectl` + `rclone` + `sqlite` baked into one image, used by
[`cluster/prod/platform/syncthing-backups-restic/backup-cron.yaml`](../../cluster/prod/platform/syncthing-backups-restic/backup-cron.yaml),
`backblaze-copy-cron.yaml`, and `file-backup-cron.yaml`. The backup job mounts Syncthing's data
and also execs into several other apps' own postgres/mariadb pods (plus Vaultwarden's own
built-in `vaultwarden backup` command) to dump their databases over a trusted local
socket/in-process call - it needs `kubectl` for that `exec`, and previously ran
`apk add --no-cache kubectl` on every 15-minute invocation instead of baking it in once. The copy
job needs `rclone` to talk to Backblaze through restic's `rclone:` backend rather than its native
`s3:` backend - restic's s3 backend only reads one global
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` pair, so a single `restic copy` process can't hold
separate credentials for the local Ceph RGW repo and the Backblaze repo at once
(restic/restic#16, open/low-priority upstream). Routing one side through rclone sidesteps that,
since rclone's own credentials come through separate `RCLONE_CONFIG_*_ACCOUNT`/`KEY` env vars.
The file-backup job needs `sqlite` (the `sqlite3` CLI) for Actual's reader pod - see the
Dockerfile's own comment for why, and `backup-cron.yaml`/`file-backup-cron.yaml`'s comments for
why Vaultwarden doesn't need it (built-in backup command, no clone involved).

Image: `harbor.k8s.lysakermoen.com/library/restic-backup`, built and pushed by hand (see
Releasing below) - same convention as `services/smb-gateway`.

## Releasing

Cluster nodes are `amd64` - build for that explicitly, not whatever your workstation's arch is:

```sh
docker buildx build --platform linux/amd64 \
  -t harbor.k8s.lysakermoen.com/library/restic-backup:vX.Y.Z \
  services/restic-backup --load
docker push harbor.k8s.lysakermoen.com/library/restic-backup:vX.Y.Z
```

then update the tag in `cluster/prod/platform/syncthing-backups-restic/backup-cron.yaml`,
`backblaze-copy-cron.yaml`, and `file-backup-cron.yaml`.

The `syncthing` namespace needs its own `harbor-pull` secret
(`harbor-pull-secret.enc.yaml`, same content as
`cluster/prod/apps/nginx-custom-image/secret.enc.yaml`), applied by hand like every other secret
here. It also needs `backblaze-secret.enc.yaml` (B2 keyID/applicationKey + the Backblaze repo's
own restic password) for the copy job. The `actual` namespace separately needs its own
`harbor-pull` secret (`actual-harbor-pull-secret.enc.yaml`) for `file-backup-cron.yaml`'s Actual
reader pod, which also pulls this image.
