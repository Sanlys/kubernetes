#!/usr/bin/env bash
# Writes a kubeconfig for the read-only cloud identity
# (cluster/prod/platform/cloud-readonly). Meant for a cloud environment's
# setup script; needs K8S_RO_TOKEN set as an environment secret.
#
# No certificate-authority on purpose: the cloud sandbox's egress proxy
# re-terminates TLS with its own CA (already in the system trust store) and
# verifies the real Let's Encrypt cert upstream. kubectl honours HTTPS_PROXY.
set -euo pipefail

: "${K8S_RO_TOKEN:?set K8S_RO_TOKEN (cloud-readonly ServiceAccount token)}"
SERVER="${K8S_RO_SERVER:-https://k8s-ro.lysakermoen.com}"
KUBECONFIG_OUT="${KUBECONFIG:-$HOME/.kube/config}"

mkdir -p "$(dirname "$KUBECONFIG_OUT")"
umask 077
cat > "$KUBECONFIG_OUT" <<KCFG
apiVersion: v1
kind: Config
clusters:
  - name: prod-ro
    cluster:
      server: ${SERVER}
users:
  - name: cloud-readonly
    user:
      token: ${K8S_RO_TOKEN}
contexts:
  - name: prod-ro
    context:
      cluster: prod-ro
      user: cloud-readonly
current-context: prod-ro
KCFG
echo "wrote $KUBECONFIG_OUT (context prod-ro -> $SERVER)"
