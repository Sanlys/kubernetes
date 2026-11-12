# cloud-readonly

Read-only kubectl access for scheduled cloud Claude sessions (the frequent
`scripts/health-sweep.py` run and the Sunday deep scan), so they stop
depending on a laptop kubeconfig and the LAN/VPN.

Two parts, deliberately separate:

| Part | Synced on merge? | What it does |
| --- | --- | --- |
| `./` (`app.yaml`) | yes | ServiceAccount, ClusterRole/Binding, monitoring proxy Role, token Secret. Nothing reachable from outside. |
| `./exposure` (`app.yaml.disabled`) | **no** | Public HTTPS endpoint `k8s-ro.lysakermoen.com` in front of the API server, served by the existing public traefik-public on 443 (no new port-forward or LoadBalancer). Turned on by hand, see below. |

## What the identity can and can't do

`clusterrole.yaml` has the full reasoning. In short: get/list/watch on every
core resource except Secrets, and on every listed API group. No Secrets, no
`pods/exec|attach|portforward|proxy`, no `nodes/proxy`, no token minting, no
writes of any kind. `monitoring-proxy-role.yaml` adds GET through the service
proxy to the Alertmanager and Prometheus APIs only.

Check it after the first sync:

```sh
SA=system:serviceaccount:cloud-readonly:cloud-readonly
kubectl auth can-i --list --as=$SA | head -50
kubectl auth can-i get secrets -A --as=$SA                       # no
kubectl auth can-i create pods/exec -n rook-ceph --as=$SA       # no
kubectl auth can-i get nodes/proxy --as=$SA                     # no
kubectl auth can-i get pods/log -A --as=$SA                     # yes
kubectl get --as=$SA --raw /api/v1/namespaces/monitoring/services/monitoring-kube-prometheus-alertmanager:9093/proxy/api/v2/alerts | head -c 200
```

(The two monitoring service names are derived from the `monitoring` release
name. If the last command 404s, `kubectl -n monitoring get svc` and fix the
names here, in `exposure/ingressroute.yaml` and in `health-sweep.py`.)

### Ceph

Ceph CLI access (`ceph status`, `ceph osd df`, ...) is **not** granted: it
would need `pods/exec` into `rook-ceph-tools`, which holds the admin keyring
(full write access to Ceph). The health sweep now reads
`CephCluster .status.ceph` instead, and the deep scan can use the `ceph_*`
metrics through the Prometheus proxy. If that turns out not to be enough, the
safer follow-up is a second toolbox (StatefulSet, so the pod name is stable for
`resourceNames`) with a `mon 'allow r' mgr 'allow r' osd 'allow r'` key, and
`pods/exec` on that one pod only. That's a Ceph change, so it's not in here.

## Why the endpoint looks like this

Cloud sessions reach the internet only as HTTPS through an egress proxy that
re-terminates TLS, and their source IPs aren't fixed. That rules out an IP
allowlist, client-certificate auth, a WireGuard/Tailscale tunnel (UDP) and an
frp `stcp` visitor. What does work is a normal HTTPS hostname with a public
cert plus a bearer token. A LoadBalancer straight onto the API server would
fail here, since its cert comes from the cluster CA and the egress proxy checks
upstream certs. So the already-public traefik-public terminates TLS with a
Let's Encrypt cert for one more hostname and does the filtering (comments in `exposure/ingressroute.yaml`):

- TLS terminated by Traefik, so the API server's own listener isn't on the internet
- GET only, Bearer header required (anonymous scanners get a 404 from Traefik)
- path denylist (secrets, exec/attach/portforward, `*/proxy` except the two
  monitoring APIs, KubeVirt console), which holds even for a leaked token of
  some other, more privileged ServiceAccount
- impersonation headers stripped, rate limited
- RBAC above as the real control

Residual risk worth knowing: any valid bearer token for this cluster (not only
this one) gets GET-only, denylisted access through this hostname. Pinning the
endpoint to this one token would need a ForwardAuth service; it's a possible
follow-up and isn't included here.

## Turning on the public endpoint (manual, Sander)

1. Merge, let `cloud-readonly` sync, run the `can-i` checks above.
2. DNS: `k8s-ro.lysakermoen.com` pointing wherever the other public
   `*.lysakermoen.com` hosts point (a wildcard record may already cover it).
3. Confirm traefik-public can read the `kube-root-ca.crt` ConfigMap in
   `default` (the ServersTransport uses it to verify the API server):
   `kubectl auth can-i get configmaps -n default --as=system:serviceaccount:traefik-public:traefik-public-traefik-public`
   (SA name may differ, check `kubectl -n traefik-public get sa`).
4. `git mv exposure/app.yaml.disabled exposure/app.yaml`, merge.
5. Smoke test from outside the LAN:
   - `curl -s -o /dev/null -w '%{http_code}\n' https://k8s-ro.lysakermoen.com/version` gives 404 (no token)
   - with the kubeconfig from step 6: `kubectl get nodes` works,
     `kubectl get secrets -A` fails, `kubectl -n rook-ceph exec deploy/rook-ceph-tools -- true` fails.
6. Copy the token into the cloud environment (project settings, Cloud
   environment):
   - env variable `token` (no underscores allowed there) =
     `kubectl -n cloud-readonly get secret cloud-readonly-token -o jsonpath='{.data.token}' | base64 -d`
   - network: Limited, with `k8s-ro.lysakermoen.com` and `dl.k8s.io` added
     to allowed domains
   - setup script: install kubectl, then `scripts/cloud-kubeconfig.sh`

Rotate: `kubectl -n cloud-readonly delete secret cloud-readonly-token`, then
update the environment's `token`. Emergency off: rename `exposure/app.yaml` back to
`.disabled` (Argo prunes the route), or delete the ServiceAccount.
