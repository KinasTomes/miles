#!/usr/bin/env python3
"""Run TestColdLiveFrontDoors against an approved, already deployed installation.

The runner never edits Miles coordination state, deletes jobs or kills
processes it did not start. Cloud must be the deployed ALB/EC2 front door named
in the private live config; with --local-edge the runner starts only an Edge
runtime and its TLS front door and stops exactly those processes afterwards.

Private directory (outside Git):
  installation.json      reviewed installation inventory (operator.login_profile)
  miles-cold-live.json   live config; its installation_file points at installation.json
  edge-runtime.json      --local-edge only: Edge runtime config (runtime role via aws_config_file)
  edge-nginx.conf        --local-edge only: Edge TLS front door
"""
import argparse
import hashlib
import json
import os
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
# Credentials a child must never inherit: the Edge runtime uses its own
# bounded role, never the deploy session used for the preflight.
CREDENTIAL_ENV = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_CREDENTIAL_EXPIRATION")


def clean_commit():
    if subprocess.run(["git", "status", "--porcelain"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip():
        sys.exit("refusing to run: working tree is not clean, evidence must bind to one commit")
    return subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip()


def deploy_credentials(profile):
    out = subprocess.run(["aws", "configure", "export-credentials", "--profile", profile, "--format", "process"],
                         capture_output=True, text=True, check=True).stdout
    c = json.loads(out)
    env = {"AWS_ACCESS_KEY_ID": c["AccessKeyId"], "AWS_SECRET_ACCESS_KEY": c["SecretAccessKey"]}
    if c.get("SessionToken"):
        env["AWS_SESSION_TOKEN"] = c["SessionToken"]
    return env


def stop(processes):
    for p in processes:
        if p.poll() is None:
            p.terminate()
    for p in processes:
        try:
            p.wait(timeout=30)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--private", required=True, type=Path)
    parser.add_argument("--local-edge", action="store_true", help="start a local Edge runtime and front door for this run")
    parser.add_argument("--timeout", default="150m", help="go test timeout; covers the real protection wait")
    args = parser.parse_args()

    private = args.private.resolve()
    inventory = json.loads((private / "installation.json").read_text())
    live_config = private / "miles-cold-live.json"
    result = Path(json.loads(live_config.read_text())["result_file"])
    if result.exists():
        sys.exit(f"refusing to run: result file already exists: {result}")
    commit = clean_commit()

    processes = []
    try:
        if args.local_edge:
            node_env = {k: v for k, v in os.environ.items() if k not in CREDENTIAL_ENV}
            # Disclosed in the evidence: periodic jobs off on this Edge node only.
            node_env["MILES_DISABLE_PERIODIC"] = "1"
            processes.append(subprocess.Popen(["nginx", "-c", str(private / "edge-nginx.conf"), "-g", "daemon off;"]))
            processes.append(subprocess.Popen([str(ROOT / "bin/miles"), "--config", str(private / "edge-runtime.json")], env=node_env))
            time.sleep(3)
            for p in processes:
                if p.poll() is not None:
                    raise RuntimeError(f"{p.args[0]} exited early with code {p.returncode}")

        test_env = {k: v for k, v in os.environ.items() if k not in CREDENTIAL_ENV}
        test_env.update(deploy_credentials(inventory["operator"]["login_profile"]))
        test_env.update(AWS_REGION=inventory["region"], MILES_COLD_LIVE_CONFIG=str(live_config), MILES_RUN_COMMIT=commit)
        code = subprocess.run(["go", "test", "-v", "-count=1", "-timeout", args.timeout, "-run", "^TestColdLiveFrontDoors$", "./tests/live"],
                              cwd=ROOT, env=test_env).returncode
    finally:
        stop(processes)

    if not result.exists():
        sys.exit("result file was not produced")
    data = json.loads(result.read_text())
    print(f"commit={commit} passed={data.get('passed')} observations={len(data.get('observations', []))} "
          f"admin_events={len(data.get('admin_events', []))}")
    print(f"private result {result} sha256={hashlib.sha256(result.read_bytes()).hexdigest()}")
    print("Review and sanitize before committing any evidence; raw results stay private.")
    sys.exit(code)


if __name__ == "__main__":
    main()
