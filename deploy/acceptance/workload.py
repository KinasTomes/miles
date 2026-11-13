"""Operator orchestration for HTTP evidence, telemetry and exact shutdown proofs."""

import datetime as dt
import json
from pathlib import Path
import re
import secrets
import subprocess
import time
import urllib.request

from deploy.acceptance.common import ROOT, now, private, require


def tunnel(r):
    r.identity()
    require(
        r.file("edge-ready.json").exists(),
        "Edge must be ready before authenticated tunnel",
    )
    if r.file("tunnel-pid.json").exists():
        pid = r.read("tunnel-pid.json")["pid"]
        try:
            cmd = Path("/proc/" + str(pid) + "/cmdline").read_bytes()
        except FileNotFoundError:
            cmd = b""
        require(
            not cmd or (b"admin_tunnel.py" in cmd and str(r.path).encode() in cmd),
            "Tunnel PID belongs to another process",
        )
        if cmd:
            return
    with r.file("admin-tunnel.log").open("ab") as log:
        p = subprocess.Popen(
            [
                "python3",
                "-B",
                str(ROOT / "scripts/admin_tunnel.py"),
                "--ssh-config",
                str(r.file("ssh-config")),
                "--target",
                "edge",
                "--token-file",
                str(r.file("system-admin-token")),
                "--user-id",
                "operator",
                "--local-port",
                "19001",
            ],
            stdout=log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
            env=r.environment(),
        )
    r.write("tunnel-pid.json", {"pid": p.pid})
    r.wait(
        "authenticated Edge tunnel",
        lambda: "Admin tunnel ready: edge" in r.file("admin-tunnel.log").read_text(),
        60,
    )
    require(p.poll() is None, "Tunnel exited; do not send token to another listener")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def admin(r, path, body=None, method=None):
    tunnel(r)
    # The tunnel script authenticates and owns its loopback socket before publishing readiness.
    token = r.file("system-admin-token").read_text().strip()
    request = urllib.request.Request(
        "http://127.0.0.1:19001/admin/v1/" + path,
        data=json.dumps(body).encode() if body is not None else None,
        headers={
            "Authorization": "Bearer " + token,
            "Content-Type": "application/json",
        },
        method=method or ("POST" if body is not None else "GET"),
    )
    with urllib.request.build_opener(NoRedirect()).open(
        request, timeout=30
    ) as response:
        return json.load(response)


def admission(r):
    """Read the durable schedule; never open a closing gate or change its cadence."""
    expected = r.read("bootstrap-gate.json")["Item"]["history_id"]["S"]

    def safe():
        r.live_window(needed=25)

        def item(key):
            return r.aws(
                "dynamodb",
                "get-item",
                "--table-name",
                "miles-dev-SystemConfig",
                "--key",
                json.dumps({"config_id": {"S": key}}),
                "--consistent-read",
            )["Item"]

        gate = item("coordination/r1/current")
        require(
            gate["history_id"]["S"] == expected,
            "Installation history changed; stop before dispatch",
        )
        state = item("recovery/state/" + expected)
        due = dt.datetime.fromisoformat(
            state["next_cut_at"]["S"].replace("Z", "+00:00")
        )
        if (
            gate["state"]["S"] == "Open"
            and gate["epoch"]["N"] == state["next_cut_epoch"]["N"]
            and due - now() > dt.timedelta(minutes=10)
        ):
            r.write(
                "admission-proof.json",
                {"at": now().isoformat(), "gate": gate, "recovery_state": state},
            )
            return True
        return False

    r.wait("open admission away from scheduled cut", safe, 1500)


