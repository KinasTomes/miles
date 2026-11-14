#!/usr/bin/env python3
"""Run TestColdLiveFrontDoors against an installation deployed by deploy-installation.py.

Cloud is reached through its ALB and Edge through its EC2 front door, both with
the installation's test CA. Admin is reached only through pinned SSH-over-SSM
tunnels to one Cloud node and the Edge node, bound to 127.0.0.1. The runner
never edits Miles coordination state, deletes jobs or touches processes it did
not start, and writes nothing into the repository.
"""
import argparse
from contextlib import ExitStack, contextmanager
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import socket
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
CREDENTIAL_ENV = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_CREDENTIAL_EXPIRATION")


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def clean_commit():
    if subprocess.run(["git", "status", "--porcelain"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip():
        sys.exit("refusing to run: working tree is not clean, evidence must bind to one commit")
    return subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip()


@contextmanager
def admin_tunnel(installation, node, port):
    """Forward 127.0.0.1:port to the node's loopback Admin over SSH/SSM with a pinned host key."""
    if not re.fullmatch(r"i-[a-f0-9]{8,17}", node):
        raise ValueError("invalid managed instance ID")
    host_key = installation.command(node, "cat /etc/ssh/ssh_host_ed25519_key.pub").strip().split()
    if len(host_key) < 2 or host_key[0] != "ssh-ed25519" or not re.fullmatch(r"[A-Za-z0-9+/=]+", host_key[1]):
        raise ValueError("invalid host key from SSM")
    with tempfile.TemporaryDirectory(prefix="miles-cold-") as directory:
        path = Path(directory)
        (path / "known-hosts").write_text(node + " " + " ".join(host_key[:2]) + "\n")
        proxy = shlex.join(["aws", "ssm", "start-session", "--target", node, "--document-name", "AWS-StartSSHSession",
                            "--parameters", "portNumber=22", "--profile", installation.profile, "--region", installation.region])
        config = path / "ssh-config"
        config.write_text("Host " + node + "\n HostName " + node + "\n HostKeyAlias " + node + "\n User miles-owner\n"
                          " IdentityFile " + shlex.quote(str(installation.private / "owner-key")) + "\n IdentitiesOnly yes\n"
                          " UserKnownHostsFile " + shlex.quote(str(path / "known-hosts")) + "\n StrictHostKeyChecking yes\n"
                          " BatchMode yes\n ExitOnForwardFailure yes\n ServerAliveInterval 15\n ProxyCommand " + proxy + "\n")
        config.chmod(0o600)
        process = subprocess.Popen(["ssh", "-F", str(config), "-N", "-L", f"127.0.0.1:{port}:127.0.0.1:9000", node],
                                   stdin=subprocess.DEVNULL, env={k: v for k, v in os.environ.items() if k not in CREDENTIAL_ENV})
        try:
            deadline = time.monotonic() + 60
            while True:
                if process.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("Admin tunnel to " + node + " failed")
                try:
                    socket.create_connection(("127.0.0.1", port), timeout=1).close()
                    break
                except OSError:
                    time.sleep(0.5)
            yield f"http://127.0.0.1:{port}"
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--config", type=Path, required=True, help="private installation.json")
    parser.add_argument("--private", type=Path, required=True, help="deploy-installation.py private directory")
    parser.add_argument("--result", type=Path, required=True, help="new private result file")
    parser.add_argument("--timeout", default="150m", help="go test timeout; covers the real protection wait")
    args = parser.parse_args()
    if args.result.exists():
        sys.exit(f"refusing to run: result file already exists: {args.result}")
    commit = clean_commit()

    deploy = load("deploy_installation", ROOT / "infra/terraform/cloud/scripts/deploy-installation.py")
    installation = deploy.Installation(args.config.resolve(), args.private)
    installation.identity()
    inventory = installation.inventory()
    nodes = installation.nodes()
    cloud = next((node for node, profile in nodes if profile == "cloud"), None)
    edge = inventory.get("edge") or {}
    if not cloud or not edge.get("instance_id") or not edge.get("public_ip") or not inventory.get("alb_dns"):
        sys.exit("refusing to run: Cloud ALB/ASG and Edge EC2 must be deployed")
    domain = installation.name + ".test"

    with ExitStack() as stack:
        cloud_admin = stack.enter_context(admin_tunnel(installation, cloud, 19001))
        edge_admin = stack.enter_context(admin_tunnel(installation, edge["instance_id"], 19002))
        directory = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix="miles-cold-config-")))
        live = directory / "miles-cold-live.json"
        live.write_text(json.dumps({
            "region": installation.region,
            "installation_file": str(args.config.resolve()),
            "ca_file": str(installation.private / "pki/tls-ca.pem"),
            "token_file": str(installation.private / "system-admin-token"),
            "result_file": str(args.result.resolve()),
            "endpoints": [
                {"name": "cloud", "url": f"https://cloud.{domain}:443", "connect": inventory["alb_dns"] + ":443", "admin": cloud_admin},
                {"name": "edge", "url": f"https://edge.{domain}:443", "connect": edge["public_ip"] + ":443", "admin": edge_admin},
            ],
        }, indent=2))
        live.chmod(0o600)
        print(f"commit={commit} cloud_admin_node={cloud} edge_node={edge['instance_id']}", flush=True)
        env = {k: v for k, v in os.environ.items() if k not in CREDENTIAL_ENV}
        # Only the preflight uses AWS from the test; it runs first, inside this session's lifetime.
        exported = json.loads(subprocess.run(["aws", "configure", "export-credentials", "--profile", installation.profile, "--format", "process"],
                                             capture_output=True, text=True, check=True).stdout)
        env.update(AWS_ACCESS_KEY_ID=exported["AccessKeyId"], AWS_SECRET_ACCESS_KEY=exported["SecretAccessKey"],
                   AWS_SESSION_TOKEN=exported.get("SessionToken", ""), AWS_REGION=installation.region,
                   MILES_COLD_LIVE_CONFIG=str(live), MILES_RUN_COMMIT=commit)
        code = subprocess.run(["go", "test", "-v", "-count=1", "-timeout", args.timeout, "-run", "^TestColdLiveFrontDoors$", "./tests/live"],
                              cwd=ROOT, env=env).returncode

    if not args.result.exists():
        sys.exit("result file was not produced")
    data = json.loads(args.result.read_text())
    print(f"passed={data.get('passed')} observations={len(data.get('observations', []))} admin_events={len(data.get('admin_events', []))}")
    print(f"private result {args.result} sha256={hashlib.sha256(args.result.read_bytes()).hexdigest()}")
    sys.exit(code)


if __name__ == "__main__":
    main()
