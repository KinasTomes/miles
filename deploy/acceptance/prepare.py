"""Build fresh operator inputs without AWS mutations or private historical files."""

import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import urllib.request

from deploy.acceptance.common import (
    ROOT,
    Run,
    config,
    digest,
    load,
    now,
    private,
    require,
    save,
)

IMAGE = "public.ecr.aws/amazonlinux/amazonlinux@sha256:3f09d02367148ef8086b8dc7ce098d16b21da58bbfe1e1167da25a17df8f0e51"
HELPER_URL = "https://rolesanywhere.amazonaws.com/releases/1.8.5/Aarch64/Linux/Amzn2023/aws_signing_helper"
HELPER_SHA = "3d131aa888cd56da446f9c6bb460b1f0569f6c7edc74eae6193a2fe3928883ba"


def init(directory, config_path):
    p = Path(directory).expanduser().resolve()
    require(
        not p.exists() and not p.is_relative_to(ROOT),
        "Choose a new directory outside Git",
    )
    c = config(load(config_path))
    require(
        not subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT).strip(),
        "Commit source before preparing a run",
    )
    p.mkdir(parents=True, mode=0o700)
    revision = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True
    ).strip()
    import secrets

    rid = now().strftime("focused-%Y%m%dT%H%M%SZ-") + secrets.token_hex(3)
    save(p / "config.json", c, new=True)
    save(
        p / "run.json",
        {
            "run_id": rid,
            "source": revision,
            "created_at": now().isoformat(),
            "native_config": str(
                Path(
                    os.environ.get("AWS_CONFIG_FILE", str(Path.home() / ".aws/config"))
                )
                .expanduser()
                .resolve()
            ),
            "native_credentials": str(
                Path(
                    os.environ.get(
                        "AWS_SHARED_CREDENTIALS_FILE",
                        str(Path.home() / ".aws/credentials"),
                    )
                )
                .expanduser()
                .resolve()
            ),
        },
        new=True,
    )
    r = Run(p)
    archive = r.command(["git", "-C", ROOT, "archive", revision])
    source = r.file("source")
    source.mkdir(mode=0o700)
    # git archive contains only reviewed tracked paths; retain its links for Terraform's source tree.
    with tarfile.open(fileobj=io.BytesIO(archive)) as t:
        t.extractall(source, filter="data")
    built = r.command(
        [
            "python3",
            ROOT / "deploy/build-release.py",
            "--arch",
            "arm64",
            "--out",
            r.file("release.tar.gz"),
        ],
        log="release-build.log",
        timeout=600,
    )
    r.write("release.json", json.loads(built))
    for name, arch in [
        ("warmbench", "arm64"),
        ("datasetgen", "arm64"),
    ]:
        env = dict(os.environ, CGO_ENABLED="0")
        if arch:
            env.update(GOOS="linux", GOARCH=arch)
        r.command(
            ["go", "build", "-trimpath", "-o", r.file(name), "./cmd/" + name],
            env=env,
            timeout=600,
        )
    r.write(
        "build-hashes.json",
        {n: digest(r.file(n)) for n in ["release.tar.gz", "warmbench", "datasetgen"]},
    )
    pki(r)
    import shlex

    cli = shutil.which("aws")
    require(cli, "Install AWS CLI v2 before init")
    process = shlex.join(
        [
            "env",
            "AWS_CONFIG_FILE=" + r.native_config,
            "AWS_SHARED_CREDENTIALS_FILE=" + r.native_credentials,
            cli,
            "configure",
            "export-credentials",
            "--profile",
            c["deploy_profile"],
            "--format",
            "process",
            "--region",
            c["region"],
        ]
    )
    auditor = "miles-audit-" + rid.removeprefix("focused-")
    private(
        r.file("aws-config"),
        "[profile acceptance-deploy]\nregion = "
        + c["region"]
        + "\ncredential_process = "
        + process
        + "\n\n"
        + "[profile acceptance-audit]\nrole_arn = arn:aws:iam::"
        + c["account_id"]
        + ":role/miles/acceptance/"
        + auditor
        + "\nsource_profile = acceptance-deploy\nrole_session_name = acceptance-audit\nduration_seconds = 3600\nregion = "
        + c["region"]
        + "\n",
    )
    private(
        r.file("backend.hcl"),
        "\n".join(
            k + " = " + json.dumps(v)
            for k, v in {
                "bucket": c["backend_bucket"],
                "key": c["backend_key"],
                "region": c["region"],
                "profile": "acceptance-deploy",
            }.items()
        )
        + "\nencrypt = true\nuse_lockfile = true\n",
    )
    variables = {
        "account_id": c["account_id"],
        "region": c["region"],
        "availability_zones": c["zones"],
        "runtime_permissions_boundary_arn": c["runtime_boundary_arn"],
        "runtime_secret_name": "miles-dev/runtime-" + rid,
        "paused": True,
        "enable_compute": False,
        "enable_asg": False,
        "enable_alb": False,
        "egress_mode": "interface",
        "alb_internal": True,
        "alb_deregistration_delay_seconds": 300,
        "ami_id": c["ami_id"],
        "fleet_instance_type": "t4g.small",
        "release_architecture": "arm64",
        "enable_edge_private_endpoint": True,
        "edge_ca_pem": r.file("identity-ca.pem").read_text(),
        "client_cidrs": ["10.0.0.0/16"],
        "owner_ssh_public_key": r.file("operator-key.pub").read_text().strip(),
        "enable_test_fault_injection": False,
        "enable_alarms": True,
    }
    if c.get("existing_autoscaling_role_arn"):
        variables["existing_autoscaling_role_arn"] = c["existing_autoscaling_role_arn"]
    r.write("foundation.tfvars.json", variables, new=True)
    r.write(
        "names.json",
        {
            "auditor": auditor,
            "inline": "MilesAcceptance-" + rid,
            "secret": variables["runtime_secret_name"],
        },
    )
    r.event("init", "source and fresh PKI prepared; no AWS calls")
    print(str(p))


