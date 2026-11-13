#!/usr/bin/env python3
"""Explicit acceptance operations; no AWS mutation occurs on import or init/assets."""

import argparse
import fcntl
import json
import os
from pathlib import Path
import signal
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from deploy.acceptance.common import Run, now, require, digest
from deploy.acceptance import prepare
from deploy.acceptance import deployment as deploy
from deploy.acceptance import hosts
from deploy.acceptance import iam
from deploy.acceptance import cleanup
from deploy.acceptance import workload

MUTATIONS = {
    "iam-activate",
    "apply",
    "install",
    "bootstrap",
    "trial",
    "shutdown",
    "cleanup-grant",
    "cleanup",
    "finish",
    "restart",
    "unprotect",
    "recover-apply",
    "benchmark-case",
}


def parser():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-dir", required=True, type=Path)
    sub = p.add_subparsers(dest="command", required=True)
    init = sub.add_parser("init")
    init.add_argument("--config", required=True, type=Path)
    configure = sub.add_parser("configure")
    configure.add_argument("--config", required=True, type=Path)
    configure.add_argument("--region", default="ap-southeast-1")
    configure.add_argument("--deploy-profile", default="miles-dev")
    configure.add_argument("--iam-profile", default="miles-iam-bootstrap")
    configure.add_argument("--backend-bucket")
    configure.add_argument("--boundary-arn")
    for name in [
        "assets",
        "preflight",
        "iam-review",
        "iam-activate",
        "install",
        "material",
        "bootstrap",
        "tunnel",
        "freeze",
        "snapshot",
        "unprotect",
        "cleanup-review",
        "cleanup-grant",
        "cleanup-plan",
        "cleanup",
        "finish",
        "audit",
        "status",
        "logout",
        "restart",
        "wait-cut",
    ]:
        cmd = sub.add_parser(name)
        if name in MUTATIONS or name == "material":
            cmd.add_argument("--confirm-run", required=True)
    for name in ("plan", "apply", "recover-apply"):
        cmd = sub.add_parser(name)
        cmd.add_argument("stage", choices=("foundation", "hosts", "active"))
        if name in ("apply", "recover-apply"):
            cmd.add_argument("--confirm-run", required=True)
    trial = sub.add_parser("trial")
    trial.add_argument("--concurrency", type=int, choices=(1, 4), required=True)
    trial.add_argument("--confirm-run", required=True)
    bench = sub.add_parser("benchmark-case")
    bench.add_argument("--case-id", required=True)
    bench.add_argument("--repetition", type=int, choices=(1, 2, 3), required=True)
    bench.add_argument("--confirm-run", required=True)
    stop = sub.add_parser("shutdown")
    stop.add_argument("--native-throttle", action="store_true")
    stop.add_argument("--confirm-run", required=True)
    recover = sub.add_parser("recover-host")
    recover.add_argument("node", choices=("edge", "client"))
    collect = sub.add_parser("collect")
    collect.add_argument("--trial")
    login = sub.add_parser("login")
    login.add_argument("--iam", action="store_true")
    return p


def wait_cut(r):
    def gate():
        return r.aws(
            "dynamodb",
            "get-item",
            "--table-name",
            "miles-dev-SystemConfig",
            "--key",
            json.dumps({"config_id": {"S": "coordination/r1/current"}}),
            "--consistent-read",
        )["Item"]

    initial = gate()
    epoch = int(initial["epoch"]["N"])
    r.write("cut-wait-start.json", {"at": now().isoformat(), "gate": initial})

    def advanced():
        r.live_window(needed=30)
        current = gate()
        if int(current["epoch"]["N"]) > epoch and current["state"]["S"] == "Open":
            r.write("cut-wait-end.json", {"at": now().isoformat(), "gate": current})
            return True
        return False

    r.wait("natural epoch advance", advanced, 3900)


def restart(r):
    r.identity()
    r.live_window(needed=25)
    require(
        r.file("stop-proof.json").exists()
        and not r.file("cleanup-intent.json").exists(),
        "Restart only after retained stop proof, before cleanup",
    )
    require(
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
        == b"0",
        "Edge process still alive",
    )
    folder = "previous-stops/" + now().strftime("%Y%m%dT%H%M%S%f")
    r.file(folder).mkdir(parents=True, mode=0o700)
    for name in (
        "stop-proof.json",
        "stop-intent.json",
        "stop-journal.log",
        "stop-systemd.txt",
        "pulse-result.json",
    ):
        if r.file(name).exists():
            r.file(name).rename(r.file(folder + "/" + name))
    r.ssh("edge", "sudo", "-n", "systemctl", "start", "miles.service")
    r.event(
        "restart",
        "same installation/release restarted; previous FAIL remains in " + folder,
    )


