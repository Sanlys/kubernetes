# Homelab Kubernetes cluster (Talos + Argo CD)

GitOps repo for a home Kubernetes cluster running on Talos Linux. Argo CD's
`root` Application (`cluster/prod/app.yaml`) recursively discovers every
`**/app.yaml` under `cluster/prod/` and syncs it - there is no central list
to register a new app in. To add an app or platform component, create a new
directory with its own `app.yaml` plus manifests; it picks itself up.

## Before writing any YAML

- **Look at existing apps first.** This repo has no shared templates/Helm
  charts for its own apps - conventions live by example. Before building
  something new, find the closest existing app and match its shape:
  - Bucket-backed storage / rclone mounts → `cluster/prod/apps/immich/`
    (the canonical example) and `cluster/prod/apps/media/`
  - Sops-encrypted secrets → `cluster/prod/platform/frpc/`,
    `cluster/prod/apps/nginx-custom-image/`
  - Cross-namespace bucket sharing / RBAC → `cluster/prod/apps/syncthing/`
    + `cluster/prod/apps/immich/bucket-reader-rbac.yaml`
  - Plain stateful app with a PVC → `cluster/prod/apps/actual/`,
    `cluster/prod/apps/trilium/`, `cluster/prod/apps/vaultwarden/`
  - Multi-container pod sharing a network namespace (VPN sidecar) →
    `cluster/prod/apps/media/qbittorrent.yaml`
- **Ask before assuming.** Anything with real architectural weight -
  storage layout, how multiple apps will share state, network/VPN scope,
  what should be public vs internal, secret handling - should be confirmed
  with the user before committing to an approach, not guessed. This is a
  personal homelab with real tradeoffs (seeding vs deduplication, security
  vs convenience); the "obviously correct" design isn't always what the
  user actually wants. When in doubt, ask.
- **Don't assume a container image's base OS.** linuxserver.io images in
  particular are a mix of Alpine and Ubuntu bases depending on the app -
  don't hardcode `apt-get` (or `apk`) into a script without checking, or
  make the script detect which package manager is present at runtime.

## Namespaces

Default is one namespace per Argo CD Application (matching the app name).
It's fine to put multiple tightly-coupled apps in one namespace when they
form a genuine stack sharing state (e.g. `media`: qbittorrent + the arr
stack + jellyfin all sharing one bucket) - don't do this by default, only
when apps are actually coupled.

## Storage

- **Block storage (RWO only)**: `rook-ceph-block-ssd` / `rook-ceph-block-hdd`
  StorageClasses (Ceph RBD, `ReadWriteOnce`). There is currently no RWX
  filesystem StorageClass (no CephFS) - a PVC can only be mounted by
  containers within a single Pod, not shared live across separate Pods/nodes.
  Keep this in mind before designing anything that assumes multiple pods can
  share one persistent volume; either put those containers in one Pod, or
  don't design around cross-pod hardlinks/shared writes.
- **Object storage**: `rook-ceph-bucket` StorageClass via an
  `ObjectBucketClaim` (backed by Ceph RGW, S3-compatible, but treat it as
  "the cluster's object storage" rather than coupling to S3 specifics).
  Owning the OBC in the same namespace as its consumers is much simpler
  than cross-namespace access - the generated Secret/ConfigMap can be wired
  up directly via `envFrom`/`secretRef`/`configMapRef`, no RBAC needed.
  Only reach for the cross-namespace RBAC pattern (see
  `syncthing/rbac-immich-bucket-reader.yaml`) when actually reading another
  app's existing bucket.
- **rclone/FUSE mounts**: cross-container mount propagation does **not**
  work on this cluster's Talos nodes (would need `MountFlags=shared` on
  kubelet/containerd, which isn't set up) - a sidecar mounting a volume for
  another container to see doesn't work. Instead, mount in-process inside
  the container that needs it: copy a static rclone binary in via an
  initContainer (`rclone/rclone` image), then run `rclone mount` from a
  `postStart` lifecycle hook in the main container, `preStop` unmounts.
  Needs `securityContext.privileged: true` and a `/dev/fuse` hostPath
  volume. See `immich/server.yaml` for the original, most-commented version
  of this pattern.

## Security policies / privileged workloads

