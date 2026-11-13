#!/usr/bin/env python3
"""Run a frozen acceptance recipe until completion or an operator checkpoint."""

import argparse
import contextlib
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from deploy.acceptance.common import ROOT, Run, digest, now, require
from deploy.acceptance.run import MUTATIONS

NEEDS_APPROVAL = 20
NEEDS_AUTH = 21
NEEDS_RECONCILIATION = 22
FAILED = 1

def _matrix_benchmark_cases():
    matrix_file = ROOT / "benchmarks/workload-matrix.json"
    if matrix_file.is_file():
        try:
            data = json.loads(matrix_file.read_text())
            return [c["id"] for c in data.get("cases", [])]
        except Exception:
            pass
    return [
        "warm-cloud-c1", "warm-cloud-c4", "warm-edge-c1", "warm-edge-c4",
        "overwrite-cloud", "domain-shared-default", "domain-separated",
        "multipart-threshold-minus-one", "multipart-threshold-equal",
        "multipart-threshold-plus-one", "cold-cloud-ir", "cold-cloud-flexible",
        "cold-edge-ir", "cold-edge-flexible", "edge-clone-download",
        "edge-clone-upload", "recovery-cloud-r1", "recovery-cloud-r2",
        "recovery-edge-r1", "recovery-edge-r2",
    ]

_BENCHMARK_CASES = _matrix_benchmark_cases()

# Recipes invoke only existing kit operations, never a shell or external script.
COMMANDS = (
    {
        (x,)
        for x in (
            "assets",
            "preflight",
            "iam-review",
            "iam-activate",
            "install",
            "material",
            "bootstrap",
            "freeze",
            "snapshot",
            "cleanup-review",
            "cleanup-grant",
            "unprotect",
            "cleanup-plan",
            "cleanup",
            "finish",
            "audit",
            "logout",
            "collect",
            "wait-cut",
            "restart",
            "shutdown",
        )
    }
    | {
        (action, stage)
        for action in ("plan", "apply")
        for stage in ("foundation", "hosts", "active")
    }
    | {("trial", "--concurrency", str(c)) for c in (1, 4)}
    | {
        ("shutdown", "--native-throttle"),
    }
    | {
        ("benchmark-case", "--case-id", cid, "--repetition", str(rep))
        for cid in _BENCHMARK_CASES
        for rep in (1, 2, 3)
    }
)
WRITES = MUTATIONS | {"material", "logout"}
TESTS = {"trial", "shutdown", "benchmark-case"}
COST_REVIEW = {"approval-review.md", "cost-review.json"}


def required_review(command):
    action = command[0]
    if action == "apply":
        return COST_REVIEW | {command[1] + "-plan.json"}
    if action == "iam-activate":
        return (
            COST_REVIEW
            | {"iam-review.json"}
            | {
                "iam/" + p.name.removesuffix(".tmpl")
                for p in (ROOT / "deploy/acceptance/policies").glob("*.tmpl")
            }
        )
    if action in {"cleanup-grant", "unprotect"}:
        return {"cleanup-review.json", "iam/cleanup.json", "frozen/manifest.json"}
    if action == "cleanup":
        return {
            "destroy-reviewed.json",
            "destroy.tfplan",
            "destroy-plan.json",
            "frozen/manifest.json",
        }
    if action in {"finish", "logout"}:
        return {"frozen/manifest.json", "destroy-reviewed.json"}
    return COST_REVIEW


def local_file(r, name):
    path = r.file(name)
    require(
        path.resolve().is_relative_to(r.path)
        and not any(p.is_symlink() for p in (path, *path.parents) if p != r.path.parent)
        and path.is_file(),
        "Evidence must be a regular file inside this run",
    )
    return path


