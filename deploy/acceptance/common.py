"""Private run state and explicit AWS/SSH/Terraform boundaries for acceptance."""

import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
CONFIG_KEYS = {
    "account_id",
    "region",
    "deploy_role_arn",
    "runtime_boundary_arn",
    "backend_bucket",
    "backend_key",
    "deploy_profile",
    "iam_profile",
    "ami_id",
    "zones",
    "existing_autoscaling_role_arn",
    "max_usd",
    "hours",
    "assets_dir",
}
DEFAULTS = {
    "region": "ap-southeast-1",
    "backend_key": "cloud/dev/terraform.tfstate",
    "deploy_profile": "miles-dev",
    "iam_profile": "miles-iam-bootstrap",
    "hours": 4,
    "max_usd": 10,
}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def now():
    return dt.datetime.now(dt.timezone.utc)


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def load(path):
    return json.loads(Path(path).read_text())


def private(path, data, *, new=False):
    path = Path(path)
    require(not path.is_symlink(), "Refuse symlink output")
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if new:
        with path.open("x") as out:
            os.chmod(path, 0o600)
            out.write(data)
    else:
        fd, temporary = tempfile.mkstemp(dir=path.parent, prefix=".write-")
        try:
            with os.fdopen(fd, "w") as out:
                out.write(data)
            os.replace(temporary, path)
        finally:
            Path(temporary).unlink(missing_ok=True)
    return path


def save(path, value, compact=False, **kwargs):
    if compact:
        body = json.dumps(value, separators=(",", ":"), sort_keys=True)
    else:
        body = json.dumps(value, indent=2, sort_keys=True) + "\n"
    return private(path, body, **kwargs)


def config(value):
    require(
        isinstance(value, dict) and not set(value) - CONFIG_KEYS,
        "Unknown configuration field",
    )
    c = {**DEFAULTS, **value}
    require(
        re.fullmatch(r"\d{12}", c.get("account_id", "")),
        "account_id must have 12 digits",
    )
    for key in ("deploy_role_arn", "runtime_boundary_arn"):
        kind = "role" if key == "deploy_role_arn" else "policy"
        require(
            re.fullmatch(
                r"arn:aws:iam::" + c["account_id"] + ":" + kind + r"/[\w+=,.@/-]+",
                c.get(key, ""),
            ),
            "Invalid account-bound " + key,
        )
    require(
        re.fullmatch(r"[a-z]{2}-[a-z]+-\d", c["region"]),
        "Invalid commercial AWS region",
    )
    require(
        re.fullmatch(r"[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]", c.get("backend_bucket", "")),
        "Invalid state bucket",
    )
    require(
        isinstance(c["backend_key"], str)
        and c["backend_key"]
        and "\n" not in c["backend_key"],
        "Invalid backend key",
    )
    require(
        0 < c["hours"] <= 12 and 0 < c["max_usd"] <= 50,
        "Acceptance run supports at most 12 hours / 50 USD",
    )
    require(
        re.fullmatch(r"ami-[0-9a-f]{8,17}", c.get("ami_id", "")),
        "Pin an Amazon Linux 2023 ARM64 AMI",
    )
    if c.get("existing_autoscaling_role_arn"):
        require(
            c["existing_autoscaling_role_arn"].startswith(
                "arn:aws:iam::"
                + c["account_id"]
                + ":role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling"
            ),
            "ASG service role must belong to this account and service",
        )
    zones = c.get("zones", [c["region"] + "a", c["region"] + "b"])
    require(
        len(zones) == 2
        and len(set(zones)) == 2
        and all(re.fullmatch(c["region"] + "[a-z]", z) for z in zones),
        "Need two AZs",
    )
    c["zones"] = zones
    for key in ("deploy_profile", "iam_profile"):
        require(re.fullmatch(r"[A-Za-z0-9_.-]+", c[key]), "Invalid profile")
    return c