Two Kyverno `ClusterPolicy` resources (`disallow-privileged`,
`disallow-hostpath`, both currently `validationFailureAction: Audit`, not
enforcing) plus cluster-wide Pod Security Admission defaults restrict
privileged containers and hostPath volumes. Both are opted out of per
namespace via a `role: infra` label (the Kyverno escape hatch, same one
`rook-ceph` itself uses) plus explicit Pod Security Admission labels:

```yaml
labels:
  pod-security.kubernetes.io/enforce: "privileged"
  pod-security.kubernetes.io/enforce-version: "latest"
  pod-security.kubernetes.io/warn: "baseline"
  pod-security.kubernetes.io/audit: "baseline"
  role: infra
```

Use this only on namespaces that actually need it (FUSE mounts, VPN
sidecars needing `NET_ADMIN`/`/dev/net/tun`, etc.) - it's an explicit,
deliberate opt-out per namespace, not a default.

## Resource requests/limits

Convention (confirmed explicitly by the repo owner, see `actual/deployment.yaml`
and the `media` app): every container gets a **cpu request only, no cpu
limit** (Burstable), and **memory limit set equal to memory request** (no
burst headroom - request doubles as a hard cap). Follow this for new
containers unless told otherwise.

Caveat learned the hard way: since the memory request *is* the hard ceiling
with no slack, size it for what the container actually needs at runtime,
not just its steady-state idle usage - `apt-get`/`apk` package installs,
FUSE mount processes, and similar startup-time work all add to the peak,
and a too-low request causes real OOMKills (exit 137), not just throttling.
When in doubt, size generously; it's easier to right-size down later than
to debug a silent crash loop.

## Secrets (sops + age)

Any file ending in `secret.enc.yaml` (per `.sops.yaml`'s `path_regex`) gets
sops-encrypted with the repo's age keys - name new secret files to match
that suffix (e.g. `vpn-secret.enc.yaml` is fine; the literal k8s `Secret`
name inside can be anything). Two things are easy to miss:

- **Argo CD cannot decrypt sops files.** Any Application whose source
  directory contains a `*secret.enc.yaml` must explicitly exclude it in
  `directory.exclude` (see `frpc/app.yaml`,
  `nginx-custom-image/app.yaml`) or Argo CD will try to apply the raw
  ciphertext and fail.
- Plaintext secret files use the literal filename `secret.yaml` while
  being edited (already covered by the top-level `.gitignore`) - encrypt
  with `sops -e` to produce the committed `*secret.enc.yaml` before
  committing, never commit the plaintext version.
- Agents generally won't have the age keys to decrypt/create these -
  document the exact Secret name and keys a manifest expects in a comment,
  and let the user create/encrypt the actual secret out of band.

## Ingress

Two ingress classes, chosen per app based on whether it should be
internet-reachable:

- `public` + `cert-manager.io/cluster-issuer: letsencrypt-public`,
  hostnames on the bare domain (`*.lysakermoen.com`) - only for apps meant
  to be reachable from the internet.
- `internal` + `cert-manager.io/cluster-issuer: letsencrypt-internal`,
  hostnames under `*.k8s.lysakermoen.com` - default choice for anything
  with weak/no auth, admin tooling, or no reason to be internet-facing
  (download clients, `*arr` apps, etc.).

## Multi-container pods / VPN sidecars

Containers in a Pod share one network namespace already - a VPN sidecar
(e.g. gluetun) tunnels the whole Pod automatically, no explicit proxy
config needed on the other container(s). Two things that catch people out:

- **No start-order guarantee between containers in a Pod.** A script that
  assumes the VPN tunnel (or any other sidecar) is already up - e.g. a
  `postStart` hook doing package installs or network calls - can lose that
  race. Since a **failed `postStart` hook gets the whole container killed
  by kubelet** (not just logged), anything network-dependent in one should
  retry/tolerate the sidecar not being ready yet rather than fail outright.
- **A kill-switch firewall (e.g. gluetun's) blocks cluster-internal
  traffic too**, not just internet traffic, unless explicitly excepted
  (`FIREWALL_OUTBOUND_SUBNETS` or equivalent). If a container behind the
  VPN also needs to reach in-cluster services, add that exception. Pinning
  the exact pod/service CIDR isn't worth the fragility if you're not sure
  of the split - allowing the whole RFC1918 private range is just as safe
  for the firewall's actual purpose (stopping internet-bound leaks) and
  saves the trouble of finding cluster's exact CIDRs.
