"""Signed data acceptance through pinned SSH/SSM Admin access and verified TLS."""
from contextlib import contextmanager
from urllib.parse import urlencode
from http.client import HTTPConnection
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import secrets
import shlex
import socket
import subprocess
import tempfile
import time
import os
from xml.etree import ElementTree


spec = importlib.util.spec_from_file_location("owner_tunnel", Path(__file__).with_name("admin_tunnel.py"))
tunnel = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tunnel)


@contextmanager
def admin(installation, node):
    if not re.fullmatch(r"i-[a-f0-9]{8,17}", node):
        raise ValueError("Invalid managed instance ID")
    host_key = installation.command(node, "cat /etc/ssh/ssh_host_ed25519_key.pub").strip().split()
    if len(host_key) < 2 or host_key[0] != "ssh-ed25519" or not re.fullmatch(r"[A-Za-z0-9+/=]+", host_key[1]):
        raise ValueError("Invalid pinned host key from trusted SSM channel")
    with tempfile.TemporaryDirectory(prefix="miles-probe-") as directory:
        path = Path(directory)
        known = path / "known-hosts"
        known.write_text(node + " " + " ".join(host_key[:2]) + "\n")
        proxy = shlex.join(["aws", "ssm", "start-session", "--target", node, "--document-name", "AWS-StartSSHSession",
                            "--parameters", "portNumber=22", "--profile", installation.profile, "--region", installation.region])
        config = path / "ssh-config"
        config.write_text("Host " + node + "\n HostName " + node + "\n HostKeyAlias " + node + "\n User miles-owner\n"
                          " IdentityFile " + shlex.quote(str(installation.private / "owner-key")) + "\n IdentitiesOnly yes\n"
                          " UserKnownHostsFile " + shlex.quote(str(known)) + "\n ProxyCommand " + proxy + "\n")
        config.chmod(0o600)
        control, probe = path / "control", path / "http"
        process = subprocess.Popen(tunnel.command(config, node, control), stdin=subprocess.DEVNULL,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   env={k:v for k,v in os.environ.items() if not k.startswith("AWS_")})
        try:
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline and process.poll() is None:
                if tunnel.control(control, node, "check") and tunnel.control(control, node, "forward", (str(probe) + ":127.0.0.1:9000",)):
                    break
                time.sleep(0.25)
            else:
                raise RuntimeError("Admin SSH/SSM forwarding failed")
            token = tunnel.token_file(installation.private / "system-admin-token")
            if not tunnel.healthy(probe, "operator", token):
                raise RuntimeError("Authenticated Admin probe failed")

            def request(method, route, body=None):
                if not route.startswith("/admin/v1/"):
                    raise ValueError("Invalid Admin route")
                connection = HTTPConnection("localhost", timeout=30)
                try:
                    connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                    connection.sock.settimeout(30)
                    connection.sock.connect(str(probe))
                    connection.request(method, route, body=json.dumps(body) if body is not None else None,
                                       headers={"Authorization":"Bearer " + token, "Content-Type":"application/json"})
                    response = connection.getresponse()
                    raw = response.read(2*1024*1024 + 1)
                    if len(raw) > 2*1024*1024:
                        raise ValueError("Admin response exceeds bound")
                    return response.status, json.loads(raw) if raw else {}
                finally:
                    connection.close()
            yield request
        finally:
            tunnel.stop(process)


def signed(installation, credentials, hostname, destination, method, path, output, body=None):
    # Secrets enter curl only through its stdin configuration. Host/SNI stay signed.
    options = {"url":"https://" + hostname + path, "connect-to":hostname + ":443:" + destination + ":443",
               "cacert":str(installation.private / "pki/tls-ca.pem"), "aws-sigv4":"aws:amz:" + installation.region + ":s3",
               "user":credentials["access_key_id"] + ":" + credentials["secret_access_key"], "request":method,
               "output":str(output), "write-out":"%{http_code}", "max-time":"120", "noproxy":"*"}
    if body is not None:
        options["data-binary"] = "@" + str(body)
    configuration = "silent\nshow-error\n" + "\n".join(key + " = " + json.dumps(value) for key, value in options.items()) + "\n"
    result = subprocess.run(["curl", "--config", "-"], input=configuration, capture_output=True, text=True, timeout=150)
    if result.returncode or not result.stdout.strip().isdigit():
        raise RuntimeError("Signed HTTPS transport failed")
    return int(result.stdout.strip())


