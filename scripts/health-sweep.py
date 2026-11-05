#!/usr/bin/env python3
"""
One-script recurring health sweep for this cluster + repo.

Design goal: run this every cycle and read only its stdout. Each check is
compared against the last run's state (stored locally) and only prints
when something is NEW or CHANGED - a clean cycle prints nothing at all.
When it does print something, that's the signal to drop into manual,
unscripted investigation (reading diffs/changelogs, WebSearch on a CVE,
etc.) rather than trusting this script's own text as the final word.

Checks:
  - Ceph health (transition-based: only reports when the summary changes)
  - ArgoCD app sync/health (per-app transition-based)
  - Pods stuck outside Running/Succeeded (reports newly-bad and newly-recovered)
  - Firing Alertmanager alerts, excluding Watchdog (reports every currently-active
    alert every cycle, tagged new/active, plus resolved counts - a persistent
    alert is never silently dropped after its first mention)
  - Open GitHub PRs on this repo (reports only PRs never seen before - merge/hold
    decisions still need a human/manual-Claude look at the real diff+changelog)
  - CVE watch (delegates to cve-watch.py, relays its output if not clean)

Exit code is always 0. Empty stdout == fully clean cycle.
"""
import json
import subprocess
from pathlib import Path

REPO_DIR = "/Users/sanderlysakermoen/Documents/git/kubernetes"
STATE_FILE = Path(__file__).parent / ".health-sweep-state.json"

# LAN gateway for the cluster's home network - used to tell "I'm not on this
# network right now" apart from "the cluster itself is actually down".
GATEWAY_IP = "10.0.6.1"
EXTERNAL_PROBE_IP = "1.1.1.1"


def load_state():
    if STATE_FILE.exists():
        try:
            return json.loads(STATE_FILE.read_text())
        except Exception:
            pass
    return {
        "conn_last": None,
        "ceph_last": None,
        "argocd_last": {},
        "pods_last": [],
        "alerts_last": [],
        "prs_seen": [],
    }


def save_state(state):
    STATE_FILE.write_text(json.dumps(state, indent=2))


def run(cmd, cwd=None, timeout=30):
    try:
        r = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout)
        return r.stdout.strip(), r.stderr.strip(), r.returncode
    except Exception as e:
        return "", str(e), 1


def check_connectivity(state, report):
    """Returns True if the cluster API is reachable. If not, figures out
    whether that's because this machine isn't on the cluster's network right
    now (common, routine, muted after the first report) or because the
    cluster itself is actually down (rare, serious, never muted)."""
    _, _, rc = run(["kubectl", "get", "--raw=/healthz", "--request-timeout=5s"], timeout=10)
    if rc == 0:
        if state.get("conn_last") not in (None, "ok"):
            report.append("CLUSTER: connectivity restored")
        state["conn_last"] = "ok"
        return True

    _, _, gw_rc = run(["ping", "-c", "1", "-W", "2000", GATEWAY_IP], timeout=5)
    _, _, ext_rc = run(["ping", "-c", "1", "-W", "2000", EXTERNAL_PROBE_IP], timeout=5)

    if gw_rc == 0:
        # On the cluster's own network, but the API server still didn't
        # answer - this is a real problem, not a "wrong network" situation.
        # Always report, every cycle, until it clears - never mute this one.
        report.append(
            "CLUSTER DOWN: on cluster network (gateway reachable) but API server "
            "unreachable - needs investigation, not just a connectivity blip"
        )
        state["conn_last"] = "cluster_down"
    else:
        detail = "internet OK, so just not on the cluster LAN" if ext_rc == 0 else "internet also unreachable"
        if state.get("conn_last") != "local":
            report.append(
                f"LOCAL: not on cluster network (gateway {GATEWAY_IP} unreachable, {detail}) "
                "- skipping cluster-side checks this cycle"
            )
        state["conn_last"] = "local"
    return False


def check_ceph(state, report):
    out, err, rc = run(["kubectl", "exec", "-n", "rook-ceph", "deploy/rook-ceph-tools", "--", "ceph", "health"])
    if rc != 0:
        report.append(f"! CEPH check failed: {err or out}")
        return
    current = out
    last = state.get("ceph_last")
    if current != last:
        if last is None:
            report.append(f"CEPH: {current}")
        else:
            report.append(f"CEPH changed: '{last}' -> '{current}'")
    state["ceph_last"] = current


def check_argocd(state, report):
    out, err, rc = run(["kubectl", "get", "application", "-n", "argocd", "-o", "json"])
    if rc != 0:
        report.append(f"! ARGOCD check failed: {err or out}")
        return
    try:
        apps = json.loads(out).get("items", [])
    except Exception as e:
        report.append(f"! ARGOCD parse failed: {e}")
        return
    last = state.get("argocd_last", {})
    current = {}
    for app in apps:
        name = app["metadata"]["name"]
        sync = app.get("status", {}).get("sync", {}).get("status", "Unknown")
        health = app.get("status", {}).get("health", {}).get("status", "Unknown")
        status = f"{sync}/{health}"
        current[name] = status
        healthy = status == "Synced/Healthy"
        prev = last.get(name)
        if not healthy and status != prev:
            report.append(f"ARGOCD: {name} -> {status}")
        elif healthy and prev and prev != status and prev != "Synced/Healthy":
            report.append(f"ARGOCD: {name} recovered ({prev} -> {status})")
    state["argocd_last"] = current


