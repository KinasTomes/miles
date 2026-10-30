#!/usr/bin/env python3
"""Offline simulator + native DynamoDB adapter probe + existing Warm protocol smoke.

No AWS credentials, deployed resources, production authority or final MVP gates.
All containers/tables belong to this disposable run and are cleaned on failure.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import sys
import uuid

from local_test import command, fixture_environment, local_endpoint, wait_backend
from warm_benchmark_smoke import private_json


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--out", type=Path, required=True, help="new private directory, parent must exist")
    args = p.parse_args()
    args.out = args.out.resolve()
    args.out.mkdir(mode=0o700)
    env = fixture_environment(args.out)
    context = command(["docker", "context", "show"]).strip()
    host = json.loads(command(["docker", "context", "inspect", context]))[0]["Endpoints"]["docker"]["Host"]
    if os.environ.get("DOCKER_HOST") or not host.startswith("unix://"):
        raise RuntimeError("smoke requires a local Unix Docker context")
    source = command(["git", "rev-parse", "HEAD"]).strip()
    dirty = bool(command(["git", "status", "--porcelain"]).strip())
    binary = args.out / "costsim"
    command(["go", "build", "-trimpath", "-o", str(binary), "./cmd/costsim"], env, timeout=120)
    for name in ("cloud-boundary", "edge-revival"):
        command([str(binary), "-trace", f"benchmarks/traces/{name}.json", "-out", str(args.out / (name + ".json"))], env)
    print("PASS deterministic accounting traces and shared tiering decisions", flush=True)
    project = "miles-costsim-" + uuid.uuid4().hex[:12]
    compose = ["docker", "compose", "-f", "deploy/local/compose.yaml", "-p", project]
    try:
        command(compose + ["up", "-d", "dynamodb"], env, timeout=300)
        endpoint = local_endpoint("http://" + command(compose + ["port", "dynamodb", "8000"], env).strip())
        wait_backend(env, endpoint, "dynamodb")
        native_env = dict(env, MILES_DDB_LOCAL_ENDPOINT=endpoint, MILES_DDB_LOCAL_REQUIRED="1",
                          MILES_WARM_METADATA_ARTIFACT=str(args.out / "dynamodb-metadata.json"))
        logs = command(["go", "test", "-v", "-count=1", "-timeout=10m", "./internal/metadata/dynamodb",
                        "-run", "^TestLocal(WarmMetadataPrototype|PolicyPersistence|MultipartContract|ProducerContract)$"],
                       native_env, timeout=660)
        (args.out / "native-integration.log").write_text(logs)
        probe = json.loads((args.out / "dynamodb-metadata.json").read_text())
        if probe["metadata_backend"] != "dynamodb-local" or probe["status"] != "passed" or len(probe["samples"]) != 30:
            raise RuntimeError("native adapter probe lacks its expected verified samples")
        print("PASS native DynamoDB metadata timing probe, policy, multipart and producer contracts", flush=True)
    finally:
        command(compose + ["down", "--volumes", "--remove-orphans"], env, timeout=60)
    logs = command(["go", "test", "-count=1", "./internal/api/admin", "./cmd/dsgctl", "./internal/config/integration",
                    "./internal/multipart", "./internal/cost", "./internal/simulator", "./cmd/costsim"], env, timeout=180)
    (args.out / "consumer-contracts.log").write_text(logs)
    # Reuse the original harness and fresh dataset/isolation, not a parallel runner.
    warm_logs = command([sys.executable, "-B", "scripts/warm_benchmark_smoke.py", "--out", str(args.out / "warm")], env, timeout=600)
    (args.out / "warm-smoke.log").write_text(warm_logs)
    warm = json.loads((args.out / "warm" / "summary.json").read_text())
    if len(warm["runs"]) != 4 or any(r["verified_gets"] != 36 for r in warm["runs"]):
        raise RuntimeError("Warm smoke did not verify all full-file hashes")
    artifacts = {name: hashlib.sha256((args.out / name).read_bytes()).hexdigest()
                 for name in ("cloud-boundary.json", "edge-revival.json", "dynamodb-metadata.json", "native-integration.log", "consumer-contracts.log", "warm-smoke.log")}
    summary = {"schema_version": 1, "kind": "cost-simulator-local-integration-prototype", "source_revision": source,
               "dirty_source": dirty, "host": platform.system() + " " + platform.machine(),
               "costsim_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(), "mvp_threshold_applied": False,
               "artifacts_sha256": artifacts, "warm_runs": warm["runs"],
               "metadata_backends": {"warm_http": "badger", "adapter_probe": "dynamodb-local"},
               "integration": "Shared policy, Admin primitives/CLI, multipart and producer ports; not a production Admin/multipart HTTP composition",
               "limitations": ["Synthetic traces are model inputs, not observed AWS usage or a complete cost total",
                               "DynamoDB Local probe is not Warm HTTP/DynamoDB throughput, production R1/R2 or billed AWS capacity",
                               "Full DynamoDB runtime, distributed executors and live cost/performance acceptance remain W5/W6"]}
    private_json(args.out / "summary.json", summary)
    print("PASS original Warm harness: C1/C4, gateway and direct emulator, all full-file hashes", flush=True)
    print("PASS local integration prototype; no AWS calls; private evidence:", args.out, flush=True)


if __name__ == "__main__":
    main()
