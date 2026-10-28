#!/usr/bin/env python3
"""Paired protocol smoke: real Miles/Badger versus direct pinned Moto, no AWS.

Keeps private configs/runtime data beside artifacts; always stops its process and
removes only its disposable Docker project. This is not a performance baseline.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import subprocess
import time
import uuid

from local_test import ROOT, aws, command, fixture_environment, local_endpoint, response, wait_backend


def private_json(path, value):
    with open(path, "x", opener=lambda p, flags: os.open(p, flags, 0o600)) as f:
        json.dump(value, f, indent=2)
        f.write("\n")


def gateway_addresses(process, log):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError("gateway exited; inspect private gateway.log")
        addresses = {}
        for line in log.read_text().splitlines(keepends=True):
            if line.endswith("\n"):
                r = json.loads(line)
                addresses[r.get("msg")] = r.get("address")
        if addresses.get("health server listening") and addresses.get("s3 server listening"):
            health = local_endpoint("http://" + addresses["health server listening"])
            if response(health + "/readyz")[0] == 200:
                return local_endpoint("http://" + addresses["s3 server listening"])
        time.sleep(0.05)
    raise RuntimeError("gateway did not become ready; inspect private gateway.log")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--out", type=Path, required=True, help="new private directory with an existing parent")
    args = p.parse_args()
    args.out = args.out.resolve()
    args.out.mkdir(mode=0o700)
    env = fixture_environment(args.out)
    context = command(["docker", "context", "show"]).strip()
    host = json.loads(command(["docker", "context", "inspect", context]))[0]["Endpoints"]["docker"]["Host"]
    if os.environ.get("DOCKER_HOST") or not host.startswith("unix://"):
        raise RuntimeError("smoke requires a local Unix Docker context")
    binaries = args.out / "bin"
    binaries.mkdir(mode=0o700)
    for name in ("miles", "warmbench"):
        command(["go", "build", "-trimpath", "-o", str(binaries / name), "./cmd/" + name], env, timeout=120)
    command(["go", "run", "./cmd/datasetgen", "--profile", "smoke", "--seed", "miles-w02-v1", "--out", str(args.out / "dataset")], env)
    cpu = next((s.split(":", 1)[1].strip() for s in Path("/proc/cpuinfo").read_text().splitlines() if s.startswith("model name")), "unknown")
    environment = {
        "topology": "single development VM; physical S3 is pinned Moto, not AWS",
        "host": platform.system() + " " + platform.machine() + " development VM",
        "cpu": cpu + "; " + str(os.cpu_count()) + " logical CPUs",
        "disk": command(["stat", "-f", "-c", "%T", str(args.out)]).strip(),
        "network": "loopback Docker-published HTTP",
        "encryption": "none; emulator only",
        "async_work": "producer payload/outbox retained in Badger; no exporter/seal/checkpoint worker",
    }
    suffix = uuid.uuid4().hex[:12]
    compose = ["docker", "compose", "-f", "deploy/local/compose.yaml", "-p", "miles-warmbench-" + suffix]
    manifest = args.out / "dataset" / "manifest.json"
    summary = {"kind": "local-protocol-smoke-only", "gateway_sha256": hashlib.sha256((binaries / "miles").read_bytes()).hexdigest(),
               "aws_cli_version": command(["aws", "--version"], env).strip(),
               "docker_version": command(["docker", "--version"], env).strip(), "runs": []}
    try:
        command(compose + ["up", "-d", "s3"], env, timeout=300)
        endpoint = local_endpoint("http://" + command(compose + ["port", "s3", "5000"], env).strip())
        wait_backend(env, endpoint, "s3api")
        for concurrency in (1, 4):
            trial = args.out / ("concurrency-" + str(concurrency))
            trial.mkdir(mode=0o700)
            backend_bucket = "packs-" + suffix + "-" + str(concurrency)
            direct_bucket = "direct-" + suffix + "-" + str(concurrency)
            for bucket in (backend_bucket, direct_bucket):
                aws(env, endpoint, "s3api", "create-bucket", "--bucket", bucket, "--create-bucket-configuration", "LocationConstraint=ap-southeast-1")
            startup = {"profile": "edge", "mode": "local", "health_address": "127.0.0.1:0", "s3_address": "127.0.0.1:0", "shutdown_seconds": 10,
                       "local_data_path": str(trial / "runtime"), "pack_endpoint": endpoint, "pack_bucket": backend_bucket}
            startup_file = trial / "startup.json"
            private_json(startup_file, startup)
            log = trial / "gateway.log"
            with log.open("w") as output:
                process = subprocess.Popen([str(binaries / "miles"), "-config", str(startup_file)], cwd=ROOT, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=output)
                try:
                    gateway = gateway_addresses(process, log)
                    credential_file = trial / "runtime" / "credentials.json"
                    cred = json.loads(credential_file.read_text())
                    gateway_env = dict(env, AWS_ACCESS_KEY_ID=cred["access_key_id"], AWS_SECRET_ACCESS_KEY=cred["secret_access_key"])
                    aws(gateway_env, gateway, "s3api", "create-bucket", "--bucket", "warm-benchmark")
                    direct_cred = trial / "direct-credentials.json"
                    private_json(direct_cred, {"access_key_id": "mileslocal", "secret_access_key": "mileslocal"})
                    for name, target, origin, bucket, credentials in (
                        ("direct", "local-s3-smoke", endpoint, direct_bucket, direct_cred),
                        ("miles", "miles-warm", gateway, "warm-benchmark", credential_file),
                    ):
                        declared = dict(environment, metadata="badger" if name == "miles" else "none",
                                        gateway_commit=command(["git", "rev-parse", "HEAD"]).strip() if name == "miles" else "",
                                        policy_revision="1" if name == "miles" else "",
                                        cache_state="fresh process/LRU before PUT; no admit-on-write; GET passes separate" if name == "miles" else "emulator internals uncontrolled",
                                        isolation="new bucket, new Badger/domain metadata and physical backend bucket per trial" if name == "miles" else "new direct bucket per trial")
                        config = dict(target=target, endpoint=origin, region="ap-southeast-1", bucket=bucket, credentials_file=str(credentials), environment=declared)
                        file = trial / (name + "-config.json")
                        private_json(file, config)
                        command([str(binaries / "warmbench"), "--config", str(file), "--manifest", str(manifest), "--out", str(trial / name), "--concurrency", str(concurrency), "--read-repeats", "3"], env, timeout=180)
                        raw = (trial / name / "result.json").read_bytes()
                        r = json.loads(raw)
                        gets = [s for s in r["samples"] if s["operation"] == "GetObject"]
                        if r["status"] != "passed" or len(gets) != 36 or any(s.get("error") or len(s.get("sha256", "")) != 64 for s in gets):
                            raise RuntimeError("smoke did not verify every logical file")
                        summary["runs"].append({"target": target, "concurrency": concurrency, "samples": len(r["samples"]), "verified_gets": len(gets), "artifact_sha256": hashlib.sha256(raw).hexdigest()})
                        print("PASS", name, "concurrency", concurrency, "36 full-file GET hashes", flush=True)
                    process.send_signal(signal.SIGTERM)
                    if process.wait(timeout=15) != 0 or '"msg":"shutdown complete"' not in log.read_text():
                        raise RuntimeError("gateway did not shut down cleanly")
                    if cred["secret_access_key"] in log.read_text():
                        raise RuntimeError("secret found in private runtime log")
                finally:
                    if process.poll() is None:
                        process.terminate()
                        try:
                            process.wait(timeout=15)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=5)
        private_json(args.out / "summary.json", summary)
    finally:
        command(compose + ["down", "--volumes", "--remove-orphans"], env, timeout=60)
    print("PASS paired local protocol smoke; no AWS calls; private evidence:", args.out, flush=True)


if __name__ == "__main__":
    main()