def check_pods(state, report):
    out, err, rc = run([
        "kubectl", "get", "pods", "-A",
        "--field-selector=status.phase!=Running,status.phase!=Succeeded",
        "-o", "json",
    ])
    if rc != 0:
        report.append(f"! PODS check failed: {err or out}")
        return
    try:
        items = json.loads(out).get("items", [])
    except Exception as e:
        report.append(f"! PODS parse failed: {e}")
        return
    current = set()
    details = {}
    for p in items:
        key = f"{p['metadata']['namespace']}/{p['metadata']['name']}"
        current.add(key)
        phase = p.get("status", {}).get("phase", "?")
        details[key] = phase
    last = set(state.get("pods_last", []))
    new_bad = current - last
    recovered = last - current
    for k in sorted(new_bad):
        report.append(f"POD: {k} is {details.get(k, '?')}")
    if recovered:
        report.append(f"POD: {len(recovered)} previously-stuck pod(s) recovered")
    state["pods_last"] = sorted(current)


def check_alerts(state, report):
    name_out, _, rc = run([
        "kubectl", "get", "pods", "-n", "monitoring",
        "-l", "app.kubernetes.io/name=alertmanager",
        "-o", "jsonpath={.items[0].metadata.name}",
    ])
    if rc != 0 or not name_out:
        report.append("! ALERTS check failed: could not find alertmanager pod")
        return
    out, err, rc = run([
        "kubectl", "exec", "-n", "monitoring", name_out, "-c", "alertmanager",
        "--", "wget", "-qO-", "http://localhost:9093/api/v2/alerts",
    ])
    if rc != 0:
        report.append(f"! ALERTS check failed: {err or out}")
        return
    try:
        alerts = json.loads(out)
    except Exception as e:
        report.append(f"! ALERTS parse failed: {e}")
        return
    current = {}
    for a in alerts:
        labels = a.get("labels", {})
        if labels.get("alertname") == "Watchdog":
            continue
        fp = a.get("fingerprint")
        if a.get("status", {}).get("state") != "active":
            continue
        # Workload-identifying labels take priority over "pod"/"job", which on
        # a metrics-derived alert (e.g. KubeDeploymentReplicasMismatch) belong
        # to the exporter (kube-state-metrics) itself, not the actual affected
        # workload. "pod" is only trustworthy when nothing more specific is
        # set (e.g. KubePodNotReady), and gets the namespace prefixed.
        ns = labels.get("namespace")
        pod_tag = f"{ns}/{labels['pod']}" if ns and labels.get("pod") else labels.get("pod")
        job_name_tag = f"{ns}/{labels['job_name']}" if ns and labels.get("job_name") else labels.get("job_name")
        tag = (
            labels.get("deployment") or labels.get("statefulset") or labels.get("daemonset")
            # job_name (e.g. KubeJobFailed) names the actual batch Job -
            # found via geoip-db-sync-29897640 showing up tagged as the
            # kube-state-metrics exporter's own pod instead, the same
            # mistagging class the deployment/statefulset/daemonset
            # priority above already fixed, just missing this one label.
            or job_name_tag
            or labels.get("node") or pod_tag or labels.get("instance") or ""
        )
        current[fp] = f"{labels.get('alertname')} ({tag})" if tag else labels.get("alertname", fp)
    last = {x.split("::", 1)[0] for x in state.get("alerts_last", [])}
    current_fps = set(current.keys())
    # Every currently-active alert is reported every cycle (never silently
    # dropped after its first mention) - a long-lived alert must stay
    # visible for as long as it's actually firing.
    for fp in sorted(current_fps, key=lambda fp: current[fp]):
        tag = "new" if fp not in last else "active"
        report.append(f"ALERT {tag}: {current[fp]}")
    resolved = last - current_fps
    if resolved:
        report.append(f"ALERT: {len(resolved)} previously-firing alert(s) resolved")
    state["alerts_last"] = [f"{fp}::{desc}" for fp, desc in current.items()]


def check_prs(state, report):
    out, err, rc = run(["gh", "pr", "list", "--state", "open", "--json", "number,title,url"], cwd=REPO_DIR)
    if rc != 0:
        report.append(f"! PR check failed: {err or out}")
        return
    try:
        prs = json.loads(out)
    except Exception as e:
        report.append(f"! PR parse failed: {e}")
        return
    seen = set(state.get("prs_seen", []))
    new_prs = [p for p in prs if p["number"] not in seen]
    for p in new_prs:
        report.append(f"PR #{p['number']}: {p['title']} - {p['url']}")
    state["prs_seen"] = sorted(seen | {p["number"] for p in prs})


def check_cve(report):
    out, err, rc = run(["python3", str(Path(__file__).parent / "cve-watch.py")], timeout=180)
    first_line = out.splitlines()[0] if out else ""
    if rc != 0:
        report.append(f"! CVE check failed: {err or out}")
    elif "clean" not in first_line:
        report.append(out)


def main():
    state = load_state()
    report = []
    reachable = check_connectivity(state, report)
    if reachable:
        check_ceph(state, report)
        check_argocd(state, report)
        check_pods(state, report)
        check_alerts(state, report)
    check_prs(state, report)  # independent of cluster reachability
    check_cve(report)  # independent of cluster reachability
    save_state(state)
    for line in report:
        print(line)


if __name__ == "__main__":
    main()