class Run:
    def __init__(self, directory):
        self.path = Path(directory).expanduser().resolve()
        require(
            self.path.is_dir() and not self.path.is_relative_to(ROOT),
            "Use an initialized private directory outside the repository",
        )
        require(self.path.stat().st_mode & 0o077 == 0, "Run directory must be 0700")
        self.c = config(load(self.path / "config.json"))
        self.meta = load(self.path / "run.json")
        self.id = self.meta["run_id"]
        self.stack = self.path / "source/infra/terraform/cloud/environments/dev"
        self.native_config = self.meta["native_config"]
        self.native_credentials = self.meta["native_credentials"]

    def file(self, name):
        require(
            not Path(name).is_absolute() and ".." not in Path(name).parts,
            "Invalid artifact path",
        )
        return self.path / name

    def read(self, name):
        return load(self.file(name))

    def write(self, name, value, **kwargs):
        return save(self.file(name), value, **kwargs)

    def event(self, step, outcome):
        with self.file("run-log.jsonl").open("a") as out:
            out.write(
                json.dumps({"at": now().isoformat(), "step": step, "outcome": outcome})
                + "\n"
            )

    def live_window(self, needed=0):
        window = self.read("paid-window.json")
        end = dt.datetime.fromisoformat(window["cleanup_by"])
        require(
            now() < end - dt.timedelta(minutes=90 + needed),
            "Stop new workload; enter cleanup reserve",
        )

    def command(self, argv, *, env=None, input=None, timeout=120, log=None):
        if env is None and Path(str(argv[0])).name in ("ssh", "scp"):
            env = self.environment()
        p = subprocess.run(
            [str(x) for x in argv],
            input=input,
            capture_output=True,
            env=env,
            timeout=timeout,
            cwd=ROOT,
        )
        if log:
            private(
                self.file(log),
                p.stdout.decode(errors="replace") + p.stderr.decode(errors="replace"),
            )
        if p.returncode and not log:
            log = "errors/" + now().strftime("%Y%m%dT%H%M%S%f") + ".log"
            private(
                self.file(log),
                p.stdout.decode(errors="replace") + p.stderr.decode(errors="replace"),
            )
        require(
            p.returncode == 0,
            "Command failed: "
            + str(argv[0])
            + "; inspect private "
            + (log or "artifacts / command result"),
        )
        return p.stdout

    def environment(self, private_config=False):
        env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
        env.update(
            AWS_REGION=self.c["region"],
            AWS_DEFAULT_REGION=self.c["region"],
            AWS_PAGER="",
            AWS_CONFIG_FILE=str(self.file("aws-config"))
            if private_config
            else self.native_config,
            AWS_SHARED_CREDENTIALS_FILE=self.native_credentials,
            AWS_EC2_METADATA_DISABLED="true",
        )
        return env

    def aws(
        self,
        service,
        operation,
        *args,
        iam=False,
        audit=False,
        output="json",
        region=None,
    ):
        profile = (
            self.c["iam_profile"]
            if iam
            else ("acceptance-audit" if audit else self.c["deploy_profile"])
        )
        if iam:
            read = operation.startswith(
                ("get-", "list-", "describe-", "validate-", "simulate-")
            )
            require(
                service in ("iam", "accessanalyzer", "sts") or read,
                "IAM identity cannot mutate workload",
            )
        if not operation.startswith(
            ("get-", "list-", "describe-", "head-", "validate-", "simulate-")
        ):
            self.identity(iam=iam)
        env = self.environment(private_config=audit)
        data = self.command(
            [
                "aws",
                "--profile",
                profile,
                "--region",
                region or self.c["region"],
                "--no-cli-pager",
                "--cli-connect-timeout",
                "5",
                "--cli-read-timeout",
                "30",
                service,
                operation,
                *args,
                "--output",
                output,
            ],
            env=env,
            timeout=180,
        )
        return json.loads(data) if data.strip() and output == "json" else ({} if output == "json" else data)

    def identity(self, iam=False):
        i = self.aws("sts", "get-caller-identity", iam=iam)
        require(i["Account"] == self.c["account_id"], "AWS account mismatch")
        expected = (
            "arn:aws:iam::" + self.c["account_id"] + ":root"
            if iam
            else (
                "arn:aws:sts::"
                + self.c["account_id"]
                + ":assumed-role/"
                + self.c["deploy_role_arn"].rsplit("/", 1)[1]
                + "/"
            )
        )
        require(
            i["Arn"] == expected if iam else i["Arn"].startswith(expected),
            "Wrong IAM/deploy identity",
        )
        return i

    def tf(self, *args, stack=None, log=None):
        self.identity()
        env = self.environment(True)
        env.update(
            AWS_PROFILE="acceptance-deploy",
            TF_DATA_DIR=str(self.file("terraform-data")),
        )
        return self.command(
            ["terraform", "-chdir=" + str(stack or self.stack), *args],
            env=env,
            timeout=3000,
            log=log,
        )

    def outputs(self, name="active"):
        return {k: v["value"] for k, v in self.read(name + "-outputs.json").items()}

    def ssh(self, node, *args, input=None, timeout=120, log=None):
        require(node in ("edge", "client", "cloud-a", "cloud-b"), "Unknown node alias")
        return self.command(
            [
                "ssh",
                "-F",
                self.file("ssh-config"),
                node,
                shlex.join([str(a) for a in args]),
            ],
            input=input,
            timeout=timeout,
            log=log,
        )

    def copy(self, node, files):
        self.command(
            [
                "scp",
                "-F",
                self.file("ssh-config"),
                *[self.file(f) for f in files],
                node + ":/home/ec2-user/acceptance/",
            ],
            timeout=300,
        )

    def wait(self, label, check, seconds=600):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if check():
                return
            time.sleep(5)
        raise ValueError(label + " deadline; inspect evidence, do not replay mutations")
