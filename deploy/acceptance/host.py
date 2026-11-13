#!/usr/bin/env python3
"""Runs on disposable AWS hosts only. Inputs arrive in a private staged directory."""

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import sys
import tarfile

BASE = Path("/home/ec2-user/acceptance")


def run(args, **kwargs):
    return subprocess.run([str(x) for x in args], check=True, **kwargs)


def unpack(archive, destination, expected, limit):
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
        raise ValueError("Staged archive hash mismatch")
    destination.mkdir(mode=0o700, exist_ok=False)
    with tarfile.open(archive) as t:
        members = t.getmembers()
        if len(members) > 1500 or sum(m.size for m in members) > limit:
            raise ValueError("Archive size limit")
        for m in members:
            name = PurePosixPath(m.name)
            if not m.isfile() or name.is_absolute() or ".." in name.parts:
                raise ValueError("Unsafe archive member")
            target = destination / m.name
            target.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
            target.write_bytes(t.extractfile(m).read())
            target.chmod(0o755 if m.name.startswith("bin/") else 0o600)


def stage():
    settings = json.loads((BASE / "host-settings.json").read_text())
    manifest = json.loads((BASE / "host-rpms-manifest.json").read_text())
    unpack(
        BASE / "host-rpms.tar.gz", BASE / "rpms", manifest["sha256"], 400 * 1024 * 1024
    )
    run(["cloud-init", "status", "--wait"], stdout=subprocess.DEVNULL)
    run(["rpm", "--import", "/etc/pki/rpm-gpg/RPM-GPG-KEY-amazon-linux-2023"])
    run(
        [
            "dnf",
            "--disablerepo=*",
            "--repofrompath=staged,file://" + str(BASE / "rpms"),
            "--setopt=staged.gpgcheck=1",
            "install",
            "-y",
            "nginx",
            "chrony",
            "python3.12",
        ],
        stdout=subprocess.DEVNULL,
    )
    run(["systemctl", "disable", "--now", "nginx"], stdout=subprocess.DEVNULL)
    release = json.loads((BASE / "release.json").read_text())
    unpack(
        BASE / "release.tar.gz", BASE / "bundle", release["sha256"], 100 * 1024 * 1024
    )
    run(
        [
            "install",
            "-d",
            "-m",
            "700",
            "-o",
            "chrony",
            "-g",
            "chrony",
            "/var/log/chrony",
        ]
    )
    run(
        [
            "install",
            "-m",
            "644",
            BASE / "bundle/deploy/chrony/aws-burst.conf",
            "/etc/chrony.conf",
        ]
    )
    run(["chronyd", "-p", "-f", "/etc/chrony.conf"], stdout=subprocess.DEVNULL)
    run(["systemctl", "restart", "chronyd"])
    run(["chronyc", "waitsync", "120", "0.01", "0", "1"])
    if settings["node"] == "edge":
        if subprocess.run(["id", "-u", "miles"], capture_output=True).returncode:
            run(
                [
                    "useradd",
                    "--system",
                    "--home-dir",
                    "/var/lib/miles",
                    "--shell",
                    "/sbin/nologin",
                    "miles",
                ]
            )
        run(
            [
                "install",
                "-d",
                "-m",
                "700",
                "-o",
                "miles",
                "-g",
                "miles",
                "/var/lib/miles",
            ]
        )
        run(
            [
                "python3.12",
                BASE / "bundle/deploy/install.py",
                "--bundle",
                BASE / "bundle",
                "--profile",
                "edge",
                "--address",
                "127.0.0.1",
            ],
            stdout=subprocess.DEVNULL,
        )
        for src, dst in [
            ("edge-startup.json", "startup.json"),
            ("edge-aws-config", "aws-config"),
            ("edge-nginx.conf", "nginx.conf"),
        ]:
            run(["install", "-m", "644", BASE / src, "/etc/miles/" + dst])
        if (
            hashlib.sha256((BASE / "aws_signing_helper").read_bytes()).hexdigest()
            != settings["helper_sha256"]
        ):
            raise ValueError("Signing helper changed")
        run(
            [
                "install",
                "-m",
                "755",
                BASE / "aws_signing_helper",
                "/usr/local/bin/aws_signing_helper",
            ]
        )
        run(["install", "-d", "-m", "750", "-g", "miles", "/etc/miles/rolesanywhere"])
        for src, dst in [
            ("identity.pem", "certificate.pem"),
            ("identity-key.pem", "private-key.pem"),
        ]:
            run(
                [
                    "install",
                    "-m",
                    "640",
                    "-g",
                    "miles",
                    BASE / src,
                    "/etc/miles/rolesanywhere/" + dst,
                ]
            )
        run(["install", "-d", "-m", "700", "/etc/miles/tls"])
        for src, dst in [
            ("edge.pem", "fullchain.pem"),
            ("edge-key.pem", "private-key.pem"),
        ]:
            run(["install", "-m", "600", BASE / src, "/etc/miles/tls/" + dst])
        run(
            [
                "install",
                "-m",
                "644",
                BASE / "miles-edge-proxy.service",
                "/etc/systemd/system/miles-edge-proxy.service",
            ]
        )
        run(["nginx", "-t", "-c", "/etc/miles/nginx.conf"])
        run(["systemctl", "daemon-reload"])
        for src in ["identity-key.pem", "edge-key.pem"]:
            (BASE / src).unlink()
    else:
        import ipaddress

        edge = str(ipaddress.ip_address(settings["edge_ip"]))
        hosts = Path("/etc/hosts")
        text = hosts.read_text()
        if "edge.miles.test" in text:
            raise ValueError("Host alias already set; reconcile installation")
        hosts.write_text(text + "\n" + edge + " edge.miles.test\n")
        for n in ["warmbench", "datasetgen"]:
            (BASE / n).chmod(0o755)
        run(
            [
                "runuser",
                "-u",
                "ec2-user",
                "--",
                BASE / "datasetgen",
                "--profile",
                "backup30",
                "--seed",
                "focused-acceptance",
                "--out",
                BASE / "dataset",
            ]
        )
    proof = {"prepared": settings["node"], "source": release["source_commit"]}
    (BASE / "staged-proof.json").write_text(json.dumps(proof))
    print(json.dumps(proof))


