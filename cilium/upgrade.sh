#!/usr/bin/env bash
# Upgrade (or roll back) the Cilium Helm release in kube-system.
# Run from the repo root with the ADMIN kubeconfig (not the read-only one).
# See cilium/README.md for the full runbook.
#
#   cilium/upgrade.sh check      read-only: versions, values drift, health, diff
#   cilium/upgrade.sh preflight  pre-pull the new images on every node
#   cilium/upgrade.sh apply      helm upgrade to $CILIUM_VERSION (release.env)
#   cilium/upgrade.sh verify     read-only: rollouts, cilium status, versions
#   cilium/upgrade.sh rollback   helm rollback to the previous revision
set -euo pipefail

cd "$(dirname "$0")"
# shellcheck source=release.env
source ./release.env
NS=kube-system
RELEASE=cilium
CHART=cilium/cilium
VALUES=values.yaml
TARGET="$CILIUM_VERSION"

need() { command -v "$1" >/dev/null || { echo "missing: $1 (nix-shell has it)" >&2; exit 1; }; }
need kubectl; need helm; need cilium

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

running_version() {
  kubectl -n "$NS" get ds cilium \
    -o jsonpath='{.spec.template.spec.containers[0].image}' \
    | sed -E 's/.*:v([0-9.]+).*/\1/'
}

confirm() {
  read -r -p "$1 [type yes] " a
  [[ "$a" == "yes" ]] || { echo "aborted"; exit 1; }
}

repo() {
  helm repo add cilium https://helm.cilium.io >/dev/null 2>&1 || true
  helm repo update cilium >/dev/null
}

check() {
  say "Context"
  kubectl config current-context
  kubectl get nodes -o wide | awk '{print $1, $2, $5, $9}'

  repo
  CUR=$(running_version)
  say "Versions: running $CUR, target $TARGET"
  helm -n "$NS" history "$RELEASE" --max 3

  say "Values drift: live release values vs $VALUES (rendered with chart $CUR)"
  tmp=$(mktemp -d)
  helm -n "$NS" get values "$RELEASE" -o yaml > "$tmp/live-values.yaml"
  api=(--kube-version "$(kubectl version -o yaml | awk '/serverVersion/{f=1} f&&/gitVersion/{print $2; exit}')"
       --api-versions monitoring.coreos.com/v1 --api-versions monitoring.coreos.com/v1/ServiceMonitor)
  helm template "$RELEASE" "$CHART" --version "$CUR" -n "$NS" "${api[@]}" -f "$tmp/live-values.yaml" > "$tmp/live.yaml"
  helm template "$RELEASE" "$CHART" --version "$CUR" -n "$NS" "${api[@]}" -f "$VALUES" > "$tmp/repo.yaml"
  # Secrets/certs are generated randomly on every render; ignore them.
  if diff <(grep -vE '^\s+(ca\.crt|tls\.crt|tls\.key|ca\.key):' "$tmp/live.yaml") \
          <(grep -vE '^\s+(ca\.crt|tls\.crt|tls\.key|ca\.key):' "$tmp/repo.yaml") > "$tmp/drift.diff"; then
    echo "OK: $VALUES renders the same manifests as the live release values"
  else
    cat "$tmp/drift.diff"
    echo
    echo "DRIFT: $VALUES does not match the live release. Live values saved to $tmp/live-values.yaml."
    echo "Fix $VALUES (commit it) before running apply."
  fi

  say "Cilium health (before)"
  cilium status --wait --wait-duration 2m

  say "What the upgrade changes (kubectl diff, server-side dry run)"
  helm template "$RELEASE" "$CHART" --version "$TARGET" -n "$NS" "${api[@]}" -f "$VALUES" \
    | kubectl diff -f - || true
  echo "(The Job hubble-generate-certs may show as immutable; that is expected, Helm replaces it.)"
}

preflight() {
  repo
  say "Pre-pulling cilium $TARGET images on every node (cilium-pre-flight-check)"
  helm install cilium-preflight "$CHART" --version "$TARGET" -n "$NS" \
    --set preflight.enabled=true --set agent=false --set operator.enabled=false
  echo "Waiting for the pre-flight DaemonSet and Deployment (up to 10 min)..."
  kubectl -n "$NS" rollout status ds/cilium-pre-flight-check --timeout 10m
  kubectl -n "$NS" rollout status deploy/cilium-pre-flight-check --timeout 5m
  kubectl -n "$NS" get ds cilium cilium-pre-flight-check
  echo "Ready count of cilium-pre-flight-check must equal cilium's. Removing pre-flight..."
  helm uninstall cilium-preflight -n "$NS"
}

apply() {
  repo
  CUR=$(running_version)
  [[ "${CUR%.*}" == "${TARGET%.*}" ]] || {
    echo "Refusing: $CUR -> $TARGET is a minor-version jump. Read the upgrade notes,"
    echo "go one minor at a time, and run 'cilium status' between steps."
    [[ "${FORCE_MINOR:-}" == 1 ]] || exit 1
  }
  if kubectl -n "$NS" get ds cilium-pre-flight-check >/dev/null 2>&1; then
    echo "Pre-flight is still installed; run 'preflight' to completion first." >&2; exit 1
  fi
  say "helm upgrade $RELEASE $CUR -> $TARGET"
  confirm "Upgrade Cilium on $(kubectl config current-context)?"
  helm upgrade "$RELEASE" "$CHART" --version "$TARGET" -n "$NS" -f "$VALUES" \
    --wait --timeout 20m
  verify
}

verify() {
  say "Rollouts"
  kubectl -n "$NS" rollout status ds/cilium --timeout 15m
  kubectl -n "$NS" rollout status ds/cilium-envoy --timeout 10m
  for d in cilium-operator hubble-relay hubble-ui; do
    kubectl -n "$NS" rollout status "deploy/$d" --timeout 5m
  done
  say "Cilium health (after)"
  cilium status --wait --wait-duration 5m
  say "Images now running"
  kubectl -n "$NS" get ds,deploy -o custom-columns='NAME:.metadata.name,IMAGE:.spec.template.spec.containers[*].image' \
    | grep -E 'cilium|hubble'
  say "Pods not Running/Completed anywhere (should be empty)"
  kubectl get pods -A --no-headers | awk '$4!="Running" && $4!="Completed"' || true
}

rollback() {
  helm -n "$NS" history "$RELEASE" --max 5
  read -r -p "Revision to roll back to (blank = previous): " rev
  confirm "Roll back Cilium${rev:+ to revision $rev}?"
  helm rollback "$RELEASE" ${rev:+"$rev"} -n "$NS" --wait --timeout 20m
  verify
}

case "${1:-}" in
  check|preflight|apply|verify|rollback) "$1" ;;
  *) sed -n '2,11p' "$0"; exit 1 ;;
esac
