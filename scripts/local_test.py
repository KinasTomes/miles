#!/usr/bin/env python3
"""Disposable DynamoDB Local/Moto fixtures and real gateway-process smoke tests.

Uses the AWS CLI with dummy credentials and explicit loopback endpoints only.
Does not implement production adapters or claim AWS/R1/R2 acceptance.
"""
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, build_opener
import uuid

ROOT = Path(__file__).resolve().parents[1]
HTTP = build_opener(ProxyHandler({}))


def local_endpoint(value):
    parsed = urlsplit(value)
    if (parsed.scheme != "http" or parsed.hostname != "127.0.0.1"
            or parsed.username or parsed.password or parsed.path not in ("", "/")
            or parsed.query or parsed.fragment or not parsed.port):
        raise ValueError("fixture endpoint must be explicit HTTP IPv4 loopback")
    return f"http://127.0.0.1:{parsed.port}"


def fixture_environment(directory):
    env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
    config = directory / "aws-config"
    config.write_text("[default]\nregion = ap-southeast-1\ns3 =\n    addressing_style = path\n")
    env.update(AWS_ACCESS_KEY_ID="mileslocal", AWS_SECRET_ACCESS_KEY="mileslocal",
               AWS_DEFAULT_REGION="ap-southeast-1", AWS_EC2_METADATA_DISABLED="true",
               AWS_CONFIG_FILE=str(config), AWS_SHARED_CREDENTIALS_FILE=os.devnull,
               AWS_MAX_ATTEMPTS="1", AWS_PAGER="", NO_PROXY="127.0.0.1,localhost")
    return env


def command(args, env=None, expect_error=None, timeout=60):
    result = subprocess.run(args, cwd=ROOT, env=env, text=True, capture_output=True, timeout=timeout)
    if expect_error:
        if result.returncode == 0 or expect_error not in result.stderr:
            raise RuntimeError(f"expected {expect_error}: {result.stdout} {result.stderr}")
    elif result.returncode:
        raise RuntimeError(f"{args[0]} failed: {result.stderr}")
    return result.stdout


def aws(env, endpoint, service, *args, expect_error=None):
    return command(["aws", "--endpoint-url", local_endpoint(endpoint), "--no-cli-pager",
                    "--cli-connect-timeout", "2", "--cli-read-timeout", "5", "--output", "json",
                    service, *args], env, expect_error)


def wait_backend(env, endpoint, service):
    # Docker's port proxy can accept TCP before the service can answer its API.
    operation = "list-tables" if service == "dynamodb" else "list-buckets"
    expected = "TableNames" if service == "dynamodb" else "Buckets"
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        try:
            if expected in json.loads(aws(env, endpoint, service, operation)):
                return
        except (RuntimeError, json.JSONDecodeError):
            pass
        time.sleep(0.2)
    raise RuntimeError(f"{service} fixture API did not become usable within 60 seconds")


def dynamodb_fixture(env, endpoint, suffix):
    table = "miles_fixture_" + suffix
    aws(env, endpoint, "dynamodb", "create-table", "--table-name", table,
        "--attribute-definitions", "AttributeName=pk,AttributeType=S",
        "--key-schema", "AttributeName=pk,KeyType=HASH", "--billing-mode", "PAY_PER_REQUEST")
    try:
        def put(key, revision):
            return {"Put": {"TableName": table, "Item": {"pk": {"S": key}, "revision": {"N": str(revision)}},
                            "ConditionExpression": "attribute_not_exists(pk)"}}
        aws(env, endpoint, "dynamodb", "transact-write-items", "--transact-items",
            json.dumps([put("head", 1), put("event", 1)]))
        failed = [{"Update": {"TableName": table, "Key": {"pk": {"S": "head"}},
                              "UpdateExpression": "SET revision=:next", "ConditionExpression": "revision=:old",
                              "ExpressionAttributeValues": {":old": {"N": "0"}, ":next": {"N": "2"}}}}, put("failed-event", 2)]
        aws(env, endpoint, "dynamodb", "transact-write-items", "--transact-items",
            json.dumps(failed), expect_error="TransactionCanceledException")
        head = json.loads(aws(env, endpoint, "dynamodb", "get-item", "--table-name", table,
                             "--key", '{"pk":{"S":"head"}}', "--consistent-read"))
        event = json.loads(aws(env, endpoint, "dynamodb", "get-item", "--table-name", table,
                              "--key", '{"pk":{"S":"event"}}', "--consistent-read"))
        if event["Item"]["revision"]["N"] != "1":
            raise RuntimeError("committed transaction is missing its event")
        if head["Item"]["revision"]["N"] != "1":
            raise RuntimeError("failed transaction changed head")
        absent = json.loads(aws(env, endpoint, "dynamodb", "get-item", "--table-name", table,
                               "--key", '{"pk":{"S":"failed-event"}}', "--consistent-read") or "{}")
        if "Item" in absent:
            raise RuntimeError("failed transaction leaked outbox effect")
    finally:
        aws(env, endpoint, "dynamodb", "delete-table", "--table-name", table)
    print("PASS DynamoDB Local: atomic bundle, conditional failure, consistent outcome and cleanup", flush=True)


