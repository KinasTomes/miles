#!/usr/bin/env python3
"""On-instance ASG termination hook. Never equate timeout/exit alone with drain."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import time
from urllib.error import HTTPError
from urllib.request import ProxyHandler, Request, build_opener


STATE = Path("/var/lib/miles-lifecycle")
POLL_SECONDS = 5
HEARTBEAT_SECONDS = 30
TOTAL_SECONDS = 2100  # app1800/ALB1800 plus bounded control-plane slack


def event(name, **fields):
    print(json.dumps({"event": name, **fields}), flush=True)


class IncompleteDrain(RuntimeError):
    pass


class Lifecycle:
    def __init__(self, config):
        self.config = config
        self.http = build_opener(ProxyHandler({}))
        self.token = None
        self.instance = None

    def metadata(self, path):
        if not self.token:
            request = Request("http://169.254.169.254/latest/api/token", method="PUT",
                              headers={"X-aws-ec2-metadata-token-ttl-seconds": "21600"})
            self.token = self.http.open(request, timeout=5).read().decode()
        request = Request("http://169.254.169.254/latest/" + path,
                          headers={"X-aws-ec2-metadata-token": self.token})
        try:
            return self.http.open(request, timeout=5).read().decode()
        except HTTPError as error:
            if error.code == 401:
                self.token = None
            raise

    def command(self, argv):
        env = {key: value for key, value in os.environ.items()
               if not key.startswith("AWS_") and key.upper() not in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY")}
        env.update(AWS_MAX_ATTEMPTS="1", AWS_PAGER="", AWS_CONFIG_FILE="/dev/null",
                   AWS_SHARED_CREDENTIALS_FILE="/dev/null", AWS_EC2_METADATA_DISABLED="false")
        result = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=20)
        if result.returncode:
            # CLI diagnostics can contain infrastructure IDs. Journal records the
            # failed operation only; an operator can inspect CloudTrail privately.
            raise RuntimeError("command failed: " + argv[0] + " " + argv[1])
        return result.stdout

    def aws(self, service, operation, *args):
        result = self.command(["aws", service, operation, *args, "--region", self.config["region"],
                               "--cli-connect-timeout", "5",
                               "--cli-read-timeout", "10", "--output", "json", "--no-cli-pager"])
        return json.loads(result) if result.strip() else {}

    def hook(self, operation):
        return self.aws("autoscaling", operation, "--auto-scaling-group-name", self.config["asg_name"],
                        "--lifecycle-hook-name", self.config["hook_name"], "--instance-id", self.instance,
                        *(["--lifecycle-action-result", "CONTINUE"] if operation == "complete-lifecycle-action" else []))

    def service(self):
        text = self.command(["systemctl", "show", "miles.service", "--no-pager",
                             "--property=ActiveState,MainPID,ExecMainStatus,Result,InvocationID"])
        return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)

    def stopped_cleanly(self, invocation):
        state = self.service()
        if state.get("MainPID") != "0" or state.get("ActiveState") in ("active", "activating", "deactivating"):
            return False
        if state.get("Result") != "success" or state.get("ExecMainStatus") != "0":
            raise IncompleteDrain("gateway stopped without a successful shutdown")
        records = self.command(["journalctl", "_SYSTEMD_INVOCATION_ID=" + invocation,
                                "--output=json", "--lines=200", "--no-pager"])
        for line in records.splitlines():
            try:
                message = json.loads(json.loads(line)["MESSAGE"])
            except (ValueError, KeyError, TypeError):
                continue
            if isinstance(message, dict) and message.get("msg") == "shutdown complete":
                return True
        raise IncompleteDrain("gateway exit lacks shutdown-complete proof for this invocation")

    def target_unused(self):
        response = self.aws("elbv2", "describe-target-health", "--target-group-arn",
                            self.config["target_group_arn"], "--targets", "Id=" + self.instance + ",Port=8080")
        targets = response.get("TargetHealthDescriptions", [])
        if len(targets) != 1:
            raise RuntimeError("target health response is ambiguous")
        return targets[0].get("TargetHealth", {}).get("State") == "unused"

    def save(self, state):
        temporary = STATE / "state.new"
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as stream:
            json.dump(state, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, STATE / "state.json")

    def drain(self):
        identity = json.loads(self.metadata("dynamic/instance-identity/document"))
        self.instance = identity["instanceId"]
        if (identity["accountId"] != self.config["account_id"] or identity["region"] != self.config["region"]
                or not re.fullmatch(r"i-[a-f0-9]{17}", self.instance)):
            raise IncompleteDrain("instance identity does not match the reviewed deployment")
        boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
        path = STATE / "state.json"
        if path.is_symlink():
            raise IncompleteDrain("unsafe lifecycle state")
        if path.exists():
            state = json.loads(path.read_text())
            if state["instance"] != self.instance or state["boot"] != boot or state.get("failed"):
                raise IncompleteDrain("previous drain is incomplete; deadline cannot be reset")
        else:
            invocation = self.service().get("InvocationID", "")
            if not re.fullmatch(r"[a-f0-9]{32}", invocation):
                raise IncompleteDrain("no bound gateway invocation to drain")
            state = {"instance": self.instance, "boot": boot, "invocation": invocation,
                     "start": time.monotonic(), "stopping": False, "deregistered": False}
            self.save(state)
        next_heartbeat = 0
        while True:
            now = time.monotonic()
            if now < state["start"] or now - state["start"] >= TOTAL_SECONDS:
                state["failed"] = True
                self.save(state)
                raise IncompleteDrain("drain deadline exhausted; lifecycle completion withheld")
            try:
                # Begin the application's own admission gate before remote API waits.
                if not state["stopping"]:
                    self.command(["systemctl", "stop", "--no-block", "miles.service"])
                    state["stopping"] = True
                    self.save(state)
                    event("drain-started")
                if now >= next_heartbeat:
                    self.hook("record-lifecycle-action-heartbeat")
                    next_heartbeat = time.monotonic() + HEARTBEAT_SECONDS
                if not state["deregistered"]:
                    self.aws("elbv2", "deregister-targets", "--target-group-arn",
                             self.config["target_group_arn"], "--targets", "Id=" + self.instance + ",Port=8080")
                    state["deregistered"] = True
                    self.save(state)
                if self.stopped_cleanly(state["invocation"]) and self.target_unused():
                    self.hook("complete-lifecycle-action")
                    event("drain-completed", proof="gateway shutdown complete and ALB target unused")
                    return
            except IncompleteDrain:
                state["failed"] = True
                self.save(state)
                raise
            except (RuntimeError, subprocess.TimeoutExpired, OSError, ValueError):
                event("control-plane-retry", completion="withheld")
            time.sleep(POLL_SECONDS)

    def run(self):
        STATE.mkdir(mode=0o700, parents=True, exist_ok=True)
        info = STATE.lstat()
        if STATE.is_symlink() or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o700:
            raise IncompleteDrain("lifecycle state directory must be private and owned by the controller")
        fd = os.open(STATE / "owner.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                event("controller-lock-held", completion="withheld")
                raise
            while True:
                try:
                    # IMDS exposes the TARGET steady state, not Terminating:Wait.
                    if self.metadata("meta-data/autoscaling/target-lifecycle-state") == "Terminated":
                        self.drain()
                        return
                except IncompleteDrain:
                    raise
                except (OSError, ValueError):
                    event("metadata-retry")
                time.sleep(POLL_SECONDS)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    args = parser.parse_args()
    try:
        Lifecycle(json.loads(args.config.read_text())).run()
    except Exception as error:
        event("drain-incomplete", reason=type(error).__name__, completion="withheld")
        raise SystemExit(1)
