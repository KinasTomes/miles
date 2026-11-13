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



def verify_seed_manifest(dataset_root, manifest_path):
    """Verify all local bytes before any Cold/Recovery AWS mutation."""
    import hashlib
    manifest = json.loads(manifest_path.read_text())
    files = manifest.get("files", [])
    if not files or len(files) > 10000:
        raise ValueError("Invalid benchmark manifest file list")
    total = 0
    names = set()
    for entry in files:
        name = entry.get("path", "")
        if not isinstance(name, str) or not name or name.startswith("/") or chr(92) in name:
            raise ValueError("Unsafe dataset path")
        parts = Path(name).parts
        if any(x in ("", ".", "..") for x in parts) or name in names:
            raise ValueError("Unsafe or duplicate dataset entry")
        names.add(name)
        file = (dataset_root / name).resolve()
        if not file.is_relative_to(dataset_root.resolve()) or not file.is_file():
            raise ValueError("Dataset symlink/path escape or missing file")
        size, hasher = 0, hashlib.sha256()
        with file.open("rb") as handle:
            while chunk := handle.read(64 * 1024):
                size += len(chunk)
                hasher.update(chunk)
        if (size != entry.get("bytes") or size > 5 << 30
                or hasher.hexdigest() != entry.get("sha256")):
            raise ValueError("Dataset SHA/size mismatch before Cold/Recovery seed")
        total += size
    if total != manifest.get("total_bytes"):
        raise ValueError("Dataset total size mismatch")
    return files


def seed_benchmark_dataset(settings, folder, env, endpoint, tenant, manifest_path):
    """PUT synthetic Warm bytes and HEAD verify length.

    Partial unknown operations retain receipts for reconciliation, not replay.
    """
    dataset_root = manifest_path.parent
    files = verify_seed_manifest(dataset_root, manifest_path)
    if settings.get("runner") in {"coldbench", "recoverybench"}:
        storage_class = "STANDARD"
    else:
        storage_class = None
    if not storage_class:
        raise ValueError("Unsupported Cold/Recovery storage class")
    keys = tenant["credentials"]
    aws_env = {**env, "AWS_ACCESS_KEY_ID": keys["access_key_id"],
               "AWS_SECRET_ACCESS_KEY": keys["secret_access_key"]}
    receipts = []
    receipt_file = folder / "benchmark-seed-receipts.json"
    for entry in files:
        key = entry["path"]
        if "stop_at" in settings and (dt.datetime.fromisoformat(settings["stop_at"])
                - dt.datetime.now(dt.timezone.utc)).total_seconds() < 240:
            raise ValueError("Stop seed before bounded AWS call can enter cleanup reserve")
        pending = {"path": key, "sha256": entry["sha256"], "class": storage_class, "state": "dispatched-unknown"}
        receipts.append(pending)
        receipt_file.write_text(json.dumps(receipts, indent=2))
        command = ["aws", "--region", settings["region"], "--endpoint-url", endpoint,
                   "s3api"]
        put = subprocess.run(
            command + ["put-object", "--bucket", tenant["bucket"], "--key", key,
                       "--body", str(dataset_root / key), "--storage-class", storage_class],
            env=aws_env, capture_output=True, text=True, timeout=180,
        )
        if put.returncode:
            raise ValueError("Cold PUT failed or unknown; do not replay: " + key)
        try:
            result = json.loads(put.stdout)
        except json.JSONDecodeError as error:
            raise ValueError("Cold PUT returned unverified receipt: " + key) from error
        head = subprocess.run(
            command + ["head-object", "--bucket", tenant["bucket"], "--key", key],
            env=aws_env, capture_output=True, text=True, timeout=60,
        )
        if head.returncode:
            raise ValueError("Cold HEAD missing after PUT; reconciliation required")
        data = json.loads(head.stdout)
        observed_class = data.get("StorageClass") or "STANDARD"
        if observed_class != "STANDARD" or data.get("ContentLength") != entry["bytes"]:
            raise ValueError("Cold/Recovery backend class/length not verified; no test run")
        pending.update({
            "path": key, "class": storage_class, "bytes": entry["bytes"],
            "version": data.get("VersionId"), "put_version": result.get("VersionId"),
            "sha256": entry["sha256"],
            "state": "verified",
        })
        receipt_file.write_text(json.dumps(receipts, indent=2))
    return receipts

