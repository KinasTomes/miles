#!/usr/bin/env python3
"""Fixed SSM diagnostic command: local metrics, OS pressure and Chrony only."""

import argparse
import base64
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.request

try:
    from scripts.edge_host_telemetry import HostTelemetryRecorder, preflight_check
except ModuleNotFoundError:
    from edge_host_telemetry import HostTelemetryRecorder, preflight_check

BASE = Path("/run/miles/benchmark-telemetry")
RELEASE = Path("/opt/miles/current")
MAX_BYTES = 8 << 20
CHUNK = 12000


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def fetch(url, method="GET", headers=None, limit=262144):
    request = urllib.request.Request(url, method=method, headers=headers or {})
    with urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect()).open(request, timeout=2) as response:
        body = response.read(limit+1)
    if len(body) > limit:
        raise ValueError("Diagnostic response exceeds bound")
    return body


def identity(source, verify_release=True):
    manifest = json.loads((RELEASE / "manifest.json").read_text())
    if manifest["source_commit"] != source:
        raise ValueError("Release source differs")
    names = ("bin/miles", "scripts/cloud_node_telemetry.py",
             "scripts/edge_host_telemetry.py", "scripts/runner_diagnostics.py") if verify_release else ()
    for name in names:
        if hashlib.sha256((RELEASE / name).read_bytes()).hexdigest() != manifest["files"][name]:
            raise ValueError("Release hash differs: "+name)
    token = fetch("http://169.254.169.254/latest/api/token", "PUT",
                  {"X-aws-ec2-metadata-token-ttl-seconds": "60"}).decode()
    document = json.loads(fetch("http://169.254.169.254/latest/dynamic/instance-identity/document",
                               headers={"X-aws-ec2-metadata-token": token}))
    startup = json.loads(Path("/etc/miles/startup.json").read_text())
    expected_health = "127.0.0.1:9100" if startup["profile"] == "edge" else document["privateIp"]+":9100"
    if (startup["profile"] not in {"cloud", "edge"} or startup["runtime_node_id"] != document["instanceId"]
            or startup["health_address"] != expected_health):
        raise ValueError("Cloud runtime does not match native EC2 identity")
    pid = subprocess.check_output(["systemctl", "show", "miles.service", "-p", "MainPID", "--value"],
                                  text=True, timeout=3).strip()
    if not pid.isdigit() or int(pid) < 2:
        raise ValueError("Gateway is not running")
    binary = (RELEASE / "bin/miles").stat()
    running = Path("/proc/"+pid+"/exe").stat()
    if (binary.st_dev, binary.st_ino) != (running.st_dev, running.st_ino):
        raise ValueError("Running executable differs from the release")
    return {"instance": document["instanceId"], "account": document["accountId"],
            "region": document["region"], "profile": startup["profile"], "address": startup["health_address"], "source": source,
            "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
            "binary_stat": [binary.st_dev, binary.st_ino, binary.st_size, binary.st_mtime_ns],
            "pid": pid, "process_start": Path("/proc/"+pid+"/stat").read_text().rsplit(")", 1)[1].split()[19]}


def arguments(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("action", choices=("run", "probe", "stop", "summary", "read"))
    p.add_argument("run_id")
    p.add_argument("slot")
    p.add_argument("segment")
    p.add_argument("source")
    p.add_argument("offset", type=int)
    a = p.parse_args(argv)
    if (not re.fullmatch(r"[a-z0-9-]{1,100}", a.run_id)
            or not re.fullmatch(r"[a-z0-9-]+-rep[1-3]", a.slot)
            or not re.fullmatch(r"[a-f0-9]{16}", a.segment)
            or not re.fullmatch(r"[a-f0-9]{40}", a.source)
            or not 0 <= a.offset <= MAX_BYTES):
        p.error("Invalid diagnostic binding")
    return a


def main(argv=None):
    os.umask(0o077)
    a = arguments(argv)
    folder = BASE / a.run_id / (a.slot+"-"+a.segment)
    observed = identity(a.source)
    if a.action == "run":
        folder.mkdir(parents=True, mode=0o700, exist_ok=False)
        (folder / "identity.json").write_text(json.dumps(observed))
        recorder = HostTelemetryRecorder(folder / "host.jsonl", max_bytes=MAX_BYTES)
        ok, errors = preflight_check(recorder.chrony_probe)
        if not ok:
            raise ValueError(str(errors))
        start = time.monotonic()
        metrics_file = folder / "metrics.jsonl"
        try:
            with metrics_file.open("x") as metrics:
                while True:
                    stopping = (folder / "stop").exists() or time.monotonic()-start >= 1200
                    if identity(a.source, verify_release=False) != observed:
                        raise ValueError("Gateway process/release changed during trial")
                    rec = recorder.sample_now()
                    raw = fetch("http://"+observed["address"]+"/metrics").decode()
                    if "miles_" not in raw:
                        raise ValueError("Missing Miles per-node metrics")
                    metrics.write(json.dumps({"sequence": rec["sequence"], "at": rec["timestamp_utc"],
                                              "text": raw})+"\n")
                    metrics.flush()
                    if (recorder.ring_buffer.dropped_records_count or recorder.ring_buffer.trimmed_bytes_total
                            or metrics_file.stat().st_size + recorder.output_file.stat().st_size > MAX_BYTES):
                        raise ValueError("Telemetry truncated or exceeds bound")
                    # Always acquire a sample starting after stop was observed;
                    # a sample already in flight cannot prove the window's tail.
                    if stopping:
                        break
                    time.sleep(2)
            payload = {"identity": observed, "records": recorder.ring_buffer.read_records(),
                       "metrics": [json.loads(s) for s in metrics_file.read_text().splitlines()]}
            raw = json.dumps(payload).encode()
            if len(raw) > MAX_BYTES:
                raise ValueError("Telemetry bundle exceeds bound")
            compressed = gzip.compress(raw, mtime=0)
            (folder / "samples.json.gz").write_bytes(compressed)
            summary = {"identity": observed, "bytes": len(compressed), "raw_bytes": len(raw),
                       "sha256": hashlib.sha256(compressed).hexdigest(), "samples": len(payload["records"])}
            (folder / "summary.json").write_text(json.dumps(summary))
        except BaseException as error:
            (folder / "failed.json").write_text(json.dumps({"error": str(error)}))
            raise
        print(json.dumps(summary))
        return
    expected = json.loads((folder / "identity.json").read_text())
    if expected != observed or (folder / "failed.json").exists():
        raise ValueError("Sampler identity changed or recorder failed")
    if a.action == "stop":
        (folder / "stop").touch(mode=0o600, exist_ok=True)
        print(json.dumps({"stop_requested": True, "identity": observed}))
    elif a.action == "probe":
        lines = (folder / "host.jsonl").read_text().splitlines()
        print(json.dumps({"identity": observed, "samples": len(lines),
                          "last": json.loads(lines[-1])["timestamp_utc"] if lines else None}))
    elif a.action == "summary":
        print((folder / "summary.json").read_text())
    else:
        with (folder / "samples.json.gz").open("rb") as handle:
            handle.seek(a.offset)
            raw = handle.read(CHUNK)
        print(json.dumps({"offset": a.offset, "data": base64.b64encode(raw).decode()}))


if __name__ == "__main__":
    main()
