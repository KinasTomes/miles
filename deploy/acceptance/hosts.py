"""Pin SSH through authenticated console output, stage and bootstrap AWS hosts."""

import datetime as dt
import base64
import json
import re
import shlex

from deploy.acceptance.common import ROOT, now, private, require
from deploy.acceptance.deployment import provision_material
from deploy.acceptance.prepare import HELPER_SHA


def pin(r):
    r.identity()
    nodes = r.outputs("hosts")["acceptance_nodes"]
    require(set(nodes) == {"edge", "client"}, "Expected two acceptance nodes")
    known = []
    configs = []
    for name, node in nodes.items():
        value = []

        def ready():
            console = r.aws(
                "ec2",
                "get-console-output",
                "--instance-id",
                node["instance_id"],
                "--latest",
            )
            output = console.get("Output", "")
            if "ssh-ed25519 " not in output:
                try:
                    output = base64.b64decode(output, validate=True).decode()
                except (ValueError, UnicodeError):
                    return False
            keys = set(re.findall(r"ssh-ed25519 [A-Za-z0-9+/=]+", output))
            if len(keys) != 1:
                return False
            value.append(next(iter(keys)))
            r.write("hosts/" + name + "-key-proof.json", console)
            return True

        r.wait(name + " console key", ready, 900)
        known.append("acceptance-" + name + " " + value[0])
        configs.append(
            "Host "
            + name
            + "\n  HostName "
            + node["instance_id"]
            + "\n  User ec2-user\n  IdentityFile "
            + str(r.file("operator-key"))
            + "\n  IdentitiesOnly yes\n  StrictHostKeyChecking yes\n  UserKnownHostsFile "
            + str(r.file("known-hosts"))
            + "\n  HostKeyAlias acceptance-"
            + name
            + "\n  ConnectTimeout 20\n  ServerAliveInterval 15\n  ProxyCommand "
            + shlex.join(
                [
                    "aws",
                    "--profile",
                    r.c["deploy_profile"],
                    "--region",
                    r.c["region"],
                    "ssm",
                    "start-session",
                    "--target",
                    "%h",
                    "--document-name",
                    "AWS-StartSSHSession",
                    "--parameters",
                    "portNumber=%p",
                ]
            )
            + "\n"
        )
    private(r.file("known-hosts"), "\n".join(known) + "\n")
    private(r.file("ssh-config"), "\n".join(configs))
    return nodes


def install(r):
    r.identity()
    r.live_window()
    nodes = pin(r)
    out = r.outputs("foundation")
    inventory = out["runtime_inventory"]
    provider = out["edge_provider"]
    require(r.file("assets-proof.json").exists(), "Build/verify assets first")
    common = [
        "release.tar.gz",
        "release.json",
        "assets/host-rpms.tar.gz",
        "assets/host-rpms-manifest.json",
    ]
    # scp flattens these names into the isolated per-host acceptance directory.
    for node in ["edge", "client"]:
        if r.file("hosts/" + node + "-ready.json").exists():
            continue
        require(
            not r.file("hosts/" + node + "-stage-intent.json").exists(),
            "Host stage was already dispatched; reconcile existing state before another install",
        )

        def reachable():
            try:
                r.ssh(node, "true", timeout=30)
                return True
            except ValueError:
                return False

        r.wait(node + " SSH", reachable, 600)
        r.ssh(node, "mkdir", "-m", "700", "-p", "/home/ec2-user/acceptance")
        settings = {
            "node": node,
            "edge_ip": nodes["edge"]["private_ip"],
            "helper_sha256": HELPER_SHA,
        }
        r.write("host-settings.json", settings)
        r.copy(node, common + ["host-settings.json"])
        r.command(
            [
                "scp",
                "-F",
                r.file("ssh-config"),
                ROOT / "deploy/acceptance/host.py",
                node + ":/home/ec2-user/acceptance/host.py",
            ]
        )
        startup = json.loads((ROOT / "configs/shared-edge.example.json").read_text())
        startup.update(
            region=r.c["region"],
            runtime_node_id=nodes[node]["instance_id"],
            utc_reference="169.254.169.123",
            pack_bucket=inventory["data_bucket_name"],
            pack_kms_key_id=inventory["kms_key_arn"],
            runtime_secret_arn=inventory["secret_metadata_arn"],
        )
        if node == "client":
            startup.update(
                runtime_node_id="operator-" + r.id.removeprefix("focused-"),
                local_data_path="/var/lib/miles-bootstrap",
            )
            r.write("operator-startup.json", startup)
            extra = [
                "operator-startup.json",
                "warmbench",
                "coldbench",
                "recoverybench",
                "datasetgen",
                "tls-ca.pem",
            ]
        else:
            r.write("edge-startup.json", startup)
            command = shlex.join(
                [
                    "/usr/local/bin/aws_signing_helper",
                    "credential-process",
                    "--certificate",
                    "/etc/miles/rolesanywhere/certificate.pem",
                    "--private-key",
                    "/etc/miles/rolesanywhere/private-key.pem",
                    "--trust-anchor-arn",
                    provider["trust_anchor_arn"],
                    "--profile-arn",
                    provider["profile_arn"],
                    "--role-arn",
                    inventory["iam_roles"]["edge"],
                    "--session-duration",
                    "900",
                ]
            )
            private(
                r.file("edge-aws-config"),
                "[profile miles-runtime]\nregion = "
                + r.c["region"]
                + "\ncredential_process = "
                + command
                + "\n",
            )
            proxy = (
                (ROOT / "deploy/edge/nginx.conf")
                .read_text()
                .replace("server_name _;", "server_name edge.miles.test;")
            )
            prefix = "pid /run/miles-edge-proxy.pid;\nerror_log /var/log/nginx/miles-error.log;\nevents { worker_connections 128; }\nhttp {\n"
            prefix += 'log_format miles_trace escape=json \'{"time":"$time_iso8601","request_id":"$upstream_http_x_amz_request_id","status":$status,"body_bytes":$body_bytes_sent,"upstream_bytes":"$upstream_response_length","request_seconds":$request_time}\';\naccess_log /var/log/nginx/miles-trace.jsonl miles_trace;\n'
            private(r.file("edge-nginx.conf"), prefix + proxy + "\n}\n")
            private(
                r.file("miles-edge-proxy.service"),
                '[Unit]\nDescription=Miles acceptance Edge TLS\nAfter=miles.service\n[Service]\nType=simple\nExecStart=/usr/sbin/nginx -c /etc/miles/nginx.conf -g "daemon off;"\nExecReload=/usr/sbin/nginx -c /etc/miles/nginx.conf -s reload\nKillSignal=SIGQUIT\nTimeoutStopSec=180\nUMask=0077\n',
            )
            extra = [
                "edge-startup.json",
                "edge-aws-config",
                "edge-nginx.conf",
                "miles-edge-proxy.service",
                "assets/aws_signing_helper",
                "identity.pem",
                "identity-key.pem",
                "edge.pem",
                "edge-key.pem",
            ]
        r.copy(node, extra)
        r.write(
            "hosts/" + node + "-stage-intent.json",
            {"node": nodes[node], "at": now().isoformat()},
            new=True,
        )
        r.ssh(
            node,
            "sudo",
            "-n",
            "python3",
            "/home/ec2-user/acceptance/host.py",
            "stage",
            timeout=900,
            log="hosts/" + node + "-stage.log",
        )
        r.write(
            "hosts/" + node + "-ready.json",
            {"at": now().isoformat(), "node": nodes[node]},
        )
    r.event(
        "install",
        "private hosts staged with signed offline packages, tracing and four-attempt clock profile",
    )


