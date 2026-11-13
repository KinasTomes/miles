#!/usr/bin/env python3
"""Bounded AWS-client workloads. Secrets arrive on stdin and never enter argv/logs."""

import argparse
import concurrent.futures
import datetime as dt
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import threading
import time

BASE = Path("/home/ec2-user/acceptance")


def trial(settings):
    name = settings["name"]
    if not re.fullmatch(r"get-c[14]-[a-f0-9]{8}", name):
        raise ValueError("Invalid trial ID")
    folder = BASE / name
    folder.mkdir(mode=0o700, exist_ok=False)
    credentials = folder / "credentials.json"
    credentials.write_text(json.dumps(settings.pop("credentials")))
    env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
    env.update(
        AWS_CONFIG_FILE="/dev/null",
        AWS_SHARED_CREDENTIALS_FILE="/dev/null",
        AWS_EC2_METADATA_DISABLED="true",
        AWS_MAX_ATTEMPTS="1",
        AWS_REGION=settings["region"],
        AWS_CA_BUNDLE=str(BASE / "tls-ca.pem"),
        SSL_CERT_FILE=str(BASE / "tls-ca.pem"),
    )
    keys = json.loads(credentials.read_text())
    create_env = {
        **env,
        "AWS_ACCESS_KEY_ID": keys["access_key_id"],
        "AWS_SECRET_ACCESS_KEY": keys["secret_access_key"],
    }
    p = subprocess.run(
        [
            "aws",
            "--region",
            settings["region"],
            "--endpoint-url",
            "https://edge.miles.test",
            "s3api",
            "create-bucket",
            "--bucket",
            settings["bucket"],
        ],
        env=create_env,
        capture_output=True,
        timeout=60,
    )
    (folder / "create.out").write_bytes(p.stdout)
    (folder / "create.err").write_bytes(p.stderr)
    if p.returncode:
        raise ValueError("CreateBucket failed; retain and reconcile before retry")
    config = {
        "target": "miles-warm",
        "endpoint": "https://edge.miles.test",
        "bucket": settings["bucket"],
        "region": settings["region"],
        "credentials_file": str(credentials),
        "kms_key_id": "",
        "environment": {
            "profile": "edge",
            "topology": "two Cloud plus AWS Edge and client",
            "host": "t4g.small CPU Standard",
            "cpu": "Graviton2 2vCPU",
            "disk": "20GiB encrypted gp3",
            "network": "private VPC HTTPS",
            "metadata": "dynamodb",
            "gateway_commit": settings["source"],
            "policy_revision": "1",
            "encryption": "HTTPS and SSE-KMS",
            "cache_state": "fresh domain",
            "isolation": "fresh synthetic user/domain/bucket",
            "async_work": "shared journal enabled",
        },
    }
    (folder / "config.json").write_text(json.dumps(config))
    remaining = (
        dt.datetime.fromisoformat(settings["stop_at"])
        - dt.datetime.now(dt.timezone.utc)
    ).total_seconds()
    if remaining < 120:
        raise ValueError("Insufficient trial/drain budget")
    p = subprocess.run(
        [
            "timeout",
            "--signal=TERM",
            "--kill-after=125s",
            str(min(1200, int(remaining))) + "s",
            str(BASE / "warmbench"),
            "--config",
            str(folder / "config.json"),
            "--manifest",
            str(BASE / "dataset/manifest.json"),
            "--out",
            str(folder / "result"),
            "--concurrency",
            str(settings["concurrency"]),
            "--read-repeats",
            "3",
            "--fail-fast",
            "--drain-timeout",
            "120s",
            "--retain-objects",
        ],
        env=env,
        capture_output=True,
        timeout=min(1200, int(remaining)) + 135,
    )
    (folder / "stdout.log").write_bytes(p.stdout)
    (folder / "stderr.log").write_bytes(p.stderr)
    (folder / "exit.json").write_text(json.dumps({"code": p.returncode}))
    return p.returncode