def bootstrap():
    # Operator credentials are never written to disk, environment files or the command line.
    credentials = json.load(sys.stdin)
    expires = dt.datetime.fromisoformat(
        credentials["Expiration"].replace("Z", "+00:00")
    )
    if (
        credentials["Version"] != 1
        or not credentials["AccessKeyId"].startswith("ASIA")
        or expires < dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=2)
    ):
        raise ValueError("Fresh temporary credentials required")
    folder = Path("/run/miles-acceptance-bootstrap")
    folder.mkdir(mode=0o700, exist_ok=False)
    fd = os.memfd_create("acceptance-bootstrap")
    os.fchmod(fd, 0o600)
    os.write(fd, json.dumps(credentials).encode())
    os.lseek(fd, 0, 0)
    cfg = folder / "aws-config"
    startup = folder / "startup.json"
    try:
        settings = json.loads((BASE / "operator-startup.json").read_text())
        settings["aws_config_file"] = str(cfg)
        cfg.write_text(
            "[profile miles-runtime]\nregion = "
            + settings["region"]
            + "\ncredential_process = /usr/bin/cat /proc/"
            + str(os.getpid())
            + "/fd/"
            + str(fd)
            + "\n"
        )
        startup.write_text(json.dumps(settings))
        env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
        env.update(
            AWS_EC2_METADATA_DISABLED="true",
            AWS_CONFIG_FILE=str(cfg),
            AWS_SHARED_CREDENTIALS_FILE="/dev/null",
            AWS_PROFILE="miles-runtime",
            AWS_REGION=settings["region"],
        )
        p = subprocess.run(
            [
                str(BASE / "bundle/bin/dsgctl"),
                "admin-token",
                "bootstrap",
                "--runtime-config",
                str(startup),
                "--user",
                "operator",
                "--ttl",
                "6h",
                "--token-file",
                str(BASE / "system-admin-token"),
            ],
            env=env,
            capture_output=True,
        )
        (BASE / "bootstrap-stdout.log").write_bytes(p.stdout)
        (BASE / "bootstrap-stderr.log").write_bytes(p.stderr)
        (BASE / "bootstrap-status.json").write_text(
            json.dumps({"exit_code": p.returncode})
        )
        if p.returncode:
            raise ValueError(
                "Bootstrap failed or unknown: retain token and reconcile; never reset/replay blindly"
            )
        print("BOOTSTRAP_CONFIRMED")
    finally:
        os.close(fd)
        for f in (cfg, startup):
            f.unlink(missing_ok=True)
        folder.rmdir()


if __name__ == "__main__":
    os.umask(0o077)
    p = argparse.ArgumentParser()
    p.add_argument("action", choices=["stage", "bootstrap"])
    args = p.parse_args()
    {"stage": stage, "bootstrap": bootstrap}[args.action]()
