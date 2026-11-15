"""Offline phase/ownership gates; these fixtures do not claim AWS acceptance."""

import copy
import datetime as dt
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

from deploy.acceptance import approvals, cloud_telemetry, cold, pricing, run
from deploy.acceptance.common import ROOT, Run, digest, now


class ColdOperatorTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        path = Path(self.temp.name)
        path.chmod(0o700)
        (path/"config.json").write_text(json.dumps({
            "account_id": "123456789012", "deploy_role_arn": "arn:aws:iam::123456789012:role/test/deploy",
            "runtime_boundary_arn": "arn:aws:iam::123456789012:policy/test/Boundary",
            "backend_bucket": "fixture-state-123456789012", "ami_id": "ami-0123456789abcdef0"}))
        (path/"run.json").write_text(json.dumps({"run_id": "cold-fixture", "source": "a"*40,
            "native_config": "/fixture/config", "native_credentials": "/fixture/credentials"}))
        self.r = Run(path)
        pricing.review(self.r)
        self.case = "cold-cloud-flexible"
        self.name = self.case+"-rep1"
        self.path = f"trials/{self.name}/cold-state.json"
        self.state = {"run_id": self.r.id, "source": self.r.meta["source"], "variant": "flexible",
                      "pending": None, "done": {}, "jobs": [], "manifest_sha256": "f"*64,
                      "latest": {"snapshot": {"chunks": [{"Location": {"Pack": {"ID": "p", "Generation": "g"}}}],
                         "packs": [{"Ref": {"ID": "p", "Generation": "g"}, "Class": "STANDARD",
                                    "ProtectedUntil": (now()-dt.timedelta(minutes=1)).isoformat()}],
                         "bounds": {"Lower": now().isoformat(), "Upper": now().isoformat()}}}}
        self.r.write(self.path, self.state)
        self.policy = {"revision": 1, "tiering": {"enabled": False, "auto_restore": False},
                       "compression": {"enabled": True}, "chunking": {}}

    def review(self, stage):
        self.r.write("cold-plan.json", {"slot": self.name, "stage": stage, "policy": self.policy,
                     "state_sha256": digest(self.r.file(self.path))})
        approvals.record(self.r, "cold-step")

    def test_envelope_does_not_query_pricing_or_require_detailed_estimate(self):
        with mock.patch.object(self.r, "aws") as aws:
            pricing.review(self.r)
        aws.assert_not_called()
        review = self.r.read("cost-review.json")
        self.assertEqual(review["envelope_usd"], 20)
        self.assertIsNone(review["estimate_usd"])
        self.assertFalse(review["hard_billing_cap"])
        self.assertFalse(review["detailed_estimate_required"])
        self.r.c["max_usd"] = 21
        with self.assertRaises(ValueError): pricing.review(self.r)

    def test_commands_are_independent_and_mutations_confirm_exact_run(self):
        for action in ("cold-status", "cold-plan", "cold-step", "cold-reconcile"):
            argv = ["--run-dir", str(self.r.path), action, "--case-id", self.case, "--repetition", "1"]
            if action in {"cold-plan", "cold-step"}: argv += ["--stage", "read"]
            if action == "cold-step": argv += ["--confirm-run", self.r.id]
            args = run.parser().parse_args(argv)
            self.assertEqual(args.command, action)
        self.assertIn("cold-step", run.MUTATIONS)
        self.assertFalse(self.r.file("process/state.json").exists())

    def test_pending_mutation_blocks_plan_and_never_dispatches(self):
        self.state["pending"] = "put-cold"
        self.r.write(self.path, self.state)
        with mock.patch.object(cold, "admin") as admin, self.assertRaisesRegex(ValueError, "Unknown outcome"):
            cold.plan(self.r, self.case, 1, "put-cold")
        admin.assert_not_called()

    def test_plan_requires_real_protection_wait_and_binds_state(self):
        with mock.patch.object(cold, "admin", return_value=self.policy):
            with self.assertRaisesRegex(ValueError, "fresh cold-status"):
                cold.plan(self.r, self.case, 1, "transition")
            self.state.update(transition_ready=True, status_at=now().isoformat())
            self.r.write(self.path, self.state)
            cold.plan(self.r, self.case, 1, "transition")
        approvals.record(self.r, "cold-step")
        self.r.write("cold-plan.json", {"changed": True})
        with self.assertRaisesRegex(ValueError, "changed"):
            approvals.check(self.r, "cold-step")

    def test_state_changed_after_approval_cannot_dispatch(self):
        self.review("put-cold")
        self.state["done"]["transition"] = True
        self.r.write(self.path, self.state)
        with mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "live_window"), \
             mock.patch.object(cold, "phase") as phase, self.assertRaisesRegex(ValueError, "plan/state changed"):
            cold.step(self.r, self.case, 1, "put-cold")
        phase.assert_not_called()

    def test_missing_prerequisite_never_calls_data_phase(self):
        self.review("part-restored")
        with mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "live_window"), \
             mock.patch.object(cold, "phase") as phase, self.assertRaisesRegex(ValueError, "Earlier Cold"):
            cold.step(self.r, self.case, 1, "part-restored")
        phase.assert_not_called()

    def test_lost_write_response_retains_pending_and_resume_uses_receipt(self):
        self.state["done"]["transition"] = True
        snap = self.state["latest"]["snapshot"]
        snap["packs"][0]["Class"] = "GLACIER"
        snap["physical"] = copy.deepcopy(snap["packs"])
        self.r.write(self.path, self.state)
        self.review("put-cold")
        with mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "live_window"), \
             mock.patch.object(cold, "phase", side_effect=ValueError("lost response")) as phase:
            with self.assertRaisesRegex(ValueError, "lost response"):
                cold.step(self.r, self.case, 1, "put-cold")
        self.assertEqual(phase.call_count, 1)
        self.assertEqual(self.r.read(self.path)["pending"], "put-cold")
        self.assertNotIn("put-cold", self.r.read(self.path)["done"])

    def test_policy_restore_uses_owned_revision_and_rejects_concurrent_change(self):
        self.state.update(policy_original=self.policy, policy_owned={**self.policy, "revision": 2})
        with mock.patch.object(cold, "admin", return_value={**self.policy, "revision": 3}) as admin:
            with self.assertRaisesRegex(ValueError, "Concurrent policy"):
                cold.restore_policy(self.r, f"trials/{self.name}", self.state)
        self.assertEqual(admin.call_count, 1)

    def test_foreign_source_and_run_rejected(self):
        self.state["source"] = "b"*40
        self.r.write(self.path, self.state)
        with self.assertRaisesRegex(ValueError, "another source/run"):
            cold.slot(self.r, self.case, 1)

    def test_finish_cannot_promote_restore_get_only_to_full_lifecycle(self):
        self.state["done"] = {"transition": True, "read": True}
        self.r.write(self.path, self.state)
        self.review("finish")
        with mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "live_window"), \
             mock.patch.object(cold, "phase") as phase, self.assertRaisesRegex(ValueError, "incomplete"):
            cold.step(self.r, self.case, 1, "finish")
        phase.assert_not_called()
        self.assertFalse(self.r.file(f"trials/{self.name}/case-verdict.json").exists())
        self.assertIsNone(self.r.read(self.path)["pending"])

    def test_waiting_for_physical_transition_does_not_lock_or_dispatch_put(self):
        self.state["done"]["transition"] = True
        self.state["latest"]["snapshot"]["physical"] = copy.deepcopy(self.state["latest"]["snapshot"]["packs"])
        self.r.write(self.path, self.state)
        self.review("put-cold")
        with mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "live_window"), \
             mock.patch.object(cold, "phase") as phase, self.assertRaisesRegex(ValueError, "not yet verified"):
            cold.step(self.r, self.case, 1, "put-cold")
        phase.assert_not_called()
        self.assertIsNone(self.r.read(self.path)["pending"])

    def test_tiering_restores_disabled_policy_before_enqueuing_pinned_jobs(self):
        for fail_metrics in (False, True):
            with self.subTest(fail_metrics=fail_metrics):
                self.r.write(self.path, self.state)
                # Separate native attempt paths for the independent fixture run.
                for name in ("transition-policy-intent.json", "job-0-0-intent.json", "job-0-0.json"):
                    self.r.file(f"trials/{self.name}/"+name).unlink(missing_ok=True)
                self.review("transition")
                current = copy.deepcopy(self.policy)
                def admin(r, path, body=None, method=None):
                    nonlocal current
                    if path == "policy" and method == "PUT":
                        self.assertEqual(body["expected_revision"], current["revision"])
                        current = {**current, **{k:v for k,v in body.items() if k != "expected_revision"},
                                   "revision": current["revision"]+1}
                        return copy.deepcopy(current)
                    if path == "policy": return copy.deepcopy(current)
                    if path.startswith("cluster/nodes"):
                        return {"items": [{"ready": True, "draining": False, "active_requests": 0}]}
                    self.assertEqual(path, "jobs")
                    self.assertFalse(current["tiering"]["enabled"])
                    self.assertEqual(body["policy_revision"], 2)
                    self.assertEqual(body["params"], {"pack_id": "p", "generation": "g"})
                    return {"job_id": "own-job"}
                prefix = "telemetry/"+str(fail_metrics)
                with mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "live_window"), \
                     mock.patch.object(cold, "admin", side_effect=admin), \
                     mock.patch.object(cloud_telemetry, "start", return_value={"prefix": prefix}), \
                     mock.patch.object(cloud_telemetry, "stop"), \
                     mock.patch.object(cloud_telemetry, "verify", side_effect=ValueError("missing clock") if fail_metrics else None):
                    if fail_metrics:
                        with self.assertRaisesRegex(ValueError, "missing clock"):
                            cold.step(self.r, self.case, 1, "transition")
                    else:
                        cold.step(self.r, self.case, 1, "transition")
                state = self.r.read(self.path)
                self.assertEqual(state["jobs"], ["own-job"])
                self.assertIsNone(state["policy_owned"])
                self.assertEqual("transition" in state["done"], not fail_metrics)
                self.assertEqual(state["pending"], "transition" if fail_metrics else None)


if __name__ == "__main__":
    unittest.main()