def trial(r, concurrency):
    r.identity()
    r.live_window(needed=25)
    require(concurrency in (1, 4), "Concurrency must be one or four")
    require(r.file("active-outputs.json").exists(), "Apply active stage first")
    admission(r)
    name = "get-c" + str(concurrency) + "-" + secrets.token_hex(4)
    prefix = "trials/" + name
    r.file(prefix).mkdir(parents=True, mode=0o700)
    r.write(
        prefix + "/intent.json",
        {"at": now().isoformat(), "concurrency": concurrency},
        new=True,
    )
    identity = "get-" + secrets.token_hex(6)
    admin(
        r,
        "users",
        {"user_id": identity, "default_domain": identity, "logical_limit": 2 << 30},
    )
    keys = admin(r, "credentials", {"user_id": identity})
    settings = {
        "name": name,
        "region": r.c["region"],
        "bucket": identity,
        "source": r.meta["source"],
        "concurrency": concurrency,
        "credentials": {k: keys[k] for k in ("access_key_id", "secret_access_key")},
        "stop_at": (
            dt.datetime.fromisoformat(r.read("paid-window.json")["cleanup_by"])
            - dt.timedelta(minutes=90)
        ).isoformat(),
    }
    r.write(prefix + "/input.json", settings)
    remote = "/home/ec2-user/acceptance"
    r.ssh(
        "edge",
        "sudo",
        "-n",
        "python3.12",
        "/opt/miles/current/scripts/edge_host_telemetry.py",
        "probe",
        log=prefix + "/probe.json",
    )
    unit = "miles-" + name
    r.ssh(
        "edge",
        "sudo",
        "-n",
        "systemd-run",
        "--unit",
        unit,
        "--property=TimeoutStopSec=15",
        "python3.12",
        "/opt/miles/current/scripts/edge_host_telemetry.py",
        "run",
        "--output-file",
        "/run/miles/" + name + ".jsonl",
        "--duration",
        "1800",
    )
    time.sleep(5)
    r.ssh("edge", "sudo", "-n", "systemctl", "is-active", unit)
    raw = r.ssh("edge", "sudo", "-n", "cat", "/run/miles/" + name + ".jsonl")
    records = [json.loads(x) for x in raw.splitlines()]
    require(
        sum(bool(x.get("chrony")) for x in records) >= 2,
        "Recorder did not acquire growing clock samples",
    )
    r.command(
        [
            "scp",
            "-F",
            r.file("ssh-config"),
            ROOT / "deploy/acceptance/client.py",
            "client:" + remote + "/client.py",
        ]
    )
    r.write(prefix + "/window.json", {"started_at": now().isoformat()})
    try:
        r.ssh(
            "client",
            "python3.12",
            remote + "/client.py",
            "trial",
            input=json.dumps(settings).encode(),
            timeout=1340,
            log=prefix + "/dispatch.log",
        )
    finally:
        collect(r, name)
    result = r.read(prefix + "/result.json")
    summary = r.read(prefix + "/telemetry-summary.json")
    require(
        result["status"] == "passed" and summary.get("status") == "OK",
        "Trial failed/unverified; evidence retained, do not replay blindly",
    )
    r.event("trial-" + name, "client and clock passed")
    print(name)


