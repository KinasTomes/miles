#!/usr/bin/env python3
"""Build/stage both deployment profiles and test the real TLS proxy, without AWS."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile

from local_test import fixture_environment, runtime_fixture


ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True, help="New evidence directory outside the checkout")
    args = parser.parse_args()
    out = args.out.resolve()
    if out.is_relative_to(ROOT):
        parser.error("--out must be outside the checkout")
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    archive = out / "release.tar.gz"
    result = subprocess.run(["python3", "-B", "deploy/build-release.py", "--out", str(archive)],
                            cwd=ROOT, check=True, capture_output=True, text=True)
    release = json.loads(result.stdout)
    bundle = out / "bundle"
    with tarfile.open(archive, "r:gz") as tar:
        tar.extractall(bundle, filter="data")
    manifest = json.loads((bundle / "manifest.json").read_text())
    binaries = []
    env = fixture_environment(out)
    for profile, address in [("cloud", "10.0.1.20"), ("edge", "127.0.0.1")]:
        stage = out / profile
        subprocess.run(["python3", "-B", str(bundle / "deploy/install.py"), "--bundle", str(bundle),
                        "--root", str(stage), "--profile", profile, "--address", address], check=True)
        startup = json.loads((stage / "etc/miles/startup.json").read_text())
        assert startup == {"profile": profile, "mode": "runtime", "health_address": address + ":9100",
                           "shutdown_seconds": 1800}, "unexpected deployment config"
        binary = stage / "opt/miles/current/bin/miles"
        binaries.append(hashlib.sha256(binary.read_bytes()).hexdigest())
        assert manifest["source_commit"] in subprocess.check_output([str(binary), "-version"], text=True)
        # Reuse the native process fixture: loopback ephemeral ports, 2s shutdown
        # budget, real health/metrics/SIGTERM. Never enables production readiness.
        runtime_fixture(binary, profile, "runtime", out, env)
    assert binaries[0] == binaries[1] == manifest["files"]["bin/miles"]
    proxy_env = dict(os.environ, MILES_EDGE_PROXY_TEST="1", MILES_EDGE_PROXY_SLOW_TEST="1")
    with (out / "tls-proxy.log").open("w") as log:
        subprocess.run(["go", "test", "-v", "-race", "-count=1", "-timeout=3m", "./deploy/edge"],
                       cwd=ROOT, env=proxy_env, check=True, stdout=log, stderr=subprocess.STDOUT)
    summary = {**release, "same_binary": True, "profiles": ["cloud", "edge"],
               "health_status": 200, "ready_status": 503, "tls_proxy_slow_response_seconds": 65,
               "aws_operations": 0, "scope": "packaging/process/proxy contracts; not live deployment or R1/R2"}
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
