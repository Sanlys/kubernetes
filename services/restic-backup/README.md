# restic-backup

`restic` + `kubectl` baked into one image, used by
[`cluster/prod/platform/syncthing-backups-restic/backup-cron.yaml`](../../cluster/prod/platform/syncthing-backups-restic/backup-cron.yaml).
That job mounts Syncthing's data and also execs into several other apps' own postgres/mariadb
pods to run a logical dump over their trusted local socket - it needs `kubectl` for that `exec`,
and previously ran `apk add --no-cache kubectl` on every 15-minute invocation instead of baking
it in once.

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

then update the tag in `cluster/prod/platform/syncthing-backups-restic/backup-cron.yaml`.

The `syncthing` namespace needs its own `harbor-pull` secret
(`harbor-pull-secret.enc.yaml`, same content as
`cluster/prod/apps/nginx-custom-image/secret.enc.yaml`), applied by hand like every other secret
here.