def benchmark_case(r, case_id, repetition):
    from deploy.acceptance import approvals, direct, trial_state, cloud_telemetry
    r.identity()
    r.live_window(needed=30)
    require(repetition in (1, 2, 3), "Repetition must be 1, 2, or 3")
    require(r.file("active-outputs.json").exists(), "Apply active stage first")

    matrix_file = ROOT / "benchmarks/workload-matrix.json"
    require(matrix_file.is_file(), "Matrix file missing")
    matrix = json.loads(matrix_file.read_text())
    cases = {c["id"]: c for c in matrix.get("cases", [])}
    require(case_id in cases, f"Unknown benchmark case ID: {case_id}")
    case = cases[case_id]
    scenario = case["scenario"]
    if scenario == "cold":
        runner_bin = "coldbench"
    elif scenario == "recovery":
        runner_bin = "recoverybench"
    elif case.get("runner") == "warmbench":
        runner_bin = "warmbench"
    else:
        raise ValueError(f"Case {case_id} requires supported runner: {case.get('runner')}")

    slot_name = f"{case_id}-rep{repetition}"
    prefix = f"trials/{slot_name}"
    require(
        not r.file(f"{prefix}/result.json").exists(),
        f"Slot {slot_name} already executed; do not replay blindly",
    )
    approvals.check(r, "benchmark-case")

    admission(r)
    r.file(prefix).mkdir(parents=True, mode=0o700, exist_ok=True)
    r.write(
        f"{prefix}/intent.json",
        {"at": now().isoformat(), "case_id": case_id, "repetition": repetition, "slot": slot_name},
        new=True,
    )

    isolation = trial_state.prepare(r, slot_name, case, admin)

    variant = case["variant"]
    concurrency = case["concurrency"]
    dataset = case["dataset"]
    domain = case["domain"]

    topology = case.get("topology", "edge")
    if topology == "cloud":
        endpoint = "https://cloud.miles.test"
        profile = "cloud"
        topology_desc = "two Cloud instances behind private internal ALB"
    else:
        endpoint = "https://edge.miles.test"
        profile = "edge"
        topology_desc = "two Cloud plus AWS Edge and client"

    if topology == "cloud":
        alb_dns = r.outputs("active")["runtime_inventory"]["alb_dns"]
        require(isinstance(alb_dns, str) and alb_dns.startswith("internal-"),
                "No private ALB DNS in active Terraform outputs")
        raw_alias = r.ssh(
            "client", "sudo", "-n", "python3.12",
            "/home/ec2-user/acceptance/host.py", "cloud-alias",
            input=json.dumps({"alb_dns": alb_dns}).encode(), timeout=30,
        )
        r.write(f"trials/{case_id}-rep{repetition}/cloud-alias.json",
                json.loads(raw_alias), new=True)
        # TLS/hostname must be verified by the private CA before sending DSG keys.
        tls_status = r.ssh(
            "client", "curl", "-sS", "--max-time", "10",
            "--cacert", "/home/ec2-user/acceptance/tls-ca.pem",
            "-o", "/dev/null", "-w", "%{http_code}",
            "https://cloud.miles.test/", timeout=20,
        ).strip()
        require(re.fullmatch(r"[234][0-9]{2}", tls_status.decode()),
                "Cloud ALB certificate/route probe failed")

    t1_id = f"{case_id[:16]}-t1-r{repetition}-{secrets.token_hex(4)}"
    t1_domain = isolation["domains"][0]
    admin(r, "users", {"user_id": t1_id, "default_domain": t1_domain, "logical_limit": 10 << 30})
    debug_scope = scenario in {"domain", "edge-clone"}
    t1_key_request = {"user_id": t1_id}
    if debug_scope:
        # Backend requires system_admin AND debug_allowed for signed DSG headers.
        # Restrict to new ephemeral benchmark principals and review separately.
        t1_key_request.update(system_admin=True, debug_allowed=True)
    t1_keys = admin(r, "credentials", t1_key_request)

    tenant1 = {
        "user_id": t1_id,
        "bucket": t1_id,
        "domain": t1_domain,
        "credentials": {k: t1_keys[k] for k in ("access_key_id", "secret_access_key")},
    }

    settings = {
        "name": slot_name,
        "case_id": case_id,
        "repetition": repetition,
        "scenario": scenario,
        "variant": variant,
        "runner": runner_bin,
        "topology_type": topology,
        "topology": topology_desc,
        "endpoint": endpoint,
        "profile": profile,
        "domain": domain,
        "concurrency": concurrency,
        "dataset": dataset,
        "region": r.c["region"],
        "source": r.meta["source"],
        "tenant1": tenant1,
        "native": trial_state.native_binding(r, slot_name),
        "stop_at": (
            dt.datetime.fromisoformat(r.read("paid-window.json")["cleanup_by"])
            - dt.timedelta(minutes=90)
        ).isoformat(),
    }
    settings["native"]["logical_bucket"] = tenant1["bucket"]
    if scenario in {"recovery", "cold"}:
        settings["fault_scope"] = "lease-revoke" if variant == "r1" else "checkpoint-cut"
        # DSG access keys are not AWS IAM credentials. Export only a short-lived
        # already-verified deploy session for native DynamoDB/STS readback.
        temporary = json.loads(r.command([
            "aws", "--profile", r.c["deploy_profile"], "--region", r.c["region"],
            "configure", "export-credentials", "--format", "process",
        ], env=r.environment()))
        expires = dt.datetime.fromisoformat(temporary["Expiration"].replace("Z", "+00:00"))
        require(
            temporary.get("Version") == 1
            and temporary.get("AccessKeyId", "").startswith("ASIA")
            and bool(temporary.get("SessionToken"))
            and expires > now() + dt.timedelta(minutes=10),
            "Recovery needs a valid AWS deploy temporary session; do not reuse DSG credentials",
        )
        settings["recovery_aws"] = {
            **{k: temporary[k] for k in
               ("Version", "AccessKeyId", "SecretAccessKey", "SessionToken", "Expiration")},
            "expected_account": r.c["account_id"],
        }
        # A role session may be shorter than the installation's paid window.
        settings["stop_at"] = min(
            dt.datetime.fromisoformat(settings["stop_at"]),
            expires - dt.timedelta(minutes=1), now()+dt.timedelta(minutes=20),
        ).isoformat()
        settings["native"]["stop_at"] = settings["stop_at"]

    if scenario in {"domain", "edge-clone"}:
        t2_id = f"{case_id[:16]}-t2-r{repetition}-{secrets.token_hex(4)}"
        t2_domain = isolation["domains"][1]
        admin(r, "users", {"user_id": t2_id, "default_domain": t2_domain, "logical_limit": 10 << 30})
        t2_keys = admin(r, "credentials", {
            "user_id": t2_id, "system_admin": True, "debug_allowed": True,
        })
        settings["tenant2"] = {
            "user_id": t2_id,
            "bucket": t2_id,
            "domain": t2_domain,
            "credentials": {k: t2_keys[k] for k in ("access_key_id", "secret_access_key")},
        }

    if case.get("comparator") == "direct-s3":
        direct_receipt = direct.owned(r)
        direct_bucket = direct_receipt["bucket"]
        cred_raw = json.loads(
            r.command(
                [
                    "aws",
                    "--profile",
                    r.c["deploy_profile"],
                    "--region",
                    r.c["region"],
                    "configure",
                    "export-credentials",
                    "--format",
                    "process",
                ],
                env=r.environment(),
            )
        )
        # Never adopt an old shared/unknown bucket. Every direct comparator
        # bucket must already be provisioned, tagged and reviewed for this run.
        r.aws("s3api", "head-bucket", "--bucket", direct_bucket,
              "--expected-bucket-owner", r.c["account_id"])
        tags = r.aws("s3api", "get-bucket-tagging", "--bucket", direct_bucket,
                     "--expected-bucket-owner", r.c["account_id"])
        tagmap = {x["Key"]: x["Value"] for x in tags.get("TagSet", [])}
        require(
            tagmap.get("Project") == "Miles"
            and tagmap.get("Environment") == "dev"
            and tagmap.get("AcceptanceRun") == r.id,
            "Direct-S3 bucket missing current-run ownership tags; do not adopt legacy resources",
        )
        settings["direct_s3"] = {
            "bucket": direct_bucket,
            "endpoint": f"https://s3.{r.c['region']}.amazonaws.com",
            "kms_key_id": direct_receipt["kms_key"],
            "credentials": {
                "access_key_id": cred_raw["AccessKeyId"],
                "secret_access_key": cred_raw["SecretAccessKey"],
                "session_token": cred_raw.get("SessionToken", ""),
            },
        }

    r.write(f"{prefix}/input.json", settings)
    if scenario == "cold":
        from deploy.acceptance import cold
        cold.prepare(r, settings)
        return
    remote = "/home/ec2-user/acceptance"

    r.ssh(
        "edge",
        "sudo",
        "-n",
        "python3.12",
        "/opt/miles/current/scripts/edge_host_telemetry.py",
        "probe",
        log=f"{prefix}/probe.json",
    )
    unit = f"miles-{slot_name}"
    r.ssh("edge", "bash", "-c", f"sudo systemctl stop {unit} 2>/dev/null || true; sudo systemctl reset-failed {unit} 2>/dev/null || true")
    r.ssh(
        "edge",
        "sudo",
        "-n",
        "systemd-run",
        "--unit",
        unit,
        "--property=TimeoutStopSec=15",
        "python3.12",
        "/opt/miles/current/scripts/edge_host_telemetry.py",
        "run",
        "--output-file",
        f"/run/miles/{slot_name}.jsonl",
        "--duration",
        "1800",
    )
    time.sleep(5)
    r.ssh("edge", "sudo", "-n", "systemctl", "is-active", unit)
    raw = r.ssh("edge", "sudo", "-n", "cat", f"/run/miles/{slot_name}.jsonl")
    records = [json.loads(x) for x in raw.splitlines()]
    require(
        sum(bool(x.get("chrony")) for x in records) >= 2,
        "Recorder did not acquire growing clock samples",
    )

    r.command(
        [
            "scp",
            "-F",
            r.file("ssh-config"),
            ROOT / "deploy/acceptance/client.py",
            f"client:{remote}/client.py",
        ]
    )
    cloud = cloud_telemetry.start(r, slot_name, secrets.token_hex(8))
    r.write(f"{prefix}/window.json", {"started_at": now().isoformat()})
    try:
        r.ssh(
            "client",
            "python3.12",
            f"{remote}/client.py",
            "benchmark",
            input=json.dumps(settings).encode(),
            timeout=1340,
            log=f"{prefix}/dispatch.log",
        )
    finally:
        try:
            cloud_telemetry.stop(r, cloud)
        finally:
            try:
                collect(r, slot_name)
            finally:
                trial_state.finish(r, slot_name, case, admin)

    result = r.read(f"{prefix}/result.json")
    cloud_telemetry.verify(r, cloud, result["started_at"], result["finished_at"])
    summary = r.read(f"{prefix}/telemetry-summary.json")
    if scenario == "cold":
        receipt = result.get("cold_receipt") or {}
        complete = receipt.get("revival_verified") is True and receipt.get("inventory_after") is not None
        r.write(f"{prefix}/case-verdict.json", {
            "status": "passed" if complete and result.get("status") == "passed" else "incomplete",
            "runner_scope": result.get("scope"), "full_cold_lifecycle_verified": complete,
            "reason": "Restore/GET and inventory alone do not prove PUT/UploadPart revival/protection windows",
        }, new=True)
        require(complete, "Cold read/inventory collected; full Cold lifecycle remains INCOMPLETE without revival proof")
    require(
        result.get("status") == "passed" and summary.get("status") == "OK",
        "Benchmark trial failed or unverified; evidence retained, do not replay blindly",
    )
    r.event(f"benchmark-{slot_name}", "client and telemetry verified")
    print(slot_name)


