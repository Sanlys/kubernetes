#!/usr/bin/env bash
# Checks and unit-tests the gateway's PrometheusRule with promtool.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
RULE=$HERE/../../../cluster/prod/apps/smb-gateway/prometheusrule.yaml
IMAGE=${PROMETHEUS_IMAGE:-prom/prometheus:v3.5.0}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
python3 -c 'import sys, yaml; d = yaml.safe_load(open(sys.argv[1])); yaml.safe_dump({"groups": d["spec"]["groups"]}, open(sys.argv[2], "w"))' "$RULE" "$TMP/rules.yaml"
cp "$HERE/alerts_test.yaml" "$TMP/"
# promtool runs as nobody in the image
chmod -R a+rX "$TMP"
docker run --rm --entrypoint promtool -v "$TMP":/w -w /w "$IMAGE" check rules rules.yaml
docker run --rm --entrypoint promtool -v "$TMP":/w -w /w "$IMAGE" test rules alerts_test.yaml