def benchmark(settings):
    native = settings.get("native")
    if settings.get("runner") in {"coldbench", "recoverybench"}:
        if not isinstance(native, dict) or not settings.get("recovery_aws"):
            raise ValueError("Native installation/provider not staged and reviewed; no mutation")
        if (native.get("slot") != settings.get("name") or native.get("source") != settings.get("source")
                or native.get("stop_at") != settings.get("stop_at") or not native.get("tables")):
            raise ValueError("Native installation binding differs; no mutation")
    name = settings["name"]
    if not re.fullmatch(r"[a-z0-9-]+-rep[1-3]", name):
        raise ValueError("Invalid benchmark slot ID")
    folder = BASE / name
    folder.mkdir(mode=0o700, exist_ok=False)

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

    cred_file = folder / "credentials.json"
    t1 = settings["tenant1"]
    if settings.get("runner") in {"coldbench", "recoverybench"}:
        temporary = settings["recovery_aws"]
        expires = dt.datetime.fromisoformat(temporary["Expiration"].replace("Z", "+00:00"))
        if (temporary.get("Version") != 1 or not temporary.get("AccessKeyId", "").startswith("ASIA")
                or not temporary.get("SessionToken") or temporary.get("expected_account") != native["account"]
                or expires <= dt.datetime.fromisoformat(settings["stop_at"])):
            raise ValueError("AWS native session must cover this bounded trial; no mutation")
        (folder / "native.json").write_text(json.dumps(native))
        (folder / "native.json").chmod(0o600)
        aws_file = folder / "recovery-aws.json"
        aws_file.write_text(json.dumps({k: temporary[k] for k in
            ("Version", "AccessKeyId", "SecretAccessKey", "SessionToken", "Expiration")}))
        aws_file.chmod(0o600)
    cred_file.write_text(json.dumps(t1["credentials"]))
    os.chmod(cred_file, 0o600)

    t1_keys = t1["credentials"]
    t1_create_env = {
        **env,
        "AWS_ACCESS_KEY_ID": t1_keys["access_key_id"],
        "AWS_SECRET_ACCESS_KEY": t1_keys["secret_access_key"],
    }
    endpoint = settings.get("endpoint")
    if not endpoint:
        endpoint = "https://cloud.miles.test" if settings.get("profile") == "cloud" or settings.get("topology_type") == "cloud" else "https://edge.miles.test"
    profile = settings.get("profile")
    if not profile:
        profile = "cloud" if settings.get("topology_type") == "cloud" else "edge"
    topology = settings.get("topology")
    if not topology:
        topology = "two Cloud instances behind private internal ALB" if profile == "cloud" else "two Cloud plus AWS Edge and client"

    p1 = subprocess.run(
        [
            "aws",
            "--region",
            settings["region"],
            "--endpoint-url",
            endpoint,
            "s3api",
            "create-bucket",
            "--bucket",
            t1["bucket"],
        ],
        env=t1_create_env,
        capture_output=True,
        timeout=60,
    )
    (folder / "create-t1.out").write_bytes(p1.stdout)
    (folder / "create-t1.err").write_bytes(p1.stderr)
    if p1.returncode:
        raise ValueError("Tenant 1 CreateBucket failed; retain and reconcile")

    config1 = {
        "target": "miles-warm",
        "endpoint": endpoint,
        "bucket": t1["bucket"],
        "region": settings["region"],
        "credentials_file": str(cred_file),
        "kms_key_id": "",
        "dedup_domain": t1.get("domain", settings.get("domain", "default")),
        "environment": {
            "profile": profile,
            "topology": topology,
            "host": "t4g.small CPU Standard",
            "cpu": "Graviton2 2vCPU",
            "disk": "20GiB encrypted gp3",
            "network": "private VPC HTTPS",
            "metadata": "dynamodb",
            "gateway_commit": settings["source"],
            "policy_revision": "1",
            "encryption": "HTTPS and SSE-KMS",
            "cache_state": settings.get("cache_state", "verify-before-run"),
            "isolation": "fresh synthetic user/domain/bucket",
            "async_work": "shared journal enabled",
        },
    }
    cfg_file = folder / "config.json"
    cfg_file.write_text(json.dumps(config1))
    os.chmod(cfg_file, 0o600)

    cfg2_file = None
    if "tenant2" in settings:
        t2 = settings["tenant2"]
        cred2_file = folder / "credentials-t2.json"
        cred2_file.write_text(json.dumps(t2["credentials"]))
        os.chmod(cred2_file, 0o600)
        t2_keys = t2["credentials"]
        t2_create_env = {
            **env,
            "AWS_ACCESS_KEY_ID": t2_keys["access_key_id"],
            "AWS_SECRET_ACCESS_KEY": t2_keys["secret_access_key"],
        }
        p2 = subprocess.run(
            [
                "aws",
                "--region",
                settings["region"],
                "--endpoint-url",
                endpoint,
                "s3api",
                "create-bucket",
                "--bucket",
                t2["bucket"],
            ],
            env=t2_create_env,
            capture_output=True,
            timeout=60,
        )
        (folder / "create-t2.out").write_bytes(p2.stdout)
        (folder / "create-t2.err").write_bytes(p2.stderr)
        if p2.returncode:
            raise ValueError("Tenant 2 CreateBucket failed; retain and reconcile")

        config2 = {
            "target": "miles-warm",
            "endpoint": endpoint,
            "bucket": t2["bucket"],
            "region": settings["region"],
            "credentials_file": str(cred2_file),
            "kms_key_id": "",
            "dedup_domain": t2.get("domain", "domain-b"),
            "environment": config1["environment"],
        }
        cfg2_file = folder / "config-t2.json"
        cfg2_file.write_text(json.dumps(config2))
        os.chmod(cfg2_file, 0o600)

    if settings.get("dataset") == "boundary-8mib":
        manifest_path = BASE / "dataset-boundary-8mib/manifest.json"
    else:
        manifest_path = BASE / "dataset/manifest.json"

    source_prefix = settings.get("source_prefix", "")
    if settings.get("scenario") == "edge-clone" and not source_prefix:
        seed_out = folder / "seed"
        seed_cmd = [
            str(BASE / "warmbench"),
            "--config", str(cfg_file),
            "--manifest", str(manifest_path),
            "--out", str(seed_out),
            "--scenario", "warm",
            "--variant", "warm-c1",
            "--concurrency", "1",
            "--retain-objects",
        ]
        sp = subprocess.run(seed_cmd, env=env, capture_output=True, timeout=300)
        (folder / "seed.out").write_bytes(sp.stdout)
        (folder / "seed.err").write_bytes(sp.stderr)
        if sp.returncode:
            raise ValueError("Edge-clone seed run failed")
        seed_res = json.loads((seed_out / "result.json").read_text())
        source_prefix = seed_res["prefix"]

    if settings.get("runner") in {"coldbench", "recoverybench"}:
        verify_seed_manifest(manifest_path.parent, manifest_path)
    if settings.get("runner") in {"coldbench", "recoverybench"}:
        if (dt.datetime.fromisoformat(settings["stop_at"])
                - dt.datetime.now(dt.timezone.utc)).total_seconds() < 600:
            raise ValueError("Cold/Recovery seed requires >=10 min before cleanup reserve")
        seed_benchmark_dataset(settings, folder, env, endpoint, t1, manifest_path)

    remaining = (
        dt.datetime.fromisoformat(settings["stop_at"])
        - dt.datetime.now(dt.timezone.utc)
    ).total_seconds()
    if remaining < 120:
        raise ValueError("Insufficient trial/drain budget")

    timeout_s = min(1100, int(remaining)-135)
    if settings.get("direct_s3"):
        # Reserve both legs and process termination inside the same window.
        timeout_s = min(500, int((remaining-270)/2))
    if timeout_s < 30:
        raise ValueError("Insufficient bounded workload/comparator time")
    runner = settings.get("runner", "warmbench")
    if runner == "coldbench":
        cmd = [
            "timeout",
            "--signal=TERM",
            "--kill-after=125s",
            str(timeout_s) + "s",
            str(BASE / "coldbench"),
            "--config",
            str(cfg_file),
            "--manifest",
            str(manifest_path),
            "--out",
            str(folder / "result"),
            "--variant",
            settings["variant"],
            "--concurrency",
            str(settings["concurrency"]),
            "--poll-interval", "5s",
            "--max-poll-wait", str(max(30, timeout_s - 180)) + "s",
            "--fail-fast",
            "--retain-objects",
        ]
        cmd.extend(["--native-config", str(folder / "native.json"),
                    "--aws-credentials-file", str(aws_file)])
        # Cold seed is Warm. Tiering is an independent, exact-generation Admin
        # job after the real protection window; a logical PUT header cannot do it.
        cmd.extend(["--phase", "inspect"])
    elif runner == "recoverybench":
        expected_account = native["account"]
        cmd = [
            "timeout",
            "--signal=TERM",
            "--kill-after=125s",
            str(timeout_s) + "s",
            str(BASE / "recoverybench"),
            "--config",
            str(cfg_file),
            "--manifest",
            str(manifest_path),
            "--out",
            str(folder / "result"),
            "--variant",
            settings["variant"],
            "--fault-scope",
            settings.get(
                "fault_scope",
                "node-restart" if settings["variant"] == "r1" else "checkpoint-cut",
            ),
            "--aws-credentials-file", str(aws_file),
            "--expected-aws-account", expected_account,
            "--concurrency",
            str(settings["concurrency"]),
            "--fail-fast",
            "--retain-objects",
        ]
        cmd.extend(["--native-config", str(folder / "native.json"),
                    "--system-config-table", native["tables"]["SystemConfig"]])
    else:
        cmd = [
            "timeout",
            "--signal=TERM",
            "--kill-after=125s",
            str(timeout_s) + "s",
            str(BASE / "warmbench"),
            "--config",
            str(cfg_file),
            "--manifest",
            str(manifest_path),
            "--out",
            str(folder / "result"),
            "--scenario",
            settings["scenario"],
            "--variant",
            settings["variant"],
            "--domain",
            settings.get("domain", "default"),
            "--concurrency",
            str(settings["concurrency"]),
            "--read-repeats",
            str(settings.get("read_repeats", 3)),
            "--fail-fast",
            "--drain-timeout",
            "120s",
            "--retain-objects",
        ]
        if cfg2_file:
            cmd.extend(["--second-config", str(cfg2_file)])
        if source_prefix:
            cmd.extend(["--source-prefix", source_prefix])

    p = subprocess.run(
        cmd,
        env=env,
        capture_output=True,
        timeout=timeout_s + 135,
    )
    (folder / "stdout.log").write_bytes(p.stdout)
    (folder / "stderr.log").write_bytes(p.stderr)
    (folder / "exit.json").write_text(json.dumps({"code": p.returncode}))

    direct_code = 0
    if settings.get("direct_s3"):
        ds3 = settings["direct_s3"]
        ds3_cred = folder / "credentials-direct-s3.json"
        ds3_cred.write_text(json.dumps(ds3["credentials"]))
        os.chmod(ds3_cred, 0o600)

        ds3_config = {
            "target": "direct-s3",
            "endpoint": ds3.get("endpoint", f"https://s3.{settings['region']}.amazonaws.com"),
            "bucket": ds3["bucket"],
            "region": settings["region"],
            "credentials_file": str(ds3_cred),
            "kms_key_id": ds3["kms_key_id"],
            "environment": config1["environment"],
        }
        ds3_cfg_file = folder / "config-direct-s3.json"
        ds3_cfg_file.write_text(json.dumps(ds3_config))
        os.chmod(ds3_cfg_file, 0o600)

        ds3_cmd = [
            "timeout",
            "--signal=TERM",
            "--kill-after=125s",
            str(timeout_s) + "s",
            str(BASE / "warmbench"),
            "--config", str(ds3_cfg_file),
            "--manifest", str(manifest_path),
            "--out", str(folder / "direct-s3"),
            "--scenario", settings["scenario"],
            "--variant", settings["variant"],
            "--domain", settings.get("domain", "default"),
            "--concurrency", str(settings["concurrency"]),
            "--read-repeats", str(settings.get("read_repeats", 3)),
            "--fail-fast",
            "--drain-timeout", "120s",
            "--retain-objects",
        ]
        p_ds3 = subprocess.run(
            ds3_cmd,
            env=env,
            capture_output=True,
            timeout=timeout_s + 135,
        )
        (folder / "direct_stdout.log").write_bytes(p_ds3.stdout)
        (folder / "direct_stderr.log").write_bytes(p_ds3.stderr)
        (folder / "direct_exit.json").write_text(json.dumps({"code": p_ds3.returncode}))
        direct_code = p_ds3.returncode

    return p.returncode if p.returncode != 0 else direct_code


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