def validate(recipe):
    require(
        isinstance(recipe, dict)
        and set(recipe) == {"version", "steps", "cleanup_steps"}
        and recipe["version"] == 1,
        "Unsupported recipe schema",
    )
    seen = set()
    for section in ("steps", "cleanup_steps"):
        steps = recipe[section]
        require(isinstance(steps, list) and 0 < len(steps) <= 256, "Invalid step list")
        gates = {}
        for step in steps:
            require(isinstance(step, dict), "Invalid step")
            identifier = step.get("id", "")
            require(
                isinstance(identifier, str)
                and re.fullmatch(r"[a-z][a-z0-9-]{0,63}", identifier)
                and identifier not in seen,
                "Missing or duplicate step ID",
            )
            seen.add(identifier)
            if "review" in step:
                require(set(step) == {"id", "review"}, "Invalid review checkpoint")
                require(
                    isinstance(step["review"], list)
                    and step["review"]
                    and all(
                        isinstance(p, str)
                        and p
                        and not Path(p).is_absolute()
                        and ".." not in Path(p).parts
                        for p in step["review"]
                    ),
                    "Invalid review paths",
                )
                gates[identifier] = set(step["review"])
                continue
            require(
                set(step) <= {"id", "command", "approval", "timeout_seconds"}
                and isinstance(step.get("command"), list)
                and all(isinstance(x, str) for x in step["command"])
                and tuple(step["command"]) in COMMANDS,
                "Recipe command is not a supported kit operation",
            )
            require(
                type(step.get("timeout_seconds")) is int
                and 0 < step["timeout_seconds"] <= 5400,
                "Each command needs a timeout of 1..5400 seconds",
            )
            if step["command"][0] in WRITES:
                require(
                    step.get("approval") in gates,
                    "Mutation needs an earlier checkpoint",
                )
                require(
                    required_review(step["command"]) <= gates[step["approval"]],
                    "Checkpoint is missing the mutation's plan/scope evidence",
                )
            elif "approval" in step:
                require(step["approval"] in gates, "Unknown checkpoint")
    return recipe


def read_recipe(path):
    require(
        path.is_file() and not path.is_symlink() and path.stat().st_size <= 65536,
        "Recipe must be a regular JSON file <=64 KiB",
    )

    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "Duplicate recipe key")
            result[key] = value
        return result

    return validate(json.loads(path.read_text(), object_pairs_hook=unique))


