#!/usr/bin/env python3
"""Install one verified Edge release using its scoped EC2 bootstrap identity."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess


def bootstrap(bundle, config):
    env = {key: value for key, value in os.environ.items() if not key.startswith("AWS_")}
    env.update(AWS_CONFIG_FILE="/dev/null", AWS_SHARED_CREDENTIALS_FILE="/dev/null", AWS_PAGER="")

    def aws(*arguments):
        result = subprocess.run(["aws", *arguments, "--region", config["region"]], env=env,
                                check=True, capture_output=True, text=True, timeout=300)
        return json.loads(result.stdout)

    def run(*arguments):
        subprocess.run(arguments, env=env, check=True, stdout=subprocess.DEVNULL, timeout=300)

    if subprocess.run(["id", "-u", "miles"], capture_output=True).returncode:
        run("useradd", "--system", "--user-group", "--home-dir", "/var/lib/miles", "--shell", "/sbin/nologin", "miles")
    run("install", "-d", "-m", "0700", "-o", "miles", "-g", "miles", "/var/lib/miles")
    run("python3", str(bundle / "deploy/install.py"), "--bundle", str(bundle),
        "--profile", "edge", "--address", "127.0.0.1", "--region", config["region"])
    helper = Path("/usr/local/bin/aws_signing_helper")
    aws("s3api", "get-object", "--bucket", config["bucket"], "--key", config["helper_key"],
        "--version-id", config["helper_version"], "--expected-bucket-owner", config["account_id"], str(helper))
    if hashlib.sha256(helper.read_bytes()).hexdigest() != config["helper_sha256"]:
        helper.unlink()
        raise ValueError("Edge credential helper checksum mismatch")
    helper.chmod(0o755)
    material = json.loads(aws("secretsmanager", "get-secret-value", "--secret-id", config["bootstrap_secret_arn"])["SecretString"])
    for directory in (Path("/etc/miles/tls"), Path("/etc/miles/rolesanywhere")):
        directory.mkdir(mode=0o750, exist_ok=True)
        run("chown", "root:miles", str(directory))
    for name, key, mode in [("tls/fullchain.pem", "tls_certificate", 0o644),
                            ("tls/private-key.pem", "tls_private_key", 0o600),
                            ("rolesanywhere/certificate.pem", "provider_certificate", 0o640),
                            ("rolesanywhere/private-key.pem", "provider_private_key", 0o640)]:
        file = Path("/etc/miles") / name
        file.write_text(material[key])
        file.chmod(mode)
        if name.startswith("rolesanywhere/"):
            run("chown", "root:miles", str(file))
    key = config.get("owner_ssh_public_key")
    if key:
        if subprocess.run(["id", "-u", "miles-owner"], capture_output=True).returncode:
            run("useradd", "--create-home", "--shell", "/bin/bash", "miles-owner")
        ssh = Path("/home/miles-owner/.ssh")
        ssh.mkdir(mode=0o700, exist_ok=True)
        authorized = ssh / "authorized_keys"
        authorized.write_text('restrict,port-forwarding,permitopen="127.0.0.1:9000" ' + key + "\n")
        authorized.chmod(0o600)
        run("chown", "-R", "miles-owner:miles-owner", str(ssh))
        Path("/etc/ssh/sshd_config.d/50-miles-owner.conf").write_text(
            "Match User miles-owner\n AllowTcpForwarding local\n PermitOpen 127.0.0.1:9000\n ForceCommand /usr/bin/false\n PermitTTY no\n AllowAgentForwarding no\n X11Forwarding no\n GatewayPorts no\nMatch all\n")
        run("sshd", "-t")
        run("systemctl", "reload", "sshd")
    aws_config = (
        "[profile miles-runtime]\nregion = " + config["region"] + "\ncredential_process = "
        "/usr/local/bin/aws_signing_helper credential-process --certificate /etc/miles/rolesanywhere/certificate.pem "
        "--private-key /etc/miles/rolesanywhere/private-key.pem --trust-anchor-arn " + config["trust_anchor_arn"]
        + " --profile-arn " + config["profile_arn"] + " --role-arn " + config["role_arn"] + "\n"
    )
    Path("/etc/miles/aws-config").write_text(aws_config)
    startup = dict(profile="edge", mode="runtime", runtime_node_id="edge", region=config["region"],
                   shutdown_seconds=1800, local_data_path="/var/lib/miles", ddb_table_prefix=config["table_prefix"],
                   pack_bucket=config["bucket"], pack_kms_key_id=config["kms_key_arn"],
                   runtime_secret_arn=config["runtime_secret_arn"], aws_config_file="/etc/miles/aws-config",
                   health_address="127.0.0.1:9100", s3_address="127.0.0.1:8080", admin_address="127.0.0.1:9000",
                   utc_reference="169.254.169.123")
    Path("/etc/miles/startup.json").write_text(json.dumps(startup))
    nginx = (bundle / "deploy/edge/nginx.conf").read_text()
    Path("/etc/nginx/nginx.conf").write_text("events {}\nhttp {\n" + nginx + "\n}\n")
    override = Path("/etc/systemd/system/miles.service.d")
    override.mkdir(mode=0o755, exist_ok=True)
    # The serving user cannot access the EC2 bootstrap identity.
    (override / "identity.conf").write_text("[Service]\nIPAddressDeny=169.254.169.254\n")
    proxy_override = Path("/etc/systemd/system/nginx.service.d")
    proxy_override.mkdir(mode=0o755, exist_ok=True)
    (proxy_override / "drain.conf").write_text("[Service]\nKillSignal=SIGQUIT\nTimeoutStopSec=1850\nSendSIGKILL=no\n")
    run("nginx", "-t")
    run("systemctl", "daemon-reload")
    run("systemctl", "enable", "--now", "amazon-ssm-agent", "miles.service", "nginx.service")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--config", type=Path, required=True)
    args = parser.parse_args()
    try:
        bootstrap(args.bundle, json.loads(args.config.read_text()))
    except Exception:
        # Secret material and AWS errors never enter cloud-init output.
        raise SystemExit("Edge bootstrap failed. Inspect private configuration and AWS permissions.") from None