def logout(r):
    require(
        r.read("final-absence-summary.json").get("accepted") is True,
        "Finish cleanup audit before logout",
    )
    if r.file("tunnel-pid.json").exists():
        pid = r.read("tunnel-pid.json")["pid"]
        try:
            command = Path("/proc/" + str(pid) + "/cmdline").read_bytes()
        except FileNotFoundError:
            command = b""
        if command:
            require(
                b"admin_tunnel.py" in command and str(r.path).encode() in command,
                "PID ownership changed",
            )
            os.killpg(pid, signal.SIGTERM)
    for profile in (r.c["deploy_profile"], r.c["iam_profile"]):
        r.command(["aws", "logout", "--profile", profile], env=r.environment())
    # Only this run's derived assume-role credentials are removed; other cached profiles stay intact.
    role = r.read("names.json")["auditor"]
    for p in (Path.home() / ".aws/cli/cache").glob("*.json"):
        try:
            arn = json.loads(p.read_text()).get("AssumedRoleUser", {}).get("Arn", "")
        except (OSError, ValueError):
            continue
        if arn.startswith(
            "arn:aws:sts::" + r.c["account_id"] + ":assumed-role/" + role + "/"
        ):
            p.unlink()
    r.write("closed.json", {"at": now().isoformat(), "audit_accepted": True})
    r.event("logout", "operator sessions closed; keep private evidence")


def main(argv=None):
    os.umask(0o077)
    args = parser().parse_args(argv)
    if args.command == "configure":
        return prepare.configure(
            args.config,
            args.region,
            args.deploy_profile,
            args.iam_profile,
            args.backend_bucket,
            args.boundary_arn,
        )
    if args.command == "init":
        return prepare.init(args.run_dir, args.config)
    r = Run(args.run_dir)
    if hasattr(args, "confirm_run"):
        require(
            args.confirm_run == r.id, "Confirmation must identify this exact run_id"
        )
    if args.command == "status":
        print(
            json.dumps(
                {
                    "run_id": r.id,
                    "source": r.meta["source"],
                    "paid_window": r.read("paid-window.json")
                    if r.file("paid-window.json").exists()
                    else None,
                    "stage_receipts": [p.name for p in r.path.glob("*-outputs.json")],
                    "closed": r.file("closed.json").exists(),
                },
                indent=2,
            )
        )
        return
    if args.command == "login":
        profile = r.c["iam_profile"] if args.iam else r.c["deploy_profile"]
        subprocess.run(
            ["aws", "login", "--profile", profile, "--remote"],
            env=r.environment(),
            check=True,
        )
        r.identity(iam=args.iam)
        return
    prepared = r.read("init-complete.json")
    require(
        prepared["source"] == r.meta["source"]
        and prepared["config_sha256"] == digest(r.file("config.json")),
        "Prepared config/source changed; reconcile instead of switching account/backend mid-run",
    )
    # Prevent cleanup/rollout and a trial from racing. Read-only collection can accompany a diagnosis.
    lock = r.file("operator.lock").open("a")
    if args.command != "collect":
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError(
                "Another operation owns this run; inspect it before cleanup/rollout"
            )
    with lock:
        actions = {
            "assets": prepare.assets,
            "preflight": deploy.preflight,
            "iam-review": iam.review,
            "iam-activate": iam.activate,
            "install": hosts.install,
            "material": deploy.provision_material,
            "bootstrap": hosts.bootstrap,
            "tunnel": workload.tunnel,
            "unprotect": cleanup.unprotect,
            "freeze": cleanup.freeze,
            "snapshot": cleanup.snapshot,
            "cleanup-review": cleanup.cleanup_policy,
            "cleanup-grant": cleanup.grant_cleanup,
            "cleanup-plan": cleanup.plan,
            "cleanup": cleanup.execute,
            "finish": cleanup.finish,
            "audit": cleanup.audit,
            "logout": logout,
            "wait-cut": wait_cut,
            "restart": restart,
        }
        if args.command == "plan":
            deploy.plan(r, args.stage)
        elif args.command == "recover-apply":
            deploy.recover_apply(r, args.stage)
        elif args.command == "recover-host":
            hosts.recover_host(r, args.node)
        elif args.command == "apply":
            deploy.apply(r, args.stage)
        elif args.command == "trial":
            workload.trial(r, args.concurrency)
        elif args.command == "benchmark-case":
            workload.benchmark_case(r, args.case_id, args.repetition)
        elif args.command == "shutdown":
            workload.stop(r, args.native_throttle)
        elif args.command == "collect":
            workload.collect(r, args.trial)
        else:
            actions[args.command](r)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("STOP:", str(error), file=sys.stderr)
        raise SystemExit(1)
