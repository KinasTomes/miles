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
import subprocess
import sys
import uuid

from local_test import ROOT, command, fixture_environment, local_endpoint, wait_backend
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
        command(compose + ["up", "-d", "dynamodb", "s3"], env, timeout=300)
        endpoint = local_endpoint("http://" + command(compose + ["port", "dynamodb", "8000"], env).strip())
        s3_endpoint = local_endpoint("http://" + command(compose + ["port", "s3", "5000"], env).strip())
        wait_backend(env, endpoint, "dynamodb")
        wait_backend(env, s3_endpoint, "s3api")
        native_env = dict(env, MILES_DDB_LOCAL_ENDPOINT=endpoint, MILES_DDB_LOCAL_REQUIRED="1",
                          MILES_WARM_METADATA_ARTIFACT=str(args.out / "dynamodb-metadata.json"),
                          MILES_WARM_S3_ENDPOINT=s3_endpoint, MILES_WARM_S3_REQUIRED="1",
                          MILES_WARM_HTTP_ARTIFACT_ROOT=str(args.out / "dynamodb-warm"), MILES_WARM_SOURCE_REVISION=source)
        native = subprocess.run(["go", "test", "-v", "-count=1", "-timeout=10m", "./internal/metadata/dynamodb",
                                 "-run", "^TestLocal(WarmMetadataPrototype|WarmHTTPDynamoDBPrototype|PolicyPersistence|MultipartContract|ProducerContract)$"],
                                cwd=ROOT, env=native_env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=660)
        (args.out / "native-integration.log").write_text(native.stdout)
        if native.returncode:
            raise RuntimeError("native integration failed:\n" + native.stdout[-12000:])
        probe = json.loads((args.out / "dynamodb-metadata.json").read_text())
        if probe["metadata_backend"] != "dynamodb-local" or probe["status"] != "passed" or len(probe["samples"]) != 30:
            raise RuntimeError("native adapter probe lacks its expected verified samples")
        dynamodb_runs = []
        for concurrency in (1, 4):
            for target in ("direct", "miles"):
                relative = f"dynamodb-warm/concurrency-{concurrency}/{target}/result.json"
                raw = (args.out / relative).read_bytes()
                result = json.loads(raw)
                gets = [s for s in result["samples"] if s["operation"] == "GetObject"]
                if result["status"] != "passed" or len(gets) != 36 or any(s.get("error") or len(s.get("sha256", "")) != 64 for s in gets):
                    raise RuntimeError("DynamoDB Warm HTTP run did not verify all full-file hashes")
                dynamodb_runs.append({"path": relative, "target": result["target"], "metadata_backend": result["declared_environment"]["metadata"],
                                      "concurrency": concurrency, "samples": len(result["samples"]), "verified_gets": len(gets),
                                      "artifact_sha256": hashlib.sha256(raw).hexdigest()})
        print("PASS native DynamoDB Warm HTTP C1/C4 + direct Moto, metadata probe and consumer contracts", flush=True)
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
               "artifacts_sha256": artifacts, "warm_runs": warm["runs"], "dynamodb_warm_runs": dynamodb_runs,
               "metadata_backends": {"warm_http": "badger", "dynamodb_warm_http": "dynamodb-local", "adapter_probe": "dynamodb-local"},
               "integration": "Shared policy, Admin primitives/CLI, multipart and producer ports; not a production Admin/multipart HTTP composition",
               "limitations": ["Synthetic traces are model inputs, not observed AWS usage or a complete cost total",
                               "Warm HTTP uses the real handlers/service/engine with test-only authority and emulators; not production R1/R2 or billed AWS capacity",
                               "Multi-node Cloud/Edge deployment and final live cost/performance acceptance are downstream consumers, not W4 prerequisites"]}
    private_json(args.out / "summary.json", summary)
    print("PASS original Warm harness: C1/C4, gateway and direct emulator, all full-file hashes", flush=True)
    print("PASS local integration prototype; no AWS calls; private evidence:", args.out, flush=True)


if __name__ == "__main__":
    main()