def s3_fixture(env, endpoint, suffix, directory):
    bucket = "miles-fixture-" + suffix
    aws(env, endpoint, "s3api", "create-bucket", "--bucket", bucket,
        "--create-bucket-configuration", "LocationConstraint=ap-southeast-1")
    source = directory / "source.bin"
    source.write_bytes(b"miles-local-fixture")
    try:
        result = json.loads(aws(env, endpoint, "s3api", "put-object", "--bucket", bucket,
                                "--key", "pack", "--body", str(source), "--if-none-match", "*"))
        candidate = directory / "candidate.bin"
        candidate.write_bytes(b"must-not-overwrite-original-pack")
        aws(env, endpoint, "s3api", "put-object", "--bucket", bucket, "--key", "pack",
            "--body", str(candidate), "--if-none-match", "*", expect_error="PreconditionFailed")
        head = json.loads(aws(env, endpoint, "s3api", "head-object", "--bucket", bucket, "--key", "pack"))
        if head["ContentLength"] != source.stat().st_size:
            raise RuntimeError("S3 HEAD length mismatch")
        target = directory / "range.bin"
        aws(env, endpoint, "s3api", "get-object", "--bucket", bucket, "--key", "pack",
            "--range", "bytes=2-6", "--if-match", result["ETag"], str(target))
        if target.read_bytes() != source.read_bytes()[2:7]:
            raise RuntimeError("S3 range mismatch")
        aws(env, endpoint, "s3api", "get-object", "--bucket", bucket, "--key", "pack",
            "--if-match", '"wrong-etag"', str(directory / "rejected.bin"), expect_error="PreconditionFailed")
    finally:
        aws(env, endpoint, "s3api", "delete-object", "--bucket", bucket, "--key", "pack")
        aws(env, endpoint, "s3api", "delete-bucket", "--bucket", bucket)
    print("PASS test S3: conditional create, HEAD, Range/If-Match and cleanup", flush=True)


def response(url):
    try:
        with HTTP.open(url, timeout=2) as result:
            return result.status, result.read().decode()
    except HTTPError as error:
        return error.code, error.read().decode()


def runtime_fixture(binary, profile, mode, directory, env):
    file = directory / f"{profile}-{mode}.json"
    file.write_text(json.dumps({"profile": profile, "mode": mode, "health_address": "127.0.0.1:0", "shutdown_seconds": 2}))
    log_path = directory / f"{profile}-{mode}.log"
    with log_path.open("w") as log:
        process = subprocess.Popen([str(binary), "-config", str(file)], cwd=ROOT, env=env,
                                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=log)
        try:
            address = None
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError("runtime exited during startup: " + log_path.read_text())
                for line in log_path.read_text().splitlines(keepends=True):
                    if not line.endswith("\n"):
                        continue
                    record = json.loads(line)
                    if record.get("msg") == "health server listening":
                        address = record["address"]
                if address:
                    break
                time.sleep(0.02)
            if not address:
                raise RuntimeError("runtime did not announce its listener")
            base = local_endpoint("http://" + address)
            if response(base + "/healthz")[0] != 200:
                raise RuntimeError("process is not healthy")
            expected = 200 if mode == "dev" else 503
            deadline = time.monotonic() + 5
            while response(base + "/readyz")[0] != expected:
                if time.monotonic() >= deadline:
                    raise RuntimeError("runtime readiness did not match configured adapter mode")
                time.sleep(0.02)
            status, metrics = response(base + "/metrics")
            expected_metric = f'miles_ready{{mode="{mode}"}} {1 if mode == "dev" else 0}'
            if status != 200 or expected_metric not in metrics:
                raise RuntimeError("missing Prometheus readiness observation")
            process.send_signal(signal.SIGTERM)
            if process.wait(timeout=5) != 0 or '"msg":"shutdown complete"' not in log_path.read_text():
                raise RuntimeError("SIGTERM did not shut down cleanly")
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
    print(f"PASS {profile}/{mode}: process, health, readiness, metrics and SIGTERM", flush=True)


def main():
    for executable in ("docker", "aws", "go"):
        if not shutil.which(executable):
            raise RuntimeError(f"required local tool missing: {executable}")
    # Refuse a remote Docker context: published fixture ports must belong to this host.
    context = command(["docker", "context", "show"]).strip()
    host = json.loads(command(["docker", "context", "inspect", context]))[0]["Endpoints"]["docker"]["Host"]
    if os.environ.get("DOCKER_HOST") or not host.startswith("unix://"):
        raise RuntimeError("local fixture runner requires a local Unix Docker context")
    suffix = uuid.uuid4().hex[:12]
    compose = ["docker", "compose", "-f", "deploy/local/compose.yaml", "-p", "miles-w02-" + suffix]
    with tempfile.TemporaryDirectory(prefix="miles-local-") as tmp:
        directory = Path(tmp)
        env = fixture_environment(directory)
        try:
            command(compose + ["up", "-d"], env, timeout=300)
            endpoints = [local_endpoint("http://" + command(compose + ["port", service, port], env).strip())
                         for service, port in (("dynamodb", "8000"), ("s3", "5000"))]
            for endpoint, service in zip(endpoints, ("dynamodb", "s3api")):
                wait_backend(env, endpoint, service)
            dynamodb_fixture(env, endpoints[0], suffix)
            s3_fixture(env, endpoints[1], suffix, directory)
            binary = directory / "miles"
            command(["go", "build", "-trimpath", "-o", str(binary), "./cmd/miles"], env, timeout=120)
            for profile, mode in (("edge", "dev"), ("cloud", "dev"), ("cloud", "runtime")):
                runtime_fixture(binary, profile, mode, directory, env)
        finally:
            command(compose + ["down", "--volumes", "--remove-orphans"], env, timeout=60)
    print("PASS local fixtures complete; temporary project, processes and files removed", flush=True)


if __name__ == "__main__":
    main()
