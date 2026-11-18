#!/usr/bin/env python3
"""Validate what Argo CD would apply, so Renovate automerge has a gate.

1. Plain manifests under cluster/prod are checked with kubeconform.
2. Every Argo Application/ApplicationSet that uses a Helm chart is rendered
   with `helm template` using the same values Argo uses (inline `values`,
   `valuesObject` and `$values/...` valueFiles), and the output is checked
   with kubeconform.

By default only Helm apps touched by the diff against BASE_REF are rendered
(their app.yaml, or any file in the app's directory such as values.yaml).
Pass --all to render every Helm app; a change to the validator itself
(scripts/ci/ or the workflow) also renders everything.

SOPS-encrypted files (*.enc.yaml) are never read. Needs helm and kubeconform
on PATH, and PyYAML.
"""
import argparse
import os
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
CLUSTER = ROOT / "cluster" / "prod"
# Matches talos/topf.yaml; used for chart .Capabilities checks.
KUBE_VERSION = os.environ.get("KUBE_VERSION", "1.37.1")
# CRD API groups the cluster has, so charts render their ServiceMonitors,
# Gateway routes and so on, the same way they do in Argo.
API_VERSIONS = [
    "monitoring.coreos.com/v1",
    "monitoring.coreos.com/v1/ServiceMonitor",
    "monitoring.coreos.com/v1/PodMonitor",
    "monitoring.coreos.com/v1/PrometheusRule",
    "gateway.networking.k8s.io/v1",
    "cert-manager.io/v1",
    "autoscaling.k8s.io/v1",
    "ceph.rook.io/v1",
    "postgresql.cnpg.io/v1",
]
KUBECONFORM = [
    "kubeconform",
    "-strict",
    "-summary",
    "-ignore-missing-schemas",
    "-schema-location", "default",
    "-schema-location",
    "https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/"
    "{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json",
]


def run(cmd, **kw):
    print("+ " + " ".join(cmd[:6]) + (" ..." if len(cmd) > 6 else ""), flush=True)
    return subprocess.run(cmd, **kw)


def local_chart_dirs():
    return [p.parent for p in CLUSTER.rglob("Chart.yaml")]


def plain_manifests():
    charts = local_chart_dirs()
    out = []
    for p in sorted(CLUSTER.rglob("*.yaml")):
        name = p.name
        if name.endswith(".enc.yaml") or name in ("values.yaml", "kustomization.yaml"):
            continue
        if any(c in p.parents or p.parent == c for c in charts):
            continue
        out.append(str(p))
    return out


def changed_files(base):
    try:
        res = subprocess.run(
            ["git", "diff", "--name-only", f"{base}...HEAD"],
            cwd=ROOT, check=True, capture_output=True, text=True,
        )
    except subprocess.CalledProcessError as e:
        print(f"git diff against {base} failed ({e.stderr.strip()}); rendering all apps")
        return None
    return [ROOT / f for f in res.stdout.split()]


def helm_sources(app_file):
    """Yield (name, namespace, helm source dict) for each chart source."""
    for doc in yaml.safe_load_all(app_file.read_text()):
        if not isinstance(doc, dict):
            continue
        kind = doc.get("kind")
        spec = doc.get("spec") or {}
        if kind == "ApplicationSet":
            spec = (spec.get("template") or {}).get("spec") or {}
        elif kind != "Application":
            continue
        name = (doc.get("metadata") or {}).get("name", app_file.parent.name)
        ns = (spec.get("destination") or {}).get("namespace", "default")
        sources = spec.get("sources") or ([spec["source"]] if spec.get("source") else [])
        for src in sources:
            if src.get("chart"):
                yield name, ns, src
            elif src.get("path") and (ROOT / src["path"] / "Chart.yaml").exists() \
                    and "Sanlys/kubernetes" in src.get("repoURL", ""):
                yield name, ns, src


def render(app_file, name, ns, src, workdir):
    helm = src.get("helm") or {}
    release = helm.get("releaseName", name)
    if "{{" in release or "{{" in ns:
        # ApplicationSet template placeholders: use stable stand-ins.
        release = release.replace("{{", "").replace("}}", "")
        ns = ns.replace("{{", "").replace("}}", "")
    cmd = ["helm", "template", release]
    if src.get("chart"):
        repo = src["repoURL"]
        if repo.startswith("oci://"):
            cmd += [f"{repo.rstrip('/')}/{src['chart']}", "--version", str(src["targetRevision"])]
        else:
            cmd += [src["chart"], "--repo", repo, "--version", str(src["targetRevision"])]
    else:
        cmd += [str(ROOT / src["path"])]
    cmd += ["--namespace", ns, "--kube-version", KUBE_VERSION, "--include-crds"]
    for api in API_VERSIONS:
        cmd += ["--api-versions", api]
    for vf in helm.get("valueFiles") or []:
        if vf.startswith("$values/"):
            path = ROOT / vf[len("$values/"):]
        else:
            path = ROOT / src.get("path", "") / vf
        cmd += ["-f", str(path)]
    idx = 0
    for key in ("values", "valuesObject"):
        val = helm.get(key)
        if not val:
            continue
        f = Path(workdir) / f"{name}-{idx}.yaml"
        idx += 1
        f.write_text(val if isinstance(val, str) else yaml.safe_dump(val))
        cmd += ["-f", str(f)]
    for p in helm.get("parameters") or []:
        if "{{" not in str(p.get("value", "")):
            cmd += ["--set-string", f"{p['name']}={p['value']}"]
    res = run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        print(res.stderr, file=sys.stderr)
        return None
    out = Path(workdir) / f"{name}.rendered.yaml"
    out.write_text(res.stdout)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--all", action="store_true", help="render every Helm app")
    ap.add_argument("--base", default=os.environ.get("BASE_REF", "origin/main"))
    args = ap.parse_args()

    failed = []

    print("== Plain manifests", flush=True)
    files = plain_manifests()
    if run(KUBECONFORM + files).returncode != 0:
        failed.append("plain manifests")

    changed = None if args.all else changed_files(args.base)
    if changed is not None and any(
        f.is_relative_to(ROOT / "scripts" / "ci") or f.name == "validate.yaml" for f in changed
    ):
        print("Validator itself changed; rendering all Helm apps")
        changed = None
    apps = sorted(CLUSTER.rglob("app.yaml"))
    with tempfile.TemporaryDirectory() as workdir:
        for app in apps:
            if changed is not None and not any(
                f == app or app.parent in f.parents for f in changed
            ):
                continue
            for name, ns, src in helm_sources(app):
                label = f"{app.relative_to(ROOT)} ({src.get('chart') or src.get('path')})"
                print(f"== Helm: {label}", flush=True)
                out = render(app, name, ns, src, workdir)
                if out is None:
                    failed.append(f"render {label}")
                    continue
                if run(KUBECONFORM + [str(out)]).returncode != 0:
                    failed.append(f"validate {label}")

    if failed:
        print("\nFAILED:\n  " + "\n  ".join(failed), file=sys.stderr)
        return 1
    print("\nAll checks passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
