# restic + kubectl + rclone, baked in rather than apk-installed at every
# CronJob run (see cluster/prod/platform/syncthing-backups-restic/
# backup-cron.yaml and backblaze-copy-cron.yaml for what actually uses this).
#
# kubectl execs into each app's own postgres/mariadb pod for a logical dump
# over its trusted local socket - this image never carries or sees any app's
# DB credentials itself.
#
# rclone backs restic's `rclone:` backend for the Backblaze copy job: restic's
# native s3 backend only reads one global AWS_ACCESS_KEY_ID/SECRET pair, so a
# single `restic copy` process can't hold separate credentials for the local
# Ceph RGW repo and the Backblaze repo at the same time (confirmed against
# restic's own docs - restic/restic#16, open, low priority). Routing the
# Backblaze side through rclone sidesteps this: rclone takes its own
# credentials via RCLONE_CONFIG_*_ACCOUNT/KEY env vars, so the two sides never
# share a credential namespace.
FROM restic/restic:0.19.1@sha256:136600b6ff6843d61d355f7f71f460a166429f35de6fd11b568fece3c9a4d510 AS restic

FROM alpine:3.24
RUN apk add --no-cache ca-certificates kubectl rclone
COPY --from=restic /usr/bin/restic /usr/bin/restic
ENTRYPOINT ["/bin/sh"]