def pki(r):
    for name in ["tls", "identity"]:
        r.command(
            [
                "openssl",
                "req",
                "-x509",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-sha256",
                "-days",
                "2",
                "-subj",
                "/CN=Miles acceptance " + name + " CA",
                "-addext",
                "basicConstraints=critical,CA:TRUE",
                "-addext",
                "keyUsage=critical,keyCertSign,cRLSign",
                "-keyout",
                r.file(name + "-ca-key.pem"),
                "-out",
                r.file(name + "-ca.pem"),
            ]
        )
    for name in ["cloud", "edge", "identity"]:
        identity = name == "identity"
        ca = "identity" if identity else "tls"
        r.command(
            [
                "openssl",
                "req",
                "-new",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-sha256",
                "-subj",
                "/CN=" + ("miles-edge" if identity else name + ".miles.test"),
                "-keyout",
                r.file(name + "-key.pem"),
                "-out",
                r.file(name + ".csr"),
            ]
        )
        ext = (
            "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature"
            + ("" if identity else ",keyEncipherment")
            + "\n"
        )
        ext += "extendedKeyUsage=" + ("clientAuth" if identity else "serverAuth") + "\n"
        ext += (
            "subjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n"
            if identity
            else "subjectAltName=DNS:" + name + ".miles.test\n"
        )
        private(r.file(name + ".ext"), ext)
        r.command(
            [
                "openssl",
                "x509",
                "-req",
                "-in",
                r.file(name + ".csr"),
                "-CA",
                r.file(ca + "-ca.pem"),
                "-CAkey",
                r.file(ca + "-ca-key.pem"),
                "-CAcreateserial",
                "-days",
                "2",
                "-sha256",
                "-extfile",
                r.file(name + ".ext"),
                "-out",
                r.file(name + ".pem"),
            ]
        )
        r.command(
            [
                "openssl",
                "verify",
                "-CAfile",
                r.file(ca + "-ca.pem"),
                r.file(name + ".pem"),
            ]
        )
    r.command(
        [
            "ssh-keygen",
            "-t",
            "ed25519",
            "-N",
            "",
            "-C",
            r.id,
            "-f",
            r.file("operator-key"),
        ]
    )


