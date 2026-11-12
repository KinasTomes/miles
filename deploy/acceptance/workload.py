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


def admin(r, path, body):
    tunnel(r)
    # The tunnel script authenticates and owns its loopback socket before publishing readiness.
    token = r.file("system-admin-token").read_text().strip()
    request = urllib.request.Request(
        "http://127.0.0.1:19001/admin/v1/" + path,
        data=json.dumps(body).encode(),
        headers={
            "Authorization": "Bearer " + token,
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.build_opener(NoRedirect()).open(
        request, timeout=30
    ) as response:
        return json.load(response)


def trial(r, concurrency):
    r.identity()
    r.live_window(needed=25)
    require(concurrency in (1, 4), "Concurrency must be one or four")
    require(r.file("active-outputs.json").exists(), "Apply active stage first")
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
        require(re.fullmatch(r"get-c[14]-[a-f0-9]{8}", name), "Invalid trial name")
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