def collect(r, name=None):
    require(r.file("hosts-outputs.json").exists(), "No hosts to collect")
    prefix = "evidence/" + now().strftime("%Y%m%dT%H%M%S%f")
    r.file(prefix).mkdir(parents=True, mode=0o700)
    for label, args in [
        (
            "gateway",
            [
                "sudo",
                "-n",
                "journalctl",
                "-u",
                "miles.service",
                "-o",
                "json",
                "--no-pager",
            ],
        ),
        ("proxy", ["sudo", "-n", "cat", "/var/log/nginx/miles-trace.jsonl"]),
        ("chrony", ["sudo", "-n", "tar", "-czf", "-", "-C", "/var/log/chrony", "."]),
    ]:
        data = r.ssh("edge", *args, timeout=60)
        r.file(
            prefix + "/" + label + (".tar.gz" if label == "chrony" else ".jsonl")
        ).write_bytes(data)
    if name:
        require(
            re.fullmatch(r"(get-c[14]-[a-f0-9]{8}|[a-z0-9-]+-rep[1-3])", name),
            "Invalid trial name",
        )
        out = "trials/" + name
        # Obtain the remote result without dispatching another request if SSH response was lost.
        result = json.loads(
            r.ssh(
                "client",
                "cat",
                "/home/ec2-user/acceptance/" + name + "/result/result.json",
            )
        )
        r.write(out + "/result.json", result)
        samples = r.ssh(
            "client",
            "tar",
            "-czf",
            "-",
            "-C",
            "/home/ec2-user/acceptance/" + name + "/result",
            ".",
        )
        r.file(out + "/client-result.tar.gz").write_bytes(samples)
        expect_direct = r.read(out + "/input.json").get("direct_s3") is not None
        try:
            direct_s3_raw = r.ssh(
                "client",
                "cat",
                "/home/ec2-user/acceptance/" + name + "/direct-s3/result.json",
                timeout=10,
            )
            direct_result = json.loads(direct_s3_raw)
            require(
                direct_result.get("status") == "passed"
                and direct_result.get("manifest_sha256") == result.get("manifest_sha256"),
                "Direct S3 comparator failed or dataset manifest differs",
            )
            r.write(out + "/direct-s3-result.json", direct_result)
            direct_samples = r.ssh(
                "client",
                "tar",
                "-czf",
                "-",
                "-C",
                "/home/ec2-user/acceptance/" + name + "/direct-s3",
                ".",
                timeout=30,
            )
            r.file(out + "/direct-s3-client-result.tar.gz").write_bytes(direct_samples)
        except Exception as exc:
            if expect_direct:
                raise ValueError("Required direct-S3 paired comparator evidence missing/failed") from exc
        r.ssh("edge", "sudo", "-n", "systemctl", "stop", "miles-" + name)
        raw = r.ssh("edge", "sudo", "-n", "cat", "/run/miles/" + name + ".jsonl")
        r.file(out + "/telemetry.jsonl").write_bytes(raw)
        # Summarizer failure is retained as a verdict, not hidden by a successful workload.
        import shlex

        command = shlex.join(
            [
                "sudo",
                "-n",
                "python3.12",
                "/opt/miles/current/scripts/edge_host_telemetry.py",
                "summarize",
                "--input-file",
                "/run/miles/" + name + ".jsonl",
                "--since",
                result["started_at"],
                "--until",
                result["finished_at"],
            ]
        )
        data = r.command(
            [
                "ssh",
                "-F",
                r.file("ssh-config"),
                "edge",
                command + '; code=$?; test "$code" -le 1',
            ],
            timeout=60,
        )
        r.write(out + "/telemetry-summary.json", json.loads(data))
        r.write(
            out + "/window.json",
            {
                "started_at": result["started_at"],
                "finished_at": result["finished_at"],
                "collected_at": now().isoformat(),
            },
        )
    if r.file("stop-intent.json").exists():
        for metric in (
            "ReadMaxOnDemandThroughputThrottleEvents",
            "ConsumedReadCapacityUnits",
        ):
            data = r.aws(
                "cloudwatch",
                "get-metric-statistics",
                "--namespace",
                "AWS/DynamoDB",
                "--metric-name",
                metric,
                "--dimensions",
                "Name=TableName,Value=miles-dev-Outbox",
                "--start-time",
                r.read("stop-intent.json")["at"],
                "--end-time",
                now().isoformat(),
                "--period",
                "60",
                "--statistics",
                "Sum",
                audit=True,
            )
            r.write(prefix + "/" + metric + ".json", data)
        # Empty/late datapoints stay as recorded; CLI native exceptions remain separate evidence.
    r.event("collect", "private evidence retained at " + prefix)


