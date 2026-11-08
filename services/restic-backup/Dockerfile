# restic + kubectl, baked in rather than apk-installed at every CronJob run
# (see cluster/prod/platform/syncthing-backups-restic/backup-cron.yaml for
# what actually uses this). kubectl is needed to exec into each app's own
# postgres/mariadb pod for a logical dump over its trusted local socket -
# this image never carries or sees any app's DB credentials itself.
FROM restic/restic:0.19.1@sha256:136600b6ff6843d61d355f7f71f460a166429f35de6fd11b568fece3c9a4d510 AS restic

FROM alpine:3.24
RUN apk add --no-cache ca-certificates kubectl
COPY --from=restic /usr/bin/restic /usr/bin/restic
ENTRYPOINT ["/bin/sh"]