def pulse(settings):
    credentials = settings["credentials"]
    region = settings["region"]
    account = settings["account"]
    table = settings["table"]
    if not re.fullmatch(r"miles-dev-Outbox", table):
        raise ValueError("Pulse only supports the reviewed dev Outbox")
    expiry = dt.datetime.fromisoformat(credentials["Expiration"].replace("Z", "+00:00"))
    if not credentials["AccessKeyId"].startswith("ASIA") or expiry < dt.datetime.now(
        dt.timezone.utc
    ) + dt.timedelta(seconds=150):
        raise ValueError("Fresh temporary AWS credentials required")
    env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
    env.update(
        AWS_ACCESS_KEY_ID=credentials["AccessKeyId"],
        AWS_SECRET_ACCESS_KEY=credentials["SecretAccessKey"],
        AWS_SESSION_TOKEN=credentials["SessionToken"],
        AWS_CONFIG_FILE="/dev/null",
        AWS_SHARED_CREDENTIALS_FILE="/dev/null",
        AWS_EC2_METADATA_DISABLED="true",
        AWS_DEFAULT_REGION=region,
        AWS_REGION=region,
        AWS_PAGER="",
        AWS_MAX_ATTEMPTS="1",
        AWS_RETRY_MODE="standard",
    )
    command = [
        "aws",
        "--region",
        region,
        "--no-cli-pager",
        "--cli-connect-timeout",
        "3",
        "--cli-read-timeout",
        "5",
    ]
    identity = json.loads(
        subprocess.check_output(
            command + ["sts", "get-caller-identity", "--output", "json"],
            env=env,
            timeout=15,
        )
    )
    if identity["Account"] != account or not identity["Arn"].startswith(
        settings["role_session_prefix"]
    ):
        raise ValueError("Wrong pulse identity")
    end = time.monotonic() + 120
    stop = threading.Event()
    lock = threading.Lock()
    counts = {"success": 0, "native_throttle": 0, "unexpected": 0}
    units = [0.0]
    start = dt.datetime.now(dt.timezone.utc).isoformat()
    args = command + [
        "dynamodb",
        "scan",
        "--table-name",
        table,
        "--consistent-read",
        "--limit",
        "1024",
        "--projection-expression",
        "event_id",
        "--return-consumed-capacity",
        "TOTAL",
        "--no-paginate",
        "--query",
        "{count:Count,units:ConsumedCapacity.CapacityUnits}",
        "--output",
        "json",
    ]

    def work():
        while not stop.is_set() and time.monotonic() < end:
            consumed = 0
            try:
                p = subprocess.run(
                    args, env=env, capture_output=True, text=True, timeout=12
                )
                if p.returncode == 0:
                    category = "success"
                    consumed = float(json.loads(p.stdout)["units"])
                elif any(
                    x in p.stderr
                    for x in (
                        "ThrottlingException",
                        "ProvisionedThroughputExceededException",
                        "RequestLimitExceeded",
                    )
                ):
                    category = "native_throttle"
                else:
                    category = "unexpected"
                    stop.set()
            except (ValueError, KeyError, subprocess.TimeoutExpired):
                category = "unexpected"
                stop.set()
            with lock:
                counts[category] += 1
                units[0] += consumed

    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(lambda _: work(), range(4)))
    print(
        json.dumps(
            {
                "started_at": start,
                "finished_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                "counts": counts,
                "read_units": units[0],
                "workers": 4,
                "duration_seconds": 120,
                "page_limit": 1024,
                "table": table,
                "retries": 0,
            }
        )
    )
    return 1 if counts["unexpected"] else 0


if __name__ == "__main__":
    os.umask(0o077)
    p = argparse.ArgumentParser()
    p.add_argument("action", choices=["trial", "pulse"])
    args = p.parse_args()
    raise SystemExit(
        {"trial": trial, "pulse": pulse}[args.action](json.load(sys.stdin))
    )