def stop(r, pulse=False):
    r.identity()
    r.live_window(needed=15)
    require(
        not r.file("stop-intent.json").exists(),
        "Stop already requested; collect/inspect existing invocation",
    )
    if pulse:
        require(
            r.file("latest-snapshot.json").exists(),
            "Freeze and snapshot before native pressure",
        )
    invocation = (
        r.ssh(
            "edge",
            "sudo",
            "-n",
            "systemctl",
            "show",
            "miles.service",
            "-p",
            "InvocationID",
            "--value",
        )
        .decode()
        .strip()
    )
    require(re.fullmatch(r"[a-f0-9]{32}", invocation), "No running Edge invocation")
    r.write(
        "stop-intent.json",
        {"at": now().isoformat(), "invocation": invocation, "native_pulse": pulse},
        new=True,
    )
    r.ssh("edge", "sudo", "-n", "systemctl", "stop", "--no-block", "miles.service")
    if pulse:
        require(
            r.file("latest-snapshot.json").exists(),
            "Capture pending input evidence before pressure",
        )
        r.command(
            [
                "scp",
                "-F",
                r.file("ssh-config"),
                ROOT / "deploy/acceptance/client.py",
                "client:/home/ec2-user/acceptance/client.py",
            ]
        )
        credentials = json.loads(
            r.command(
                [
                    "aws",
                    "--profile",
                    r.c["deploy_profile"],
                    "--region",
                    r.c["region"],
                    "configure",
                    "export-credentials",
                    "--format",
                    "process",
                ],
                env=r.environment(),
            )
        )
        settings = {
            "credentials": credentials,
            "region": r.c["region"],
            "account": r.c["account_id"],
            "table": "miles-dev-Outbox",
            "role_session_prefix": "arn:aws:sts::"
            + r.c["account_id"]
            + ":assumed-role/"
            + r.c["deploy_role_arn"].rsplit("/", 1)[1]
            + "/",
        }
        raw = r.ssh(
            "client",
            "python3.12",
            "/home/ec2-user/acceptance/client.py",
            "pulse",
            input=json.dumps(settings).encode(),
            timeout=160,
            log="pulse-dispatch.log",
        )
        r.write("pulse-result.json", json.loads(raw))
    r.wait(
        "Edge stopped",
        lambda: (
            r.ssh(
                "edge",
                "sudo",
                "-n",
                "systemctl",
                "show",
                "miles.service",
                "-p",
                "MainPID",
                "--value",
            ).strip()
            == b"0"
        ),
        600,
    )
    status = r.ssh(
        "edge",
        "sudo",
        "-n",
        "systemctl",
        "show",
        "miles.service",
        "-p",
        "ExecMainStatus",
        "-p",
        "Result",
    ).decode()
    journal = r.ssh(
        "edge",
        "sudo",
        "-n",
        "journalctl",
        "_SYSTEMD_INVOCATION_ID=" + invocation,
        "-o",
        "cat",
        "--no-pager",
    ).decode()
    private(r.file("stop-journal.log"), journal)
    private(r.file("stop-systemd.txt"), status)
    accepted = (
        "ExecMainStatus=0\n" in status
        and "Result=success\n" in status
        and any(
            json.loads(line).get("msg") == "shutdown complete"
            for line in journal.splitlines()
            if line.startswith("{")
        )
    )
    if pulse:
        accepted = (
            accepted and r.read("pulse-result.json")["counts"]["native_throttle"] > 0
        )
    r.write(
        "stop-proof.json",
        {"accepted": accepted, "invocation": invocation, "at": now().isoformat()},
    )
    collect(r)
    r.event(
        "shutdown",
        "PASS"
        if accepted
        else "FAIL / unexercised native throttle; retain data before cleanup",
    )
    print(json.dumps(r.read("stop-proof.json")))
