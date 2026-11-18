#!/usr/bin/env python3
"""Validate what Argo CD would apply, so Renovate automerge has a gate.

1. Plain manifests under cluster/prod are checked with kubeconform.
2. Every Argo Application/ApplicationSet that uses a Helm chart is rendered
   with `helm template` using the same values Argo uses (inline `values`,
   `valuesObject` and `$values/...` valueFiles), and the output is checked
   with kubeconform.

Every Helm app is rendered on every run (about 15s), because CRDs found in
the plain manifests and the rendered charts are turned into schemas first,
so custom resources are checked against the CRD versions we actually ship.
The datreeio catalog is only a fallback for CRDs we don't have locally.

SOPS-encrypted files (*.enc.yaml) are never read. Needs helm and kubeconform
on PATH, and PyYAML.
"""
import argparse
import json
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
    # cwd=workdir: helm prefers a same-named local directory over --repo
    # (the CNPG chart is literally called "cluster").
    res = run(cmd, capture_output=True, text=True, cwd=workdir)
    if res.returncode != 0:
        print(res.stderr, file=sys.stderr)
        return None
    out = Path(workdir) / f"{name}.rendered.yaml"
    docs = [drop_nulls(d) for d in yaml.safe_load_all(res.stdout) if d]
    out.write_text(yaml.safe_dump_all(docs))
    return out


def drop_nulls(node):
    """The API server drops `key: null` on non-nullable fields before
    validating (charts like CNPG's render `initdb: null`), so do the same."""
    if isinstance(node, dict):
        return {k: drop_nulls(v) for k, v in node.items() if v is not None}
    if isinstance(node, list):
        return [drop_nulls(v) for v in node]
    return node


def to_json_schema(node):
    """OpenAPI v3 (CRD flavour) -> JSON schema, in the spirit of
    kubeconform's openapi2jsonschema.py with strict mode."""
    if isinstance(node, list):
        return [to_json_schema(n) for n in node]
    if not isinstance(node, dict):
        return node
    if node.get("x-kubernetes-int-or-string"):
        return {"oneOf": [{"type": "string"}, {"type": "integer"}]}
    out = {k: to_json_schema(v) for k, v in node.items() if not k.startswith("x-kubernetes-")}
    if node.get("x-kubernetes-preserve-unknown-fields"):
        out["additionalProperties"] = True
    elif "properties" in out and "additionalProperties" not in out:
        out["additionalProperties"] = False
    if out.pop("nullable", False) and "type" in out:
        out["type"] = [out["type"], "null"]
    return out


def write_crd_schemas(manifest_files, schema_dir):
    """Turn every CRD we deploy into kubeconform schemas, so custom resources
    are checked against the exact CRD version we ship rather than the
    community catalog (which lags, e.g. new CephCluster fields)."""
    count = 0
    for f in manifest_files:
        try:
            docs = list(yaml.safe_load_all(Path(f).read_text()))
        except yaml.YAMLError:
            continue
        for doc in docs:
            if not isinstance(doc, dict) or doc.get("kind") != "CustomResourceDefinition":
                continue
            spec = doc.get("spec") or {}
            group, kind = spec.get("group"), (spec.get("names") or {}).get("kind")
            if not group or not kind:
                continue
            for ver in spec.get("versions") or []:
                schema = ((ver.get("schema") or {}).get("openAPIV3Schema"))
                if not schema:
                    continue
                d = Path(schema_dir) / group
                d.mkdir(parents=True, exist_ok=True)
                (d / f"{kind.lower()}_{ver['name']}.json").write_text(
                    json.dumps(to_json_schema(schema)))
                count += 1
    print(f"Generated {count} CRD schemas from our own CRDs", flush=True)


def main():
    argparse.ArgumentParser(description=__doc__).parse_args()
    failed = []
    with tempfile.TemporaryDirectory() as workdir:
        rendered = []
        for app in sorted(CLUSTER.rglob("app.yaml")):
            for name, ns, src in helm_sources(app):
                label = f"{app.relative_to(ROOT)} ({src.get('chart') or src.get('path')})"
                print(f"== Render: {label}", flush=True)
                out = render(app, name, ns, src, workdir)
                if out is None:
                    failed.append(f"render {label}")
                else:
                    rendered.append((label, out))

        plain = plain_manifests()
        schema_dir = Path(workdir) / "schemas"
        write_crd_schemas(plain + [str(o) for _, o in rendered], schema_dir)
        kc = KUBECONFORM[:]
        kc[kc.index("default") + 1:kc.index("default") + 1] = [
            "-schema-location",
            str(schema_dir) + "/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json",
        ]

        print("== Validate: plain manifests", flush=True)
        if run(kc + plain).returncode != 0:
            failed.append("validate plain manifests")
        for label, out in rendered:
            print(f"== Validate: {label}", flush=True)
            if run(kc + [str(out)]).returncode != 0:
                failed.append(f"validate {label}")

    if failed:
        print("\nFAILED:\n  " + "\n  ".join(failed), file=sys.stderr)
        return 1
    print("\nAll checks passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
