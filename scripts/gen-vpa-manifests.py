#!/usr/bin/env python3
"""
Generate static VerticalPodAutoscaler manifests from the live cluster plus
scripts/vpa-tiers.json, one file per target workload under
cluster/prod/platform/vpa-policies/.

This is a one-shot generator, not something ArgoCD runs - output is committed,
reviewed-as-a-diff YAML, re-run by hand whenever vpa-tiers.json changes or a
workload is added/renamed (e.g. a Ceph OSD after a disk swap).

Only Deployment/StatefulSet/DaemonSet are ever queried, which is also what
keeps CNPG-managed Postgres pods (owned by a `Cluster` CRD) and KubeVirt VMs
(owned by a `VirtualMachineInstance`) out of scope without any special-casing -
neither is a Deployment/StatefulSet/DaemonSet to begin with.

Usage:
  gen-vpa-manifests.py                       dry-run, prints a summary table
  gen-vpa-manifests.py --wave 1              dry-run, restricted to wave 1's namespaces
  gen-vpa-manifests.py --wave 1 --write       writes the manifests for real
  gen-vpa-manifests.py --namespaces ns1,ns2 --write
"""
import argparse
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

REPO_DIR = Path(__file__).resolve().parent.parent
TIERS_FILE = Path(__file__).parent / "vpa-tiers.json"
OUTPUT_DIR = REPO_DIR / "cluster/prod/platform/vpa-policies"


def run_kubectl_json(args):
    r = subprocess.run(["kubectl"] + args + ["-o", "json"], capture_output=True, text=True, timeout=60)
    if r.returncode != 0:
        print(f"kubectl {' '.join(args)} failed: {r.stderr}", file=sys.stderr)
        sys.exit(1)
    return json.loads(r.stdout)


def load_tiers():
    return json.loads(TIERS_FILE.read_text())


def is_excluded(ns, name, cfg):
    for rule in cfg.get("exclude", []):
        if rule.get("namespace") and rule["namespace"] != ns:
            continue
        if rule.get("name") and rule["name"] != name:
            continue
        if rule.get("name_prefix") and not name.startswith(rule["name_prefix"]):
            continue
        if rule.get("_note"):
            # documentation-only entry (e.g. the cnpg pooler note) - never excludes anything
            continue
        if rule.get("namespace") or rule.get("name") or rule.get("name_prefix"):
            return True
    return False


def resolve_bounds(ns, name, cfg):
    """Returns (tier_name_or_None, min_dict, max_dict) for a workload, checking
    overrides top-to-bottom before falling back to namespace_defaults/default."""
    tiers = cfg["tiers"]
    for rule in cfg.get("overrides", []):
        if rule.get("namespace") and rule["namespace"] != ns:
            continue
        if rule.get("name") and rule["name"] != name:
            continue
        if rule.get("name_prefix") and not name.startswith(rule["name_prefix"]):
            continue
        if rule.get("name_suffix") and not name.endswith(rule["name_suffix"]):
            continue
        if not (rule.get("namespace") or rule.get("name") or rule.get("name_prefix") or rule.get("name_suffix")):
            continue
        if "tier" in rule:
            t = tiers[rule["tier"]]
            return rule["tier"], t["min"], t["max"]
        return "custom", rule["min"], rule["max"]

    tier_name = cfg.get("namespace_defaults", {}).get(ns, "default")
    t = tiers[tier_name]
    return tier_name, t["min"], t["max"]


