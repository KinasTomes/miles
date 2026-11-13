"""Offline proofs for recipe dispatch, approvals, failure retention and resume."""

import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from deploy.acceptance.common import Run, digest
from deploy.acceptance import process


def command(name, args, approval=None, timeout=60):
    step = {"id": name, "command": args, "timeout_seconds": timeout}
    if approval:
        step["approval"] = approval
    return step


class ProcessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="miles-process-test-")
        self.addCleanup(self.temp.cleanup)
        p = Path(self.temp.name)
        p.chmod(0o700)
        (p / "config.json").write_text(
            json.dumps(
                {
                    "account_id": "123456789012",
                    "deploy_role_arn": "arn:aws:iam::123456789012:role/test/deploy",
                    "runtime_boundary_arn": "arn:aws:iam::123456789012:policy/test/Boundary",
                    "backend_bucket": "fixture-state-123456789012",
                    "ami_id": "ami-0123456789abcdef0",
                }
            )
        )
        (p / "run.json").write_text(
            json.dumps(
                {
                    "run_id": "focused-fixture",
                    "source": "a" * 40,
                    "native_config": "/fixture/config",
                    "native_credentials": "/fixture/credentials",
                }
            )
        )
        self.r = Run(p)
        self.r.write(
            "init-complete.json",
            {
                "source": "a" * 40,
                "config_sha256": digest(p / "config.json"),
            },
        )
        identity = mock.patch.object(self.r, "identity")
        identity.start()
        self.addCleanup(identity.stop)
        self.recipe = {
            "version": 1,
            "steps": [command("assets", ["assets"])],
            "cleanup_steps": [command("collect", ["collect"])],
        }

    def controller(self, recipe=None):
        recipe = recipe or self.recipe
        process.validate(recipe)
        self.r.write("process/recipe.json", recipe)
        self.r.write(
            "process/state.json",
            {
                "recipe_sha256": digest(self.r.file("process/recipe.json")),
                "config_sha256": digest(self.r.file("config.json")),
                "source": "a" * 40,
                "phase": "steps",
                "status": "ready",
                "results": {},
                "approvals": {},
            },
        )
        return process.Process(self.r)

    def reviewed_test(self, args):
        for name in process.COST_REVIEW:
            self.r.write(name, {"fixture": True})
        return {
            **self.recipe,
            "steps": [
                {"id": "review", "review": sorted(process.COST_REVIEW)},
                command("trial", args, "review"),
            ],
        }

    def test_shipped_template_and_rejected_shell_missing_scope_and_duplicate_ids(self):
        template = ROOT / "deploy/acceptance/templates/focused-get-shutdown.json"
        process.read_recipe(template)
        for bad in (
            command("shell", ["bash", "-c", "true"]),
            command("unapproved", ["apply", "foundation"]),
            command("invalid-timeout", ["assets"], timeout=0),
            command("secret-in-command", ["trial", "--token", "fixture"]),
        ):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                process.validate({**self.recipe, "steps": [bad]})
        recipe = copy.deepcopy(self.recipe)
        recipe["steps"].append(command("assets", ["collect"]))
        with self.assertRaises(ValueError):
            process.validate(recipe)
        recipe["steps"] = [
            {"id": "review", "review": ["unrelated.json"]},
            command("write", ["apply", "foundation"], "review"),
        ]
        with self.assertRaises(ValueError):
            process.validate(recipe)

    def test_real_child_runs_once_and_logs_stay_private(self):
        runner = self.r.file("source/deploy/acceptance/run.py")
        runner.parent.mkdir(parents=True)
        runner.write_text("print('offline child complete')\n")
        c = self.controller()
        self.assertEqual(c.run(), 0)
        log = self.r.file("process/logs/assets.log")
        self.assertEqual(log.read_text().strip(), "offline child complete")
        # CLI sets the umask; direct tests explicitly assert the CLI-independent
        # writer also protects logs.
        self.assertEqual(log.stat().st_mode & 0o077, 0)
        with mock.patch.object(c, "execute") as execute:
            self.assertEqual(c.run(), 0)
            execute.assert_not_called()

    def test_review_and_config_tampering_stop_before_mutation(self):
        c = self.controller(self.reviewed_test(["trial", "--concurrency", "1"]))
        with mock.patch.object(c, "execute") as execute:
            self.assertEqual(c.run(), process.NEEDS_APPROVAL)
            execute.assert_not_called()
            self.r.write("cost-review.json", {"changed": True})
            with self.assertRaises(ValueError):
                c.approve("review")
            self.assertEqual(c.run(), process.NEEDS_APPROVAL)
            c.approve("review")
            self.r.write("approval-review.md", {"changed": True})
            self.assertEqual(c.run(), process.NEEDS_APPROVAL)
            execute.assert_not_called()
        self.r.write("config.json", {**self.r.c, "max_usd": 9})
        with self.assertRaises(ValueError):
            process.Process(self.r)

    def test_plan_review_binds_saved_plan_and_vars(self):
        self.r.write("plans/foundation/fixture.tfplan", {})
        self.r.write("plans/foundation/fixture.json", {})
        self.r.write("foundation.tfvars.json", {})
        self.r.write(
            "foundation-plan.json",
            {
                "plan": "plans/foundation/fixture.tfplan",
                "vars": "foundation.tfvars.json",
            },
        )
        c = self.controller()
        review = c.checkpoint({"review": ["foundation-plan.json"]})
        self.assertEqual(
            set(review["files"]),
            {
                "foundation-plan.json",
                "foundation.tfvars.json",
                "plans/foundation/fixture.tfplan",
                "plans/foundation/fixture.json",
            },
        )
        self.r.file("external-link").symlink_to(ROOT / "go.mod")
        with self.assertRaises(ValueError):
            c.checkpoint({"review": ["external-link"]})

    def test_auth_expiry_before_dispatch_resumes_without_duplicate_work(self):
        recipe = {**self.recipe, "steps": [command("preflight", ["preflight"])]}
        c = self.controller(recipe)
        with (
            mock.patch.object(self.r, "identity", side_effect=ValueError("expired")),
            mock.patch.object(c, "execute") as execute,
        ):
            self.assertEqual(c.run(), process.NEEDS_AUTH)
            execute.assert_not_called()
        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(c, "execute", return_value=0) as execute,
        ):
            self.assertEqual(c.run(), 0)
            self.assertEqual(c.run(), 0)
            self.assertEqual(
                [call.args[0]["id"] for call in execute.call_args_list],
                ["preflight", "collect"],
            )

    def test_unknown_mutation_is_never_replayed_and_failed_test_stays_failed_after_cleanup(
        self,
    ):
        c = self.controller(self.reviewed_test(["trial", "--concurrency", "4"]))
        c.run()
        c.approve("review")
        c.state["results"]["trial"] = {"status": "running"}
        with mock.patch.object(c, "execute") as execute:
            self.assertEqual(c.run(), process.NEEDS_RECONCILIATION)
            execute.assert_not_called()
        c.state["results"]["trial"] = {
            "status": "failed",
            "failure": True,
            "exit_code": 1,
        }
        with self.assertRaises(ValueError):
            c.reconcile("trial", ["run.json"])
        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(c, "execute", return_value=0),
        ):
            self.assertEqual(c.run(cleanup=True), process.FAILED)
        summary = self.r.read("process/summary.json")
        self.assertEqual(summary["status"], "finished_with_failures")
        self.assertEqual(summary["failed_steps"], ["trial"])

    def test_shutdown_false_receipt_is_failure_even_when_command_exits_zero(self):
        runner = self.r.file("source/deploy/acceptance/run.py")
        runner.parent.mkdir(parents=True)
        runner.write_text("print('shutdown command exited zero')\n")
        self.r.write("stop-proof.json", {"accepted": False})
        c = self.controller(self.reviewed_test(["shutdown", "--native-throttle"]))
        c.run()
        c.approve("review")
        with mock.patch.object(self.r, "identity"):
            self.assertEqual(c.run(), process.FAILED)
        self.assertTrue(
            self.r.read("process/summary.json")["steps"]["trial"]["failure"]
        )

    def test_timeout_terminates_child_and_never_replays_it(self):
        runner = self.r.file("source/deploy/acceptance/run.py")
        runner.parent.mkdir(parents=True)
        runner.write_text("import time\ntime.sleep(60)\n")
        recipe = {**self.recipe, "steps": [command("slow", ["assets"], timeout=1)]}
        c = self.controller(recipe)
        self.assertEqual(c.run(), process.FAILED)
        self.assertEqual(c.state["results"]["slow"]["exit_code"], 124)
        with mock.patch.object(c, "execute") as execute:
            self.assertEqual(c.run(), process.NEEDS_RECONCILIATION)
            execute.assert_not_called()

    def test_cleanup_after_lost_response_keeps_unknown_outcome_failed(self):
        c = self.controller()
        c.state["results"]["assets"] = {"status": "running"}
        with mock.patch.object(c, "execute", return_value=0):
            self.assertEqual(c.run(cleanup=True), process.FAILED)
        self.assertEqual(c.state["status"], "finished_with_failures")

    def test_cleanup_reuses_only_manifest_owned_by_this_run(self):
        c = self.controller()
        self.r.write(
            "frozen/manifest.json",
            {
                "run_id": self.r.id,
                "account_id": self.r.c["account_id"],
                "region": self.r.c["region"],
            },
        )
        step = command("freeze-for-cleanup", ["freeze"])
        result = {"log": "process/logs/freeze-for-cleanup.log"}
        self.assertEqual(c.execute(step, result), 0)
        self.assertTrue(self.r.file(result["log"]).is_file())
        self.r.write("frozen/manifest.json", {"run_id": "another-run"})
        with self.assertRaises(ValueError):
            c.execute(step, result)

    def test_operator_reconciliation_preserves_failed_attempt_and_binds_evidence(self):
        c = self.controller()
        c.state["results"]["assets"] = {
            "status": "failed",
            "failure": True,
            "exit_code": 1,
        }
        self.r.write("assets-proof.json", {"verified": True})
        c.reconcile("assets", ["assets-proof.json"])
        result = c.state["results"]["assets"]
        self.assertEqual(result["status"], "operator_reconciled")
        self.assertTrue(result["failure"])
        self.assertEqual(
            result["evidence"]["assets-proof.json"],
            digest(self.r.file("assets-proof.json")),
        )


if __name__ == "__main__":
    unittest.main()
