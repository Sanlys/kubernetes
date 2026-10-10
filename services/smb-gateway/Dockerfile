# Base images are build args only so a local build can swap in mirrors;
# CI builds with the defaults.
ARG GO_IMAGE=golang:1.26-alpine
ARG ALPINE_IMAGE=alpine:3.24
ARG RCLONE_IMAGE=rclone/rclone:1.75.2
ARG RESTIC_IMAGE=restic/restic:0.19.1

FROM ${RCLONE_IMAGE} AS rclone
FROM ${RESTIC_IMAGE} AS restic

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
COPY . .
# RCLONE enables the test that runs the bundled rclone's NFS server.
RUN RCLONE=/usr/local/bin/rclone go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/smb-gateway ./cmd/smb-gateway

FROM ${ALPINE_IMAGE}
# samba-server: smbd; samba-common-tools: smbpasswd/pdbedit/smbcontrol/testparm
# fuse3: fusermount3 for rclone's FUSE mounts and restic mount
# procps: pkill on shutdown
RUN apk add --no-cache samba-server samba-common-tools fuse3 tini tzdata ca-certificates procps \
 && ln -sf fusermount3 /usr/bin/fusermount \
 && rm -f /etc/samba/smb.conf \
 && echo user_allow_other >> /etc/fuse.conf
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
COPY --from=restic /usr/bin/restic /usr/local/bin/restic
COPY --from=build /out/smb-gateway /usr/local/bin/smb-gateway
COPY config.example.yaml /usr/share/smb-gateway/config.example.yaml
EXPOSE 445 8090
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/smb-gateway"]
CMD ["run"]