def assets(r):
    target = r.file("assets")
    require(not target.exists(), "Assets already present; verify before reusing")
    target.mkdir(mode=0o700)
    reuse = r.c.get("assets_dir")
    if reuse:
        src = Path(reuse).expanduser().resolve()
        m = load(src / "host-rpms-manifest.json")
        require(
            digest(src / "host-rpms.tar.gz") == m["sha256"]
            and m["source_image"] == IMAGE,
            "Asset cache hash/source mismatch",
        )
        for name in ["host-rpms.tar.gz", "host-rpms-manifest.json"]:
            shutil.copyfile(src / name, target / name)
    else:
        packages = target / "rpms"
        packages.mkdir()
        container = "miles-assets-" + r.id.lower()
        program = "dnf install -y dnf-plugins-core createrepo_c && dnf download --resolve --alldeps --destdir=/out/rpms nginx chrony python3.12 && rpmkeys --checksig /out/rpms/*.rpm && createrepo_c /out/rpms"
        existing = subprocess.run(["docker", "inspect", container], capture_output=True)
        require(
            existing.returncode != 0, "Asset container name already exists; preserve it"
        )
        try:
            r.command(
                [
                    "docker",
                    "run",
                    "--name",
                    container,
                    "--label",
                    "miles.acceptance_run=" + r.id,
                    "--rm",
                    "--platform",
                    "linux/arm64",
                    "-v",
                    str(target) + ":/out",
                    IMAGE,
                    "bash",
                    "-ec",
                    program,
                ],
                timeout=1800,
                log="asset-build.log",
            )
        finally:
            owner = subprocess.run(
                [
                    "docker",
                    "inspect",
                    "--format",
                    '{{index .Config.Labels "miles.acceptance_run"}}',
                    container,
                ],
                capture_output=True,
                text=True,
            )
            if owner.returncode == 0 and owner.stdout.strip() == r.id:
                subprocess.run(
                    ["docker", "rm", "-f", container],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                )
        with tarfile.open(target / "host-rpms.tar.gz", "w:gz") as t:
            for f in sorted(packages.rglob("*")):
                if f.is_file():
                    t.add(f, arcname=str(f.relative_to(packages)), recursive=False)
        save(
            target / "host-rpms-manifest.json",
            {"sha256": digest(target / "host-rpms.tar.gz"), "source_image": IMAGE},
        )
    helper = target / "aws_signing_helper"
    cached = Path(reuse) / "aws_signing_helper" if reuse else None
    if cached and cached.is_file() and digest(cached) == HELPER_SHA:
        shutil.copyfile(cached, helper)
    else:
        with urllib.request.urlopen(HELPER_URL, timeout=60) as source:
            data = source.read(100 * 1024 * 1024)
        helper.write_bytes(data)
    require(
        digest(helper) == HELPER_SHA,
        "Signing helper checksum mismatch; do not stage it",
    )
    helper.chmod(0o700)
    r.write(
        "assets-proof.json",
        {
            "image": IMAGE,
            "helper_url": HELPER_URL,
            "helper_sha256": HELPER_SHA,
            "rpms_sha256": digest(target / "host-rpms.tar.gz"),
        },
    )
    r.event("assets", "verified RPM bundle and pinned signing helper; no AWS calls")


def configure(output, region, deploy_profile, iam_profile, bucket=None, boundary=None):
    """Discover non-secret account inputs through explicitly selected read-only identities."""
    path = Path(output).expanduser().resolve()
    require(
        not path.exists() and not path.is_relative_to(ROOT),
        "Choose a new config file outside Git",
    )

    def aws(profile, service, operation, *args):
        raw = subprocess.check_output(
            [
                "aws",
                "--profile",
                profile,
                "--region",
                region,
                "--no-cli-pager",
                service,
                operation,
                *args,
                "--output",
                "json",
            ],
            timeout=120,
        )
        return json.loads(raw) if raw.strip() else {}

    identity = aws(deploy_profile, "sts", "get-caller-identity")
    account = identity["Account"]
    import re

    match = re.fullmatch(
        r"arn:aws:sts::" + account + r":assumed-role/([^/]+)/[^/]+", identity["Arn"]
    )
    require(match, "Sign in as the deploy role, not an IAM user/root")
    administrator = aws(iam_profile, "sts", "get-caller-identity")
    require(
        administrator["Arn"] == "arn:aws:iam::" + account + ":root",
        "IAM audit profile must be root of the same account",
    )
    role = aws(iam_profile, "iam", "get-role", "--role-name", match[1])["Role"]["Arn"]
    boundary = (
        boundary
        or "arn:aws:iam::" + account + ":policy/miles/bootstrap/MilesDevRuntimeBoundary"
    )
    aws(iam_profile, "iam", "get-policy", "--policy-arn", boundary)
    bucket = bucket or "miles-dev-tfstate-" + account + "-" + region
    aws(
        deploy_profile,
        "s3api",
        "head-bucket",
        "--bucket",
        bucket,
        "--expected-bucket-owner",
        account,
    )
    images = aws(
        deploy_profile,
        "ec2",
        "describe-images",
        "--owners",
        "amazon",
        "--filters",
        "Name=name,Values=al2023-ami-2023.*-kernel-*-arm64",
        "Name=architecture,Values=arm64",
        "Name=root-device-type,Values=ebs",
        "Name=state,Values=available",
    )["Images"]
    require(images, "No available AL2023 ARM64 AMI in this region")
    chosen = max(images, key=lambda i: i["CreationDate"])
    value = config(
        {
            "account_id": account,
            "region": region,
            "deploy_role_arn": role,
            "runtime_boundary_arn": boundary,
            "backend_bucket": bucket,
            "ami_id": chosen["ImageId"],
            "deploy_profile": deploy_profile,
            "iam_profile": iam_profile,
        }
    )
    save(path, value, new=True)
    print(
        "Read-only configuration saved at",
        path,
        "; pinned AMI",
        chosen["ImageId"],
        "review before init/plan",
    )
