# Cilium (CNI)

Cilium runs as the Helm release `cilium` in `kube-system`. It was first
installed with `cilium-cli`, and is not managed by Argo CD: Argo needs pod
networking to run, so the CNI is upgraded by hand from this folder.

| File | What it is |
| --- | --- |
| `values.yaml` | Helm values for the release, rebuilt from the live cluster on 2026-11-12 |
| `release.env` | Chart version to run. Renovate opens PRs against it; merging does **not** upgrade the cluster |
| `upgrade.sh` | `check` / `preflight` / `apply` / `verify` / `rollback` |

Related Argo apps: `cluster/prod/platform/cilium-ingress` (Hubble UI route)
and `cluster/prod/platform/cilium-servicemonitor`.

## Upgrade runbook

Run everything from the repo root inside `nix-shell` (it provides `kubectl`,
`helm` and `cilium-cli`) with the **admin** kubeconfig.

Budget about 30 minutes. A patch upgrade restarts every Cilium agent (two
nodes at a time) and every cilium-envoy pod. Established connections keep
flowing while an agent restarts, because the eBPF datapath stays loaded, but
new pods on that node can't start networking until its agent is back.
Nothing in the cluster uses L7 (HTTP/FQDN) policy today, so the envoy
restart doesn't drop traffic.

1. **Check** (read-only):

   ```sh
   cilium/upgrade.sh check
   ```

   It must print `OK: values.yaml renders the same manifests as the live
   release values`. If it prints `DRIFT`, the repo file is missing something
   that's set on the live release: fix `values.yaml` first, or the upgrade
   would change those settings. Read the `kubectl diff` at the end. For
   1.18.0 → 1.18.16 the expected changes are new images, the Envoy admin
   socket getting `mode: 432` (0660, the CVE-2026-49445 fix), three new
   `cilium-config` keys (`enable-drift-checker`, `enable-dynamic-config`,
   `http-stream-idle-timeout`), an extra operator toleration, and a label on
   the agent ServiceMonitor.

2. **Pre-flight** (pre-pulls the new images on all 8 nodes, so no node
   waits on quay.io mid-rollout):

   ```sh
   cilium/upgrade.sh preflight
   ```

3. **Apply**:

   ```sh
   cilium/upgrade.sh apply
   ```

   This runs `helm upgrade -f values.yaml` with the version pinned in
   `release.env` (never `--reuse-values`, which skips new chart defaults),
   waits for every rollout, then runs `verify`. The script refuses minor
   jumps (1.18 → 1.19) unless `FORCE_MINOR=1` is set.

4. **Verify** (read-only, can be rerun any time):

   ```sh
   cilium/upgrade.sh verify
   ```

   Expect: all rollouts complete, `cilium status` all OK, every Cilium image
   on the new version, no pods stuck outside Running/Completed. Then open a
   couple of apps through both Traefiks and check Hubble UI at
   hubble.k8s.lysakermoen.com. For a deeper check, `cilium connectivity test`
   (about 10 minutes; it creates and deletes its own `cilium-test-*`
   namespace).

## Rollback

```sh
cilium/upgrade.sh rollback
```

Shows `helm history` and rolls back to the revision you choose (default: the
previous one), then runs `verify`. Rolling back restores the vulnerable
version, so treat it as a stop-gap and open an action item.

If agents are crashlooping and `helm` can't reach the API server, the API
server itself is fine (control-plane pods use host networking on Talos), so
`helm rollback` still works. A node whose agent won't start keeps its old
datapath; drain it before rebooting.

## Version policy

- Upgrade one minor at a time (1.18 → 1.19 → 1.20), always to the newest
  patch of the next minor, and read that minor's upgrade notes first.
- Cilium 1.18 is tested against Kubernetes 1.30–1.33, and this cluster runs
  1.36. Cilium 1.20 is the first release tested against 1.36.
- If 1.19 is ever used, it must be ≥ 1.19.5: 1.19.0–1.19.4 have
  CVE-2026-56743, which affects clusters with a custom `cluster.name`, as
  this one has (`kubernetes`).
