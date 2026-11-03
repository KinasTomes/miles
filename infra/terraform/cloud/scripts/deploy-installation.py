#!/usr/bin/env python3
"""Prepare, plan and operate a fresh Miles installation using temporary AWS logins."""
import argparse
import base64
import csv
from datetime import datetime, timezone
import fcntl
import hashlib
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tarfile
import time
from urllib.request import urlopen


CLOUD = Path(__file__).resolve().parents[1]
REPO = CLOUD.parents[2]
spec = importlib.util.spec_from_file_location("installation_inputs", CLOUD / "scripts/render-installation.py")
inputs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(inputs)
HELPERS = {
    "arm64": ("Aarch64", "3d131aa888cd56da446f9c6bb460b1f0569f6c7edc74eae6193a2fe3928883ba"),
    "amd64": ("X86_64", "beec9ed1c492d93db809890f16713e3556353294b823c2184ad4e891f1b2b54d"),
}


def read(file):
    return json.loads(file.read_text())


def save(file, value):
    data = json.dumps(value, indent=2) + "\n"
    temporary = file.with_name(file.name + ".new")
    with open(temporary, "x", opener=lambda path, flags: os.open(path, flags, 0o600)) as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.chmod(0o600)
    os.replace(temporary, file)


def run(arguments, **options):
    result = subprocess.run(arguments, capture_output=True, text=True, timeout=options.pop("timeout", 300), **options)
    if result.returncode:
        raise RuntimeError("Command failed. Private state and artifacts have been retained for reconciliation.")
    return result.stdout


class AWSFailure(RuntimeError):
    def __init__(self, code):
        self.code = code
        super().__init__("AWS operation failed: " + code)