def cold_phase(settings):
    """Resume one existing slot; never create another tenant, bucket or seed."""
    name, segment = settings["name"], settings["segment"]
    phases = {"inspect", "restore-status", "restore-request", "read", "put-cold",
              "part-cold", "put-restored", "part-restored"}
    if (not re.fullmatch(r"[a-z0-9-]+-rep[1-3]", name)
            or not re.fullmatch(r"[a-f0-9]{16}", segment)
            or settings["phase"] not in phases):
        raise ValueError("Invalid Cold phase binding")
    folder = BASE / name
    baseline = json.loads((folder / "result/snapshot.json").read_text())["binding"]
    native = settings["native"]
    if {k: v for k, v in native.items() if k != "stop_at"} != {
            k: v for k, v in baseline.items() if k != "stop_at"}:
        raise ValueError("Resume installation/source differs from seed")
    temporary = settings["recovery_aws"]
    stop = dt.datetime.fromisoformat(native["stop_at"])
    expires = dt.datetime.fromisoformat(temporary["Expiration"].replace("Z", "+00:00"))
    remaining = (stop - dt.datetime.now(dt.timezone.utc)).total_seconds()
    if (not temporary["AccessKeyId"].startswith("ASIA") or not temporary.get("SessionToken")
            or temporary.get("expected_account") != native["account"]
            or expires <= stop or not 180 <= remaining <= 1200):
        raise ValueError("AWS session does not cover bounded Cold phase")
    step = folder / "steps" / segment
    step.mkdir(parents=True, mode=0o700, exist_ok=False)
    for filename, value in (("native.json", native), ("aws.json", {k: temporary[k] for k in
            ("Version", "AccessKeyId", "SecretAccessKey", "SessionToken", "Expiration")})):
        (step / filename).write_text(json.dumps(value))
        (step / filename).chmod(0o600)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith("AWS_") and k.lower() not in {"http_proxy", "https_proxy", "all_proxy"}}
    env.update(AWS_CONFIG_FILE="/dev/null", AWS_SHARED_CREDENTIALS_FILE="/dev/null",
               AWS_EC2_METADATA_DISABLED="true", SSL_CERT_FILE=str(BASE / "tls-ca.pem"))
    cmd = [str(BASE / "coldbench"), "--config", str(folder / "config.json"),
           "--manifest", str(BASE / "dataset/manifest.json"), "--out", str(step / "result"),
           "--native-config", str(step / "native.json"), "--aws-credentials-file", str(step / "aws.json"),
           "--source-snapshot", str(folder / "result/snapshot.json"),
           "--phase", settings["phase"], "--variant", settings["variant"]]
    p = subprocess.run(cmd, env=env, capture_output=True, timeout=min(1100, int(remaining)-30))
    (step / "stdout.log").write_bytes(p.stdout)
    (step / "stderr.log").write_bytes(p.stderr)
    (step / "exit.json").write_text(json.dumps({"code": p.returncode}))
    return p.returncode


def quiescence(_settings=None):
    active = []
    for process in Path("/proc").iterdir():
        if not process.name.isdigit():
            continue
        try:
            executable = (process / "exe").readlink().name
        except FileNotFoundError:
            continue
        except PermissionError:
            # Only this user's benchmark binaries can use the staged configs.
            # Check the owner before ignoring another user's protected process.
            if process.stat().st_uid == os.getuid():
                raise ValueError("Cannot inspect owned process during quiescence")
            continue
        if executable in {"warmbench", "coldbench", "recoverybench", "aws"}:
            active.append({"pid": int(process.name), "executable": executable})
    print(json.dumps({"active": active}))
    return 1 if active else 0


if __name__ == "__main__":
    os.umask(0o077)
    p = argparse.ArgumentParser()
    p.add_argument("action", choices=["trial", "pulse", "benchmark", "cold-phase", "quiescence"])
    args = p.parse_args()
    raise SystemExit(
        {"trial": trial, "pulse": pulse, "benchmark": benchmark, "cold-phase": cold_phase, "quiescence": quiescence}[args.action](
            {} if args.action == "quiescence" else json.load(sys.stdin)
        )
    )