class Process:
    def __init__(self, run):
        self.r = run
        self.recipe = validate(run.read("process/recipe.json"))
        self.state = run.read("process/state.json")
        require(
            self.state["recipe_sha256"] == digest(run.file("process/recipe.json")),
            "Frozen recipe changed; do not switch workload mid-run",
        )
        prepared = run.read("init-complete.json")
        require(
            self.state["source"] == run.meta["source"] == prepared["source"]
            and self.state["config_sha256"]
            == prepared["config_sha256"]
            == digest(run.file("config.json")),
            "Prepared source/config changed",
        )

    def save(self, status, step=None):
        self.state.update(
            status=status, current_step=step, updated_at=now().isoformat()
        )
        self.r.write("process/state.json", self.state)
        self.r.write(
            "process/summary.json",
            {
                "run_id": self.r.id,
                "source": self.state["source"],
                "status": status,
                "phase": self.state["phase"],
                "current_step": step,
                "paid_window": self.r.read("paid-window.json")
                if self.r.file("paid-window.json").exists()
                else None,
                "failed_steps": [
                    name
                    for name, result in self.state["results"].items()
                    if result.get("failure")
                ],
                "steps": self.state["results"],
                "note": "Process completion is not full benchmark or AWS acceptance.",
            },
        )

    def brief(self):
        return {
            "run_id": self.r.id,
            "status": self.state["status"],
            "phase": self.state["phase"],
            "current_step": self.state.get("current_step"),
            "completed_steps": sum(
                x["status"] == "complete" for x in self.state["results"].values()
            ),
            "failed_steps": [
                name
                for name, result in self.state["results"].items()
                if result.get("failure")
            ],
            "summary": str(self.r.file("process/summary.json")),
            "paid_window": self.r.read("paid-window.json")
            if self.r.file("paid-window.json").exists()
            else None,
        }

    def checkpoint(self, step):
        files = set(step["review"])
        for name in list(files):
            # Plan receipts point to unique saved plans and their exact vars.
            if name in {s + "-plan.json" for s in ("foundation", "hosts", "active")}:
                plan = self.r.read(name)
                files.update((plan["plan"], plan["vars"]))
                files.add(str(Path(plan["plan"]).with_suffix(".json")))
        return {
            "run_id": self.r.id,
            "source": self.state["source"],
            "recipe_sha256": self.state["recipe_sha256"],
            "config_sha256": self.state["config_sha256"],
            "files": {name: digest(local_file(self.r, name)) for name in sorted(files)},
        }

    def approved(self, step):
        expected = self.checkpoint(step)
        receipt = self.state["approvals"].get(step["id"])
        if receipt and receipt["review"] == expected:
            return True
        self.r.write(
            "process/pending-review.json", {"checkpoint": step["id"], **expected}
        )
        self.save("needs_approval", step["id"])
        return False

    def approve(self, checkpoint):
        require(
            self.state["status"] == "needs_approval"
            and self.state["current_step"] == checkpoint,
            "No matching pending checkpoint",
        )
        step = next(
            s for s in self.recipe[self.state["phase"]] if s["id"] == checkpoint
        )
        review = self.checkpoint(step)
        require(
            self.r.read("process/pending-review.json")
            == {"checkpoint": checkpoint, **review},
            "Review inputs changed; run again to generate a fresh checkpoint",
        )
        self.state["approvals"][checkpoint] = {
            "at": now().isoformat(),
            "review": review,
        }
        self.save("ready", checkpoint)

    def run(self, cleanup=False):
        if cleanup:
            # Failed workload evidence stays immutable; cleanup is a separate route.
            self.state["phase"] = "cleanup_steps"
            for result in self.state["results"].values():
                if result["status"] in {"running", "failed"}:
                    result["failure"] = True
        steps = self.recipe[self.state["phase"]]
        for step in steps:
            name = step["id"]
            if "review" in step:
                if not self.approved(step):
                    return NEEDS_APPROVAL
                continue
            previous = self.state["results"].get(name)
            if previous:
                if previous["status"] in {"complete", "operator_reconciled"}:
                    continue
                if previous["status"] != "not_started":
                    self.save("needs_reconciliation", name)
                    return NEEDS_RECONCILIATION
            if "approval" in step:
                gate = next(s for s in steps if s["id"] == step["approval"])
                if not self.approved(gate):
                    return NEEDS_APPROVAL
            require(not self.r.file("closed.json").exists(), "Run is already closed")
            # Authentication is checked before dispatch, so expired sessions can
            # resume safely without assuming a failed mutation did not commit.
            if step["command"][0] != "assets":
                try:
                    self.r.identity()
                    self.r.identity(iam=True)
                except (ValueError, OSError, subprocess.SubprocessError):
                    self.state["results"][name] = {"status": "not_started"}
                    self.save("needs_auth", name)
                    return NEEDS_AUTH
            result = {
                "status": "running",
                "started_at": now().isoformat(),
                "log": "process/logs/" + name + ".log",
            }
            self.state["results"][name] = result
            self.save("running", name)
            print(
                json.dumps({"step": name, "status": "running", "log": result["log"]}),
                flush=True,
            )
            code = self.execute(step, result)
            result.update(
                finished_at=now().isoformat(),
                exit_code=code,
                status="complete" if code == 0 else "failed",
            )
            if code:
                result["failure"] = True
                self.save("failed", name)
                return FAILED
            self.save("ready", name)
        if self.state["phase"] == "steps":
            return self.run(cleanup=True)
        failed = any(x.get("failure") for x in self.state["results"].values())
        self.save("finished_with_failures" if failed else "finished")
        return FAILED if failed else 0

    def execute(self, step, result):
        command = [
            sys.executable,
            str(self.r.file("source/deploy/acceptance/run.py")),
            "--run-dir",
            str(self.r.path),
            *step["command"],
        ]
        if step["command"][0] in WRITES - {"logout"}:
            command += ["--confirm-run", self.r.id]
        if (
            step["command"] == ["freeze"]
            and self.r.file("frozen/manifest.json").exists()
        ):
            manifest = self.r.read("frozen/manifest.json")
            require(
                manifest.get("run_id") == self.r.id
                and manifest.get("account_id") == self.r.c["account_id"]
                and manifest.get("region") == self.r.c["region"],
                "Frozen ownership differs",
            )
            result["reused_manifest_sha256"] = digest(
                self.r.file("frozen/manifest.json")
            )
            self.r.write(
                result["log"],
                {"reused_manifest_sha256": result["reused_manifest_sha256"]},
            )
            return 0
        path = self.r.file(result["log"])
        path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        timeout = step["timeout_seconds"]
        if self.r.file("paid-window.json").exists():
            end = dt.datetime.fromisoformat(
                self.r.read("paid-window.json")["cleanup_by"]
            )
            timeout = min(timeout, max(1, (end - now()).total_seconds()))
        with os.fdopen(
            os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb"
        ) as log:
            with subprocess.Popen(
                command,
                stdout=log,
                stderr=subprocess.STDOUT,
                start_new_session=True,
                env=self.r.environment(),
            ) as child:
                try:
                    code = child.wait(timeout=timeout)
                except (subprocess.TimeoutExpired, KeyboardInterrupt):
                    # Only this dispatched process group is ours. Remote unknown
                    # outcomes remain blocked for reconciliation, never replayed.
                    with contextlib.suppress(ProcessLookupError):
                        os.killpg(child.pid, signal.SIGTERM)
                    try:
                        child.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        with contextlib.suppress(ProcessLookupError):
                            os.killpg(child.pid, signal.SIGKILL)
                        child.wait()
                    return 124
        if not code and step["command"][0] == "shutdown":
            # The existing command records FAIL without a nonzero exit code.
            return (
                0 if self.r.read("stop-proof.json").get("accepted") is True else FAILED
            )
        return code

    def reconcile(self, name, evidence):
        step = next(
            (
                s
                for section in ("steps", "cleanup_steps")
                for s in self.recipe[section]
                if s["id"] == name
            ),
            None,
        )
        require(step is not None and "command" in step, "Unknown command step")
        require(
            step["command"][0] not in TESTS,
            "Failed tests cannot be acknowledged as passing",
        )
        result = self.state["results"].get(name)
        require(
            result and result["status"] in {"running", "failed"}, "Nothing to reconcile"
        )
        result.update(
            status="operator_reconciled",
            reconciled_at=now().isoformat(),
            evidence={p: digest(local_file(self.r, p)) for p in evidence},
        )
        self.save("ready", name)