class Installation:
    def __init__(self, config, directory):
        self.configuration = read(config)
        inputs.render(self.configuration)  # Validate identities before producing artifacts or calling AWS.
        self.private = directory.resolve()
        if self.private.is_relative_to(REPO) or directory.is_symlink():
            raise ValueError("Installation state must be outside the checkout")
        self.private.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.private.stat().st_mode & 0o077:
            raise ValueError("Installation directory must have mode 0700")
        self.operator = self.configuration["operator"]
        self.region = self.configuration.get("region", "ap-southeast-1")
        self.account = self.configuration["account_id"]
        self.name = self.configuration.get("installation_name", "miles-dev")
        self.profile = self.operator.get("login_profile", "miles-installation")
        self.admin_profile = self.operator.get("admin_profile", "miles-installation-iam")
        self.state_file = self.private / "deployment.json"
        self.state = read(self.state_file) if self.state_file.exists() else {}
        identity_configuration = json.loads(json.dumps(self.configuration))
        for key in ("audit_expires_at", "cleanup_expires_at"):
            identity_configuration["operator"].pop(key, None)
        digest = hashlib.sha256(json.dumps(identity_configuration, sort_keys=True).encode()).hexdigest()
        if self.state.get("configuration_sha256", digest) != digest:
            raise ValueError("Configuration changed. Review and reconcile the existing installation before replacing its inputs")
        self.state["configuration_sha256"] = digest

    def persist(self):
        save(self.state_file, self.state)

    def aws(self, *arguments, admin=False):
        profile = self.admin_profile if admin else self.profile
        env = {k:v for k,v in os.environ.items() if not k.startswith("AWS_")}
        result = subprocess.run(["aws", *arguments, "--profile", profile, "--region", self.region, "--output", "json", "--no-cli-pager"], env=env, capture_output=True, text=True, timeout=300)
        if result.returncode:
            match = re.search(r"An error occurred \(([A-Za-z0-9_.-]+)\)", result.stderr)
            raise AWSFailure(match[1] if match else "Unclassified")
        raw = result.stdout
        return json.loads(raw) if raw.strip() else {}

    def identity(self, admin=False):
        identity = self.aws("sts", "get-caller-identity", admin=admin)
        if identity["Account"] != self.account:
            raise ValueError("AWS account differs from installation configuration")
        if not admin:
            role = self.operator["deploy_role_arn"].rsplit("/", 1)[-1]
            prefix = f"arn:aws:sts::{self.account}:assumed-role/{role}/"
            if not identity["Arn"].startswith(prefix):
                raise ValueError("Workload requires the configured deploy-role session")
        return identity

    def terraform(self, stack, *arguments, admin=False):
        self.identity(admin)
        credentials = self.aws("configure", "export-credentials", "--format", "process", admin=admin)
        env = {k:v for k,v in os.environ.items() if not k.startswith("AWS_")}
        env.update(AWS_ACCESS_KEY_ID=credentials["AccessKeyId"], AWS_SECRET_ACCESS_KEY=credentials["SecretAccessKey"],
                   AWS_SESSION_TOKEN=credentials["SessionToken"], AWS_REGION=self.region, AWS_DEFAULT_REGION=self.region,
                   AWS_CONFIG_FILE="/dev/null", AWS_SHARED_CREDENTIALS_FILE="/dev/null", TF_IN_AUTOMATION="1")
        return run(["terraform", "-chdir=" + str(stack), *arguments], env=env, timeout=3600)

    def stack(self, stage):
        paths = {"operator": self.private / "operator", "state": self.private / "state"}
        if stage in paths:
            return paths[stage]
        return self.private / ("cloud-cleanup" if stage in ("unprotect", "destroy") else "cloud") / "environments/dev"

    def prepare(self):
        if self.state.get("prepared"):
            return
        for executable in ("aws", "terraform", "openssl", "go", "chronyc", "curl", "ssh", "ssh-keygen", "session-manager-plugin"):
            if not shutil.which(executable):
                raise ValueError("Install required operator tool: " + executable)
        reference = ipaddress.ip_address(self.operator["utc_reference"])
        if reference.version != 4 or reference.is_loopback or reference.is_unspecified:
            raise ValueError("operator.utc_reference must be an approved chrony IPv4 source")
        tracking = next(csv.reader([run(["chronyc", "-n", "-c", "-h", "127.0.0.1", "tracking"]).strip()]))
        if len(tracking) != 14 or tracking[1] != str(reference) or tracking[13] != "Normal":
            raise ValueError("Synchronize operator chrony with operator.utc_reference before planning resources")
        # Preparation stops before cloud resources if source changes are not committed.
        if run(["git", "status", "--porcelain"], cwd=REPO).strip():
            raise ValueError("Prepare from a clean committed checkout")
        limits = self.configuration.get("deployment", {})
        if not limits.get("max_cost_usd") or not limits.get("max_active_minutes"):
            raise ValueError("Set the cost envelope and maximum active minutes in deployment configuration")
        owner_key = self.private / "owner-key"
        if not owner_key.exists():
            run(["ssh-keygen", "-t", "ed25519", "-N", "", "-f", str(owner_key), "-q"])
        pki = self.private / "pki"
        pki.mkdir(mode=0o700, exist_ok=True)
        def openssl(*arguments):
            run(["openssl", *arguments], cwd=pki)
        for authority in ("tls", "identity"):
            key, certificate = pki / (authority + "-ca.key"), pki / (authority + "-ca.pem")
            if not key.exists():
                openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "30", "-sha256",
                        "-keyout", str(key), "-out", str(certificate), "-subj", "/CN=" + self.name + "-" + authority,
                        "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign")
            elif not certificate.exists():
                raise ValueError("Incomplete CA artifacts. Reconcile the private PKI directory")
        for leaf, authority, cn in [("cloud", "tls", "cloud." + self.name + ".test"),
                                    ("edge", "tls", "edge." + self.name + ".test"),
                                    ("provider", "identity", self.configuration.get("edge_certificate_common_name", "miles-edge"))]:
            if (pki / (leaf + ".pem")).exists():
                continue
            extension = pki / (leaf + ".ext")
            extension.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n" +
                                 ("extendedKeyUsage=clientAuth\n" if leaf == "provider" else "extendedKeyUsage=serverAuth\nsubjectAltName=DNS:" + cn + "\n"))
            openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-keyout", leaf + ".key", "-out", leaf + ".csr", "-subj", "/CN=" + cn)
            openssl("x509", "-req", "-in", leaf + ".csr", "-CA", authority + "-ca.pem", "-CAkey", authority + "-ca.key",
                    "-set_serial", str(secrets.randbits(120)), "-days", "14", "-sha256", "-extfile", leaf + ".ext", "-out", leaf + ".pem")
        for file in pki.iterdir():
            file.chmod(0o600)
        configuration = dict(self.configuration, owner_ssh_public_key=(self.private / "owner-key.pub").read_text().strip(), edge_ca_pem=(pki / "identity-ca.pem").read_text(),
                             paused=True, enable_compute=False, enable_asg=False, enable_edge=False, enable_alb=False)
        if not (self.private / "rendered").exists():
            inputs.write(configuration, self.private / "rendered")
        elif read(self.private / "rendered/installation.tfvars.json") != inputs.render(configuration)["installation.tfvars.json"]:
            raise ValueError("Prepared inputs differ from the retained artifacts")
        for name in ("operator", "state"):
            destination = self.private / name
            destination.mkdir(mode=0o700, exist_ok=True)
            for file in (CLOUD / "bootstrap" / name).glob("*.tf"):
                shutil.copyfile(file, destination / file.name)
            shutil.copyfile(CLOUD / "bootstrap" / name / ".terraform.lock.hcl", destination / ".terraform.lock.hcl")
        shutil.copytree(CLOUD, self.private / "cloud", dirs_exist_ok=True, ignore=shutil.ignore_patterns(".terraform", "terraform.tfstate*", "*.tfplan", "__pycache__"))
        self.state.update(prepared=True, source_commit=run(["git", "rev-parse", "HEAD"], cwd=REPO).strip())
        self.persist()

    def variable_file(self, stage):
        if stage == "operator":
            return self.private / "rendered/operator.tfvars.json"
        if stage == "state":
            return self.private / "rendered/bootstrap-state.tfvars.json"
        if stage == "foundation":
            return self.private / "rendered/installation.tfvars.json"
        return self.private / "active.tfvars.json"

    def initialize(self, stage):
        arguments = ["init", "-input=false", "-reconfigure", "-lockfile=readonly"]
        if stage not in ("operator", "state"):
            # Use exported temporary credentials for both backend and provider.
            backend = self.private / "backend.hcl"
            backend.write_text("\n".join(line for line in (self.private / "rendered/backend.hcl").read_text().splitlines() if not line.startswith("profile =")) + "\n")
            backend.chmod(0o600)
            arguments.append("-backend-config=" + str(backend))
        self.terraform(self.stack(stage), *arguments, admin=stage == "operator")

    def plan(self, stage):
        if not self.state.get("prepared"):
            raise ValueError("Run prepare first")
        if self.state.get("destroy_applied") and stage != "operator":
            raise ValueError("This runtime was destroyed. Use a new installation for another run")
        if stage == "foundation" and self.state.get("provisioned"):
            raise ValueError("Foundation inputs cannot replace a provisioned runtime")
        if stage == "active" and self.state.get("unprotect_applied"):
            raise ValueError("Restore datastore protections before resuming an unprotected runtime")
        prerequisites = {"state":"operator_applied", "foundation":"state_applied", "active":"provisioned", "paused":"active_applied"}
        if stage in prerequisites and not self.state.get(prerequisites[stage]):
            raise ValueError("Complete the prerequisite installation stage first")
        if stage == "destroy" and not self.state.get("unprotect_applied"):
            raise ValueError("Apply the reviewed protection update before creating the destroy plan")
        if stage in ("unprotect", "destroy"):
            self.prepare_cleanup()
        if stage == "operator":
            configuration = dict(self.configuration, edge_ca_pem=(self.private / "pki/identity-ca.pem").read_text())
            save(self.private / "rendered/operator.tfvars.json", inputs.render(configuration)["operator.tfvars.json"])
        self.initialize(stage)
        file = self.private / (stage + ".tfplan")
        arguments = ["plan", "-input=false", "-lock-timeout=60s", "-var-file=" + str(self.variable_file(stage)), "-out=" + str(file)]
        if stage in ("paused", "unprotect", "destroy"):
            arguments.append("-var=paused=true")
        if stage == "destroy":
            arguments.append("-destroy")
        self.terraform(self.stack(stage), *arguments, admin=stage == "operator")
        plan = json.loads(self.terraform(self.stack(stage), "show", "-json", str(file), admin=stage == "operator"))
        if (not str(plan.get("format_version", "")).startswith("1.") or plan.get("complete") is not True
                or plan.get("errored") is not False or plan.get("deferred_changes")
                or any(check.get("status") != "pass" for check in plan.get("checks", []))):
            raise ValueError("Require a complete, successful plan with no deferred checks")
        if stage == "operator" and any(r.get("mode") == "managed" and not r["type"].startswith("aws_iam_") for r in plan.get("resource_changes", [])):
            raise ValueError("IAM bootstrap plan contains a workload resource")
        if stage == "paused":
            guard_spec = importlib.util.spec_from_file_location("session_guard", CLOUD / "scripts/check-session-plan.py")
            guard = importlib.util.module_from_spec(guard_spec)
            guard_spec.loader.exec_module(guard)
            guard.check_plan(plan, "pause", self.account, self.region, self.name, self.configuration["runtime_permissions_boundary_arn"])
        if stage in ("unprotect", "destroy"):
            for resource in plan.get("resource_changes", []):
                if resource.get("mode") != "managed" or resource["change"]["actions"] == ["no-op"]:
                    continue
                change = resource["change"]
                if stage == "destroy":
                    if change["actions"] != ["delete"]:
                        raise ValueError("Teardown plan must only delete the installation runtime graph")
                else:
                    before, after = dict(change["before"]), dict(change["after"])
                    if (resource["type"] != "aws_dynamodb_table" or change["actions"] != ["update"]
                            or before.pop("deletion_protection_enabled", None) is not True
                            or after.pop("deletion_protection_enabled", None) is not False or before != after):
                        raise ValueError("Protection plan must only disable DynamoDB deletion protection")
        save(self.private / (stage + "-plan-details.json"), plan)
        digest = hashlib.sha256(file.read_bytes()).hexdigest()
        descriptor = {"stage": stage, "sha256": digest, "configuration_sha256": self.state["configuration_sha256"],
                      "changes": [{"address": r["address"], "actions": r["change"]["actions"]} for r in plan.get("resource_changes", [])]}
        save(self.private / (stage + "-plan.json"), descriptor)
        print(json.dumps(descriptor, indent=2))

    def apply(self, stage, expected_hash, delete_data=False):
        file = self.private / (stage + ".tfplan")
        descriptor = read(self.private / (stage + "-plan.json"))
        if hashlib.sha256(file.read_bytes()).hexdigest() != expected_hash or descriptor["sha256"] != expected_hash or descriptor["configuration_sha256"] != self.state["configuration_sha256"]:
            raise ValueError("Saved plan hash or installation inputs differ from the reviewed plan")
        self.identity(stage == "operator")
        if stage == "active":
            if not self.state.get("active_started_at"):
                self.state["active_started_at"] = datetime.now(timezone.utc).isoformat()
                self.persist()
            started = datetime.fromisoformat(self.state["active_started_at"])
            if (datetime.now(timezone.utc) - started).total_seconds() >= self.configuration["deployment"]["max_active_minutes"] * 60:
                raise ValueError("Active time window elapsed. Only drain/pause/cleanup may continue")
        applied = self.state.setdefault("applied_plans", {})
        if applied.get(stage) != expected_hash or (stage != "operator" and self.state.get("last_workload_stage") != stage):
            if stage in ("paused", "unprotect", "destroy"):
                self.drain()
            if stage == "destroy":
                if not delete_data or not self.operator.get("cleanup_expires_at"):
                    raise ValueError("Destroy requires an approved cleanup grant and --delete-data")
                self.empty_bucket()
            self.terraform(self.stack(stage), "apply", "-input=false", "-lock-timeout=60s", str(file), admin=stage == "operator")
            applied[stage] = expected_hash
            self.state[stage + "_applied"] = True
            if stage != "operator":
                self.state["last_workload_stage"] = stage
            if stage == "active":
                self.state.update(paused_applied=False, unprotect_applied=False)
            elif stage == "paused":
                self.state.update(active_applied=False, unprotect_applied=False)
            self.persist()
        if stage == "operator":
            self.create_login()
        if stage == "destroy" and self.state.get("certificate_arn"):
            try:
                self.aws("acm", "delete-certificate", "--certificate-arn", self.state["certificate_arn"])
            except AWSFailure as error:
                if error.code != "ResourceNotFoundException":
                    raise

    def create_login(self):
        user = self.operator["user_arn"].rsplit("/", 1)[-1]
        profiles = self.aws("iam", "get-user", "--user-name", user, admin=True)
        if profiles["User"]["Arn"] != self.operator["user_arn"]:
            raise ValueError("Operator ARN differs from the planned identity")
        # Never reset an existing Console login during a retry.
        try:
            self.aws("iam", "get-login-profile", "--user-name", user, admin=True)
            return
        except AWSFailure as error:
            if error.code != "NoSuchEntity":
                raise
        file = self.private / "console-login.json"
        if not file.exists():
            save(file, {"UserName": user, "Password": secrets.token_urlsafe(32) + "aA1!", "PasswordResetRequired": True})
        self.aws("iam", "create-login-profile", "--cli-input-json", "file://" + str(file), admin=True)
        print("Console login written to private console-login.json. Enroll MFA, then switch to the configured deploy role.")

    def inventory(self):
        values = json.loads(self.terraform(self.stack("active"), "output", "-json", "runtime_inventory"))
        save(self.private / "runtime-inventory.json", values)
        return values

    def secret(self, arn, value, name):
        file = self.private / (name + ".json")
        if not file.exists():
            save(file, value)
        proposed = read(file)
        versions = self.aws("secretsmanager", "describe-secret", "--secret-id", arn).get("VersionIdsToStages", {})
        if versions:
            current = json.loads(self.aws("secretsmanager", "get-secret-value", "--secret-id", arn)["SecretString"])
            if current != proposed:
                raise ValueError("Existing secret differs. Reconcile without rotating installation keys")
            return
        token_file = self.private / (name + "-request.json")
        if not token_file.exists():
            save(token_file, {"token": secrets.token_hex(16)})
        self.aws("secretsmanager", "put-secret-value", "--secret-id", arn, "--secret-string", "file://" + str(file),
                 "--client-request-token", read(token_file)["token"])

    def upload(self, inventory, file, key):
        bucket = inventory["data_bucket_name"]
        # A pre-existing release is read back and compared before reusing its version.
        try:
            head = self.aws("s3api", "head-object", "--bucket", bucket, "--key", key)
        except AWSFailure as error:
            if error.code not in ("404", "NoSuchKey", "NotFound"):
                raise
        else:
            if not head.get("VersionId") or head["VersionId"] == "null":
                raise ValueError("Existing artifact is not versioned")
            existing = self.private / "readback.bin"
            self.aws("s3api", "get-object", "--bucket", bucket, "--key", key, "--version-id", head["VersionId"], str(existing))
            if hashlib.sha256(existing.read_bytes()).digest() != hashlib.sha256(file.read_bytes()).digest():
                raise ValueError("Pinned artifact differs from the existing S3 object")
            existing.unlink()
            return head["VersionId"]
        output = self.aws("s3api", "put-object", "--bucket", bucket, "--key", key, "--body", str(file),
                          "--if-none-match", "*", "--server-side-encryption", "aws:kms", "--ssekms-key-id", inventory["kms_key_arn"])
        if not output.get("VersionId") or output["VersionId"] == "null":
            raise ValueError("Artifact upload did not return a version ID")
        return output["VersionId"]

    def check_source(self):
        if (run(["git", "rev-parse", "HEAD"], cwd=REPO).strip() != self.state.get("source_commit")
                or run(["git", "status", "--porcelain"], cwd=REPO).strip()):
            raise ValueError("Use the clean source commit pinned by prepare")

    def provision(self):
        self.check_source()
        self.identity()
        if not self.state.get("foundation_applied") or self.state.get("destroy_applied"):
            raise ValueError("Provision requires a retained foundation")
        inventory = self.inventory()
        pki = self.private / "pki"
        self.secret(inventory["secret_metadata_arn"], {"key_id": "initial", "seal_key": base64.b64encode(secrets.token_bytes(32)).decode(),
                    "cursor_key": base64.b64encode(secrets.token_bytes(32)).decode()}, "runtime-keys")
        edge_secret = {"tls_certificate": (pki / "edge.pem").read_text() + (pki / "tls-ca.pem").read_text(),
                       "tls_private_key": (pki / "edge.key").read_text(), "provider_certificate": (pki / "provider.pem").read_text(),
                       "provider_private_key": (pki / "provider.key").read_text()}
        self.secret(inventory["edge_bootstrap_secret_arn"], edge_secret, "edge-material")
        token = self.private / "system-admin-token"
        if not self.state.get("metadata_provisioned"):
            if token.exists():
                raise ValueError("Bootstrap token already exists. Reconcile the uncertain metadata outcome before retrying")
            aws_binary = shutil.which("aws")
            profile = self.private / "operator-aws-config"
            profile.write_text('[profile miles-runtime]\ncredential_process = "' + aws_binary + '" configure export-credentials --profile ' + self.profile + ' --format process --region ' + self.region + '\n')
            profile.chmod(0o600)
            startup = {"profile":"edge", "mode":"runtime", "runtime_node_id":"operator", "region":self.region,
                       "s3_address":"127.0.0.1:8080", "admin_address":"127.0.0.1:9000", "health_address":"127.0.0.1:9100",
                       "shutdown_seconds":1800, "local_data_path":str(self.private / "operator-data"),
                       "ddb_table_prefix":self.name + "-", "pack_bucket":inventory["data_bucket_name"],
                       "pack_kms_key_id":inventory["kms_key_arn"], "runtime_secret_arn":inventory["secret_metadata_arn"],
                       "aws_config_file":str(profile), "utc_reference":self.operator["utc_reference"]}
            save(self.private / "operator-startup.json", startup)
            run(["make", "build"], cwd=REPO)
            run([str(REPO / "bin/dsgctl"), "admin-token", "bootstrap", "--runtime-config", str(self.private / "operator-startup.json"),
                 "--user", "operator", "--token-file", str(token)], timeout=300,
                env={key:value for key,value in os.environ.items() if not key.startswith("AWS_")})
            self.state["metadata_provisioned"] = True
            self.persist()
        architecture = self.configuration.get("release_architecture", "arm64")
        archive = self.private / ("miles-linux-" + architecture + ".tar.gz")
        if not archive.exists():
            run(["python3", "-B", str(REPO / "deploy/build-release.py"), "--arch", architecture, "--out", str(archive)])
        with tarfile.open(archive, "r:gz") as contents:
            manifest = json.load(contents.extractfile("manifest.json"))
            if manifest["source_commit"] != self.state["source_commit"] or manifest["arch"] != architecture:
                raise ValueError("Release belongs to another source commit or architecture")
            for name, expected in manifest["files"].items():
                if hashlib.sha256(contents.extractfile(name).read()).hexdigest() != expected:
                    raise ValueError("Release manifest validation failed")
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        version = self.upload(inventory, archive, "releases/" + digest + "/" + archive.name)
        platform, helper_sha = HELPERS[architecture]
        helper = self.private / "aws_signing_helper"
        if not helper.exists():
            url = "https://rolesanywhere.amazonaws.com/releases/1.8.5/" + platform + "/Linux/Amzn2023/aws_signing_helper"
            with urlopen(url, timeout=60) as response:
                helper.write_bytes(response.read(50*1024*1024 + 1))
            helper.chmod(0o600)
        if hashlib.sha256(helper.read_bytes()).hexdigest() != helper_sha:
            raise ValueError("Credential helper checksum mismatch")
        helper_version = self.upload(inventory, helper, "deployment/helper/" + helper_sha + "/aws_signing_helper")
        self.import_certificate()
        active = read(self.private / "rendered/installation.tfvars.json")
        active.update(paused=False, enable_asg=True, enable_alb=True, enable_edge=True,
                      fleet_instance_type=self.configuration.get("fleet_instance_type", self.configuration.get("instance_type", "t4g.small")),
                      certificate_arn=self.state["certificate_arn"], release_sha256=digest, release_version_id=version,
                      edge_helper_sha256=helper_sha, edge_helper_version_id=helper_version)
        save(self.private / "active.tfvars.json", active)
        self.state["provisioned"] = True
        self.persist()
        print("Backend provisioned and immutable Cloud/Edge artifacts published. Plan active next.")

    def import_certificate(self):
        if self.state.get("certificate_arn"):
            return
        pki = self.private / "pki"
        expected = (pki / "cloud.pem").read_text().strip()
        candidates = self.aws("acm", "list-certificates", "--certificate-statuses", "ISSUED")["CertificateSummaryList"]
        for candidate in candidates:
            arn = candidate["CertificateArn"]
            if candidate.get("DomainName") != "cloud." + self.name + ".test":
                continue
            tags = {t["Key"]:t["Value"] for t in self.aws("acm", "list-tags-for-certificate", "--certificate-arn", arn)["Tags"]}
            if tags.get("Installation") == self.name and tags.get("Project") == "Miles":
                material = self.aws("acm", "get-certificate", "--certificate-arn", arn)
                if material["Certificate"].strip() == expected:
                    self.state["certificate_arn"] = arn
                    self.persist()
                    return
        if self.state.get("certificate_import_pending"):
            raise ValueError("Certificate import outcome uncertain. Reconcile ACM before retrying")
        self.state["certificate_import_pending"] = True
        self.persist()
        certificate = self.aws("acm", "import-certificate", "--certificate", "fileb://" + str(pki / "cloud.pem"),
                               "--private-key", "fileb://" + str(pki / "cloud.key"), "--certificate-chain", "fileb://" + str(pki / "tls-ca.pem"),
                               "--tags", "Key=Installation,Value=" + self.name, "Key=Project,Value=Miles")
        self.state["certificate_arn"] = certificate["CertificateArn"]
        self.persist()

    def closeout(self):
        if not self.state.get("destroy_applied"):
            raise ValueError("Apply the approved destroy plan before closeout")
        self.identity()
        inventory = read(self.private / "runtime-inventory.json")
        managed = json.loads(self.terraform(self.stack("destroy"), "show", "-json")).get("values", {}).get("root_module", {})
        def resources(module):
            return module.get("resources", []) + [r for child in module.get("child_modules", []) for r in resources(child)]
        if any(r.get("mode") == "managed" for r in resources(managed)):
            raise ValueError("Runtime Terraform state still contains managed resources")
        checks = [
            ("ec2", "describe-instances", "Reservations", ["--filters", "Name=tag:Installation,Values=" + self.name, "Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down"]),
            ("ec2", "describe-volumes", "Volumes", ["--filters", "Name=tag:Installation,Values=" + self.name]),
            ("ec2", "describe-vpcs", "Vpcs", ["--filters", "Name=tag:Installation,Values=" + self.name]),
            ("autoscaling", "describe-auto-scaling-groups", "AutoScalingGroups", ["--auto-scaling-group-names", self.name + "-gateway"]),
        ]
        for service, operation, key, arguments in checks:
            if self.aws(service, operation, *arguments)[key]:
                raise ValueError("Live runtime resources remain. Reconcile before closeout")
        for service, operation, arguments, code in [
            ("s3api", "head-bucket", ["--bucket", inventory["data_bucket_name"]], "404"),
            *[("dynamodb", "describe-table", ["--table-name", table], "ResourceNotFoundException") for table in inventory["expected_tables"]],
        ]:
            try:
                self.aws(service, operation, *arguments)
            except AWSFailure as error:
                if error.code != code:
                    raise
            else:
                raise ValueError("A datastore remains after teardown")
        key = self.aws("kms", "describe-key", "--key-id", inventory["kms_key_arn"])["KeyMetadata"]
        if key["KeyState"] != "PendingDeletion":
            raise ValueError("Runtime KMS key is not pending deletion")
        for arn in (inventory["secret_metadata_arn"], inventory["edge_bootstrap_secret_arn"]):
            if not self.aws("secretsmanager", "describe-secret", "--secret-id", arn).get("DeletedDate"):
                raise ValueError("Runtime secret is not pending deletion")
        save(self.private / "cleanup-evidence.json", {"source_commit":self.state["source_commit"],
             "runtime_state_empty":True, "instances_volumes_vpcs_absent":True, "datastores_absent":True,
             "kms_secrets_pending_deletion":True, "bootstrap_state_retained":True})
        print("Runtime cleanup verified. Remove cleanup_expires_at and review/apply the operator plan to revoke cleanup permissions.")

    def nodes(self):
        inventory = self.inventory()
        groups = self.aws("autoscaling", "describe-auto-scaling-groups", "--auto-scaling-group-names", self.name + "-gateway")["AutoScalingGroups"]
        result = [(node["InstanceId"], "cloud") for group in groups for node in group["Instances"]]
        edge = inventory.get("edge")
        if edge and edge.get("instance_id"):
            result.append((edge["instance_id"], "edge"))
        return result

    def command(self, node, program, timeout=120):
        parameters = self.private / "command-parameters.json"
        save(parameters, {"commands":[program], "executionTimeout":[str(timeout)]})
        output = self.aws("ssm", "send-command", "--instance-ids", node, "--document-name", "AWS-RunShellScript", "--parameters", "file://" + str(parameters))
        command = output["Command"]["CommandId"]
        deadline = time.monotonic() + timeout + 60
        while time.monotonic() < deadline:
            result = self.aws("ssm", "list-command-invocations", "--command-id", command, "--details")["CommandInvocations"]
            if result and result[0]["Status"] == "Success":
                return result[0]["CommandPlugins"][0]["Output"]
            if result and result[0]["Status"] in ("Failed", "TimedOut", "Cancelled"):
                raise RuntimeError("Node operation failed. Inspect the SSM command in the installation account")
            time.sleep(3)
        raise RuntimeError("SSM operation has not settled; preserve the installation and reconcile")

    def verify(self):
        self.check_source()
        self.identity()
        nodes = self.nodes()
        if len(nodes) != 3:
            raise ValueError("Expected two Cloud nodes and one Edge node")
        proof = []
        for node, profile in nodes:
            address = "127.0.0.1" if profile == "edge" else "$(hostname -I | awk '{print $1}')"
            output = self.command(node, "curl --fail --silent --max-time 10 http://" + address + ":9100/readyz && systemctl is-active miles" + (" nginx -q" if profile == "edge" else " -q"))
            proof.append({"node":node, "profile":profile, "readiness":True, "output":output})
        inventory = self.inventory()
        targets = self.aws("elbv2", "describe-target-health", "--target-group-arn", inventory["target_group_arn"])["TargetHealthDescriptions"]
        if len(targets) != 2 or any(t["TargetHealth"]["State"] != "healthy" for t in targets):
            raise RuntimeError("ALB does not have two healthy Cloud targets")
        save(self.private / "readiness-evidence.json", {"source_commit":self.state["source_commit"], "nodes":proof, "alb_targets":targets})
        spec = importlib.util.spec_from_file_location("installation_probe", REPO / "scripts/installation_probe.py")
        probe = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(probe)
        probe.verify(self, nodes, inventory)
        print("Cloud/Edge readiness, ALB targets and signed cross-node PUT/GET hashes verified.")

    def drain(self):
        # Cloud must remain running until its ASG hook binds the invocation.
        # Stopping it earlier can trigger replacement and erase that binding.
        for node, profile in self.nodes():
            if profile == "edge":
                self.command(node, "python3 /opt/miles/current/deploy/drain.py --edge", timeout=2000)

    def prepare_cleanup(self):
        if not self.state.get("paused_applied"):
            raise ValueError("Complete drain/pause before planning datastore teardown")
        destination = self.private / "cloud-cleanup"
        if destination.exists():
            return
        shutil.copytree(self.private / "cloud", destination, ignore=shutil.ignore_patterns(".terraform", "terraform.tfstate*", "*.tfplan"))
        for name in ("data", "compute", "security"):
            file = destination / "modules" / name / "main.tf"
            content = file.read_text().replace("lifecycle { prevent_destroy = true }", "")
            if name == "data":
                content = content.replace("deletion_protection_enabled = true", "deletion_protection_enabled = false")
                # terraform fmt aligns the equals sign within the table resource.
                content = re.sub(r"deletion_protection_enabled\s*=\s*true", "deletion_protection_enabled = false", content)
            file.write_text(content)

    def empty_bucket(self):
        inventory = self.inventory()
        bucket = inventory["data_bucket_name"]
        while True:
            page = self.aws("s3api", "list-object-versions", "--bucket", bucket, "--max-keys", "1000", "--no-paginate")
            objects = [{"Key":v["Key"], "VersionId":v["VersionId"]} for key in ("Versions", "DeleteMarkers") for v in page.get(key, [])]
            if not objects:
                break
            file = self.private / "delete-versions.json"
            save(file, {"Objects":objects, "Quiet":True})
            if self.aws("s3api", "delete-objects", "--bucket", bucket, "--delete", "file://" + str(file)).get("Errors"):
                raise RuntimeError("S3 reported per-object deletion failures")
        while True:
            uploads = self.aws("s3api", "list-multipart-uploads", "--bucket", bucket, "--max-uploads", "1000", "--no-paginate").get("Uploads", [])
            if not uploads:
                break
            for upload in uploads:
                self.aws("s3api", "abort-multipart-upload", "--bucket", bucket, "--key", upload["Key"], "--upload-id", upload["UploadId"])


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--private", type=Path, required=True)
    sub = parser.add_subparsers(dest="command", required=True)
    for command in ("prepare", "provision", "verify", "closeout"):
        sub.add_parser(command)
    for command in ("plan", "apply"):
        child = sub.add_parser(command)
        child.add_argument("stage", choices=("operator", "state", "foundation", "active", "paused", "unprotect", "destroy"))
        if command == "apply":
            child.add_argument("--plan-sha256", required=True)
            child.add_argument("--delete-data", action="store_true")
    args = parser.parse_args()
    try:
        installation = Installation(args.config, args.private)
        with (installation.private / "deployment.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            if args.command == "plan":
                installation.plan(args.stage)
            elif args.command == "apply":
                installation.apply(args.stage, args.plan_sha256, args.delete_data)
            else:
                getattr(installation, args.command)()
    except (ValueError, RuntimeError) as error:
        raise SystemExit(str(error)) from None
    except (OSError, KeyError, subprocess.TimeoutExpired):
        raise SystemExit("Installation step failed. Reconcile private artifacts, AWS identity and prerequisites before retrying.") from None


if __name__ == "__main__":
    main()