def bootstrap(r):
    r.identity()
    r.live_window()
    require(
        r.file("hosts/edge-ready.json").exists()
        and r.file("hosts/client-ready.json").exists(),
        "Install hosts first",
    )
    require(
        not r.file("bootstrap-intent.json").exists(),
        "Bootstrap already dispatched; reconcile retained token/native state instead of retrying",
    )
    provision_material(r)
    credentials = r.command(
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
    exported = json.loads(credentials)
    require(
        exported.get("Version") == 1
        and exported["AccessKeyId"].startswith("ASIA")
        and dt.datetime.fromisoformat(exported["Expiration"].replace("Z", "+00:00"))
        > now() + dt.timedelta(minutes=3),
        "Bootstrap needs three minutes of temporary credentials; refresh the role before dispatch",
    )
    r.write("bootstrap-intent.json", {"at": now().isoformat()}, new=True)
    r.ssh(
        "client",
        "sudo",
        "-n",
        "python3.12",
        "/home/ec2-user/acceptance/host.py",
        "bootstrap",
        input=credentials,
        timeout=300,
        log="bootstrap.log",
    )
    token = r.ssh(
        "client", "sudo", "-n", "cat", "/home/ec2-user/acceptance/system-admin-token"
    )
    private(r.file("system-admin-token"), token.decode(), new=True)
    gate = r.aws(
        "dynamodb",
        "get-item",
        "--table-name",
        "miles-dev-SystemConfig",
        "--key",
        json.dumps({"config_id": {"S": "coordination/r1/current"}}),
        "--consistent-read",
    )
    require(gate.get("Item"), "Bootstrap success without native R1 gate")
    r.write("bootstrap-gate.json", gate)
    r.write("bootstrap-confirmed.json", {"at": now().isoformat()})
    r.ssh(
        "edge",
        "sudo",
        "-n",
        "systemctl",
        "start",
        "miles.service",
        "miles-edge-proxy.service",
        timeout=120,
        log="edge-start.log",
    )

    def ready():
        try:
            r.ssh(
                "edge",
                "curl",
                "-fsS",
                "--max-time",
                "5",
                "http://127.0.0.1:9100/readyz",
            )
            return True
        except ValueError:
            return False

    r.wait("Edge readiness", ready, 240)
    r.write("edge-ready.json", {"at": now().isoformat(), "source": r.meta["source"]})
    r.event("bootstrap", "native installation and Edge ready; token retained")


def recover_host(r, node):
    r.identity()
    require(
        node in ("edge", "client")
        and r.file("hosts/" + node + "-stage-intent.json").exists(),
        "No unresolved host stage",
    )
    proof = json.loads(
        r.ssh(node, "sudo", "-n", "cat", "/home/ec2-user/acceptance/staged-proof.json")
    )
    require(
        proof["prepared"] == node and proof["source"] == r.meta["source"],
        "Host stage proof mismatch",
    )
    r.write(
        "hosts/" + node + "-ready.json",
        {"at": now().isoformat(), "node": r.outputs("hosts")["acceptance_nodes"][node]},
    )
    r.event("recover-host", "verified remote completion without repeating stage")