def is_sparse(creation_timestamp, window_days):
    created = datetime.strptime(creation_timestamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    age_days = (datetime.now(timezone.utc) - created).total_seconds() / 86400
    return age_days < window_days


def resolve_update_mode(ns, name, cfg):
    """Per-workload updateMode, first matching rule in update_mode_overrides wins,
    else the global update_mode. Used to keep VPA from evicting/resizing workloads
    where an idle-based recommendation is unsafe (e.g. photoprism, whose postStart
    hook and indexing need far more memory than its idle footprint)."""
    for rule in cfg.get("update_mode_overrides", []):
        if rule.get("namespace") and rule["namespace"] != ns:
            continue
        if rule.get("name") and rule["name"] != name:
            continue
        if rule.get("namespace") or rule.get("name"):
            return rule["update_mode"]
    return cfg["update_mode"]


def build_vpa(ns, kind, name, containers, creation_timestamp, cfg):
    tier_name, tier_min, tier_max = resolve_bounds(ns, name, cfg)
    sparse = is_sparse(creation_timestamp, cfg["sparse_data_window_days"])
    sidecar_off = set(cfg.get("sidecar_off", []))

    container_policies = []
    for c in containers:
        cname = c["name"]
        if cname in sidecar_off:
            container_policies.append({"containerName": cname, "mode": "Off"})
            continue

        min_allowed = dict(tier_min)
        if sparse:
            live_req = c.get("resources", {}).get("requests")
            if live_req:
                min_allowed = live_req

        container_policies.append({
            "containerName": cname,
            "minAllowed": min_allowed,
            "maxAllowed": dict(tier_max),
            "controlledValues": "RequestsAndLimits",
        })

    return tier_name, sparse, {
        "apiVersion": "autoscaling.k8s.io/v1",
        "kind": "VerticalPodAutoscaler",
        "metadata": {
            "name": name,
            "namespace": ns,
            "labels": {"app.kubernetes.io/managed-by": "vpa-policy-generator"},
        },
        "spec": {
            "targetRef": {"apiVersion": "apps/v1", "kind": kind, "name": name},
            "updatePolicy": {"updateMode": resolve_update_mode(ns, name, cfg)},
            "resourcePolicy": {"containerPolicies": container_policies},
        },
    }


def to_yaml(obj, indent=0):
    """Minimal, deterministic YAML emitter - just enough for this manifest shape
    (nested dicts/lists of strings), so we don't need a PyYAML dependency."""
    pad = "  " * indent
    lines = []
    if isinstance(obj, dict):
        for k, v in obj.items():
            if isinstance(v, (dict, list)) and v:
                lines.append(f"{pad}{k}:")
                lines.append(to_yaml(v, indent + 1))
            else:
                lines.append(f"{pad}{k}: {format_scalar(v)}")
    elif isinstance(obj, list):
        for item in obj:
            if isinstance(item, dict):
                sub = to_yaml(item, indent + 1).split("\n")
                sub[0] = pad + "- " + sub[0].strip()
                lines.append("\n".join(sub))
            else:
                lines.append(f"{pad}- {format_scalar(item)}")
    return "\n".join(lines)


# YAML 1.1 (what most parsers, including the ones in this cluster, actually
# use) resolves these bare words to bool/null regardless of case - anything
# matching has to be quoted or it silently changes type on round-trip. This is
# exactly how a bare `mode: Off` for a VPA containerPolicy turned into the
# boolean `false` and got rejected by the admission webhook in production.
_YAML11_AMBIGUOUS = {
    "true", "false", "yes", "no", "on", "off", "y", "n",
    "null", "~", "none",
}


def format_scalar(v):
    if isinstance(v, str):
        if v.isdigit() or v.lower() in _YAML11_AMBIGUOUS:
            return f'"{v}"'
        return v
    return str(v)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--wave", type=int, help="restrict to namespaces listed under this wave in vpa-tiers.json")
    ap.add_argument("--namespaces", help="comma-separated namespace allowlist (overrides --wave)")
    ap.add_argument("--write", action="store_true", help="actually write manifest files (default: dry-run summary only)")
    args = ap.parse_args()

    cfg = load_tiers()

    allowed_ns = None
    if args.namespaces:
        allowed_ns = set(args.namespaces.split(","))
    elif args.wave is not None:
        allowed_ns = set(cfg["waves"][str(args.wave)])

    objs = run_kubectl_json(["get", "deployments,statefulsets,daemonsets", "-A"])["items"]

    written = []
    skipped_excluded = []
    skipped_wave = []

    for o in objs:
        ns = o["metadata"]["namespace"]
        name = o["metadata"]["name"]
        kind = o["kind"]
        creation_timestamp = o["metadata"]["creationTimestamp"]
        containers = o["spec"]["template"]["spec"]["containers"]

        if is_excluded(ns, name, cfg):
            skipped_excluded.append((ns, name))
            continue
        if allowed_ns is not None and ns not in allowed_ns:
            skipped_wave.append((ns, name))
            continue

        tier_name, sparse, manifest = build_vpa(ns, kind, name, containers, creation_timestamp, cfg)
        written.append((ns, kind, name, tier_name, sparse))

        if args.write:
            OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
            out_path = OUTPUT_DIR / f"{ns}--{kind.lower()}--{name}.yaml"
            out_path.write_text(to_yaml(manifest) + "\n")

    written.sort()
    print(f"{'NAMESPACE':<26} {'KIND':<12} {'NAME':<45} {'TIER':<22} SPARSE")
    for ns, kind, name, tier_name, sparse in written:
        print(f"{ns:<26} {kind:<12} {name:<45} {tier_name:<22} {'yes' if sparse else ''}")

    print(f"\n{len(written)} workload(s) {'written to ' + str(OUTPUT_DIR) if args.write else '(dry-run, nothing written - pass --write)'}")
    print(f"{len(skipped_excluded)} excluded by policy, {len(skipped_wave)} outside the selected wave/namespace filter")


if __name__ == "__main__":
    main()