def verify(installation, nodes, inventory):
    if not installation.state.get("test_user"):
        installation.state["test_user"] = "installation-test-" + secrets.token_hex(6)
        installation.state["test_bucket"] = "installation-test-" + secrets.token_hex(8)
        installation.persist()
    user, bucket = installation.state["test_user"], installation.state["test_bucket"]
    cloud = next(node for node, profile in nodes if profile == "cloud")
    credential_file = installation.private / "test-credential.json"
    with admin(installation, cloud) as request:
        status, _ = request("GET", "/admin/v1/users/" + user)
        if status == 404:
            status, _ = request("POST", "/admin/v1/users", {"user_id":user, "logical_limit":1 << 30})
        if status not in (200, 201):
            raise RuntimeError("Test user provisioning failed")
        if not credential_file.exists():
            # Retries revoke only credentials of this fresh, dedicated test user.
            cursor = None
            while True:
                query = {"user_id":user}
                if cursor:
                    query["cursor"] = cursor
                status, listing = request("GET", "/admin/v1/credentials?" + urlencode(query))
                if status != 200:
                    raise RuntimeError("Cannot inspect test-user credentials")
                for credential in listing["items"]:
                    if credential.get("user_id") != user:
                        raise ValueError("Credential listing crossed the selected test user")
                    status, _ = request("DELETE", "/admin/v1/credentials/" + credential["access_key_id"])
                    if status not in (200, 204):
                        raise RuntimeError("Cannot reconcile test-user credential")
                next_cursor = listing.get("next_cursor")
                if not next_cursor:
                    break
                if next_cursor == cursor:
                    raise ValueError("Credential pagination made no progress")
                cursor = next_cursor
            status, credentials = request("POST", "/admin/v1/credentials", {"user_id":user, "system_admin":False, "debug_allowed":False})
            if status != 201:
                raise RuntimeError("S3 client provisioning failed")
            installation_save(credential_file, credentials)
        credentials = json.loads(credential_file.read_text())
        empty = installation.private / "empty-body"
        empty.write_bytes(b"")
        output = installation.private / "s3-response"
        cloud_host = "cloud." + installation.name + ".test"
        edge_host = "edge." + installation.name + ".test"
        status = signed(installation, credentials, cloud_host, inventory["alb_dns"], "PUT", "/" + bucket, output, empty)
        if status == 409:
            error = ElementTree.fromstring(output.read_bytes())
            if error.findtext("Code") != "BucketAlreadyOwnedByYou":
                raise RuntimeError("Test bucket conflicts with another owner")
        elif status != 200:
            raise RuntimeError("CreateBucket failed")
        status, _ = request("GET", "/admin/v1/buckets/" + bucket + "/policy")
        if status != 200:
            raise RuntimeError("Initial bucket policy was not materialized")
    payload = installation.private / "test-payload.bin"
    if not payload.exists():
        payload.write_bytes(secrets.token_bytes(256*1024))
        payload.chmod(0o600)
    expected = hashlib.sha256(payload.read_bytes()).hexdigest()
    endpoints = [(cloud_host, inventory["alb_dns"]), (edge_host, inventory["edge"]["public_ip"])]
    for index, (hostname, destination) in enumerate(endpoints):
        path = "/" + bucket + "/cross-node-" + str(index)
        if signed(installation, credentials, hostname, destination, "PUT", path, output, payload) != 200:
            raise RuntimeError("Signed PUT failed")
        other_host, other_destination = endpoints[1-index]
        if signed(installation, credentials, other_host, other_destination, "GET", path, output) != 200:
            raise RuntimeError("Cross-node GET failed")
        if hashlib.sha256(output.read_bytes()).hexdigest() != expected:
            raise RuntimeError("Cross-node object hash mismatch")
    installation_save(installation.private / "signed-data-evidence.json", {"source_commit":installation.state["source_commit"],
                      "bucket":bucket, "sha256":expected, "directions":["cloud-to-edge", "edge-to-cloud"], "tls_verified":True}, replace=True)


def installation_save(file, value, replace=False):
    temporary = file.with_name(file.name + ".new") if replace else file
    with open(temporary, "x", opener=lambda path, flags: os.open(path, flags, 0o600)) as stream:
        stream.write(json.dumps(value, indent=2) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    if replace:
        os.replace(temporary, file)