def main(argv=None):
    os.umask(0o077)
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-dir", required=True, type=Path)
    sub = p.add_subparsers(dest="action", required=True)
    init = sub.add_parser("init")
    init.add_argument("--template", required=True, type=Path)
    sub.add_parser("status")
    run = sub.add_parser("run")
    run.add_argument("--cleanup", action="store_true")
    approve = sub.add_parser("approve")
    approve.add_argument("--checkpoint", required=True)
    reconcile = sub.add_parser("reconcile")
    reconcile.add_argument("--step", required=True)
    reconcile.add_argument("--evidence", action="append", required=True)
    for cmd in (run, approve, reconcile):
        cmd.add_argument("--confirm-run", required=True)
    args = p.parse_args(argv)
    r = Run(args.run_dir)
    require(
        Path(__file__).resolve()
        == r.file("source/deploy/acceptance/process.py").resolve(),
        "Use the process runner from this run's frozen source",
    )
    if hasattr(args, "confirm_run"):
        require(args.confirm_run == r.id, "Confirm this exact run_id")
    r.file("process").mkdir(mode=0o700, exist_ok=True)
    if args.action == "status":
        current = Process(r)
        print(json.dumps(current.brief(), indent=2))
        return 0
    with r.file("process/controller.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.action == "init":
            recipe = read_recipe(args.template)
            require(
                not r.file("process/recipe.json").exists(),
                "Process already initialized",
            )
            r.write("process/recipe.json", recipe, new=True)
            r.write(
                "process/state.json",
                {
                    "recipe_sha256": digest(r.file("process/recipe.json")),
                    "source": r.meta["source"],
                    "config_sha256": digest(r.file("config.json")),
                    "phase": "steps",
                    "status": "ready",
                    "results": {},
                    "approvals": {},
                },
                new=True,
            )
        process = Process(r)
        if args.action == "approve":
            process.approve(args.checkpoint)
        elif args.action == "reconcile":
            process.reconcile(args.step, args.evidence)
        elif args.action == "run":

            def interrupted(*_):
                raise KeyboardInterrupt

            signal.signal(signal.SIGTERM, interrupted)
            code = process.run(args.cleanup)
            print(json.dumps(process.brief(), indent=2))
            return code
        if args.action in {"init", "status"}:
            print(json.dumps(process.brief(), indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print(
            "STOP: interrupted; reconcile dispatched steps before resuming",
            file=sys.stderr,
        )
        sys.exit(130)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("STOP:", str(error), file=sys.stderr)
        sys.exit(FAILED)
