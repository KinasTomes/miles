"""Offline boundaries for operator-driven benchmark modules; no AWS calls."""

import copy
import datetime as dt
import json
import os
from pathlib import Path
import tempfile
import subprocess
import sys
import unittest
from unittest import mock

from deploy.acceptance import approvals, budget, direct, run, trial_state
from deploy.acceptance.common import ROOT, Run, digest, now


class ModuleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        path = Path(self.temp.name); path.chmod(0o700)
        (path/"config.json").write_text(json.dumps({
            "account_id": "123456789012", "deploy_role_arn": "arn:aws:iam::123456789012:role/test/deploy",
            "runtime_boundary_arn": "arn:aws:iam::123456789012:policy/test/Boundary",
            "backend_bucket": "fixture-state-123456789012", "ami_id": "ami-0123456789abcdef0"}))
        (path/"run.json").write_text(json.dumps({"run_id": "focused-fixture", "source": "a"*40,
            "native_config": "/fixture/config", "native_credentials": "/fixture/credentials"}))
        self.r = Run(path)
        self.r.write("cost-review.json", {"fixture": True})
        self.r.write("approval-review.md", "Review scope")
        self.r.write("init-complete.json", {"source": "a"*40, "config_sha256": digest(path/"config.json")})

    def test_standalone_approval_needs_no_process_state_and_detects_drift(self):
        approvals.record(self.r, "trial")
        approvals.check(self.r, "trial")
        self.assertFalse(self.r.file("process/state.json").exists())
        self.r.write("cost-review.json", {"changed": True})
        with self.assertRaisesRegex(ValueError, "changed"):
            approvals.check(self.r, "trial")

    def test_standalone_approval_expires(self):
        approvals.record(self.r, "trial")
        with mock.patch.object(approvals, "now", return_value=now()+dt.timedelta(minutes=46)):
            with self.assertRaisesRegex(ValueError, "expired"):
                approvals.check(self.r, "trial")

    def test_direct_commands_are_independent_and_mutations_require_confirmation(self):
        parser = run.parser()
        for command in ("direct-provision", "direct-cleanup"):
            args = parser.parse_args(["--run-dir", str(self.r.path), command, "--confirm-run", self.r.id])
            self.assertEqual(args.command, command)
            self.assertIn(command, run.MUTATIONS)
        args = parser.parse_args(["--run-dir", str(self.r.path), "approve", "--action", "benchmark-case", "--confirm-run", self.r.id])
        self.assertEqual(args.action, "benchmark-case")

    def test_budget_60_unique_slots_and_24_pairs_unknown_rates_block_approval(self):
        matrix = json.loads((ROOT/"benchmarks/workload-matrix.json").read_text())
        inputs = json.loads((ROOT/"deploy/acceptance/benchmark-budget.example.json").read_text())
        result = budget.estimate(matrix, inputs)
        self.assertEqual(result["slot_count"], 60)
        self.assertEqual(len(set(result["slots"])), 60)
        self.assertEqual(result["direct_pairs"], 24)
        self.assertEqual(result["status"], "UNKNOWN")
        self.assertIsNone(result["gross_total_usd"])
        self.assertFalse(result["approval"])
        self.assertFalse(result["window_fit"])
        self.r.write("benchmark-budget.json", result)
        with self.assertRaises(ValueError):
            approvals.record(self.r, "benchmark-case")

    def test_budget_cli_is_standalone_outside_repository_without_pythonpath(self):
        output = self.r.path/"offline-budget.json"
        env = {k: v for k, v in os.environ.items() if k != "PYTHONPATH"}
        result = subprocess.run([sys.executable, "-B", ROOT/"deploy/acceptance/budget.py", "--inputs",
            ROOT/"deploy/acceptance/benchmark-budget.example.json", "--out", output],
            cwd=self.r.path, env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(output.read_text())["slot_count"], 60)
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)

    def test_budget_minimum_duration_is_not_limited_to_live_window(self):
        matrix = json.loads((ROOT/"benchmarks/workload-matrix.json").read_text())
        inputs = json.loads((ROOT/"deploy/acceptance/benchmark-budget.example.json").read_text())
        inputs["paid_hours"] = 1
        result = budget.estimate(matrix, inputs)
        lines = {line["id"]: line for line in result["line_items"]}
        from decimal import Decimal
        expected = Decimal(inputs["cold_bytes_per_slot"])*6/Decimal(1024**3)*3
        self.assertEqual(Decimal(lines["ir_gb_months_90d"]["quantity"]), expected)
        self.assertGreater(Decimal(lines["flexible_restore_gb"]["quantity"]), 0)
        self.assertGreater(Decimal(lines["restored_standard_gb_months"]["quantity"]), 0)

    def test_budget_rejects_missing_dimension_and_negative_nan(self):
        matrix = json.loads((ROOT/"benchmarks/workload-matrix.json").read_text())
        inputs = json.loads((ROOT/"deploy/acceptance/benchmark-budget.example.json").read_text())
        del inputs["rates"]["flexible_restore_gb"]
        with self.assertRaises(ValueError): budget.estimate(matrix, inputs)
        for value in (-1, "NaN", "Infinity"):
            with self.subTest(value=value), self.assertRaises(ValueError): budget.number(value)

    def test_direct_bucket_name_is_run_bound_and_legacy_bucket_cannot_be_adopted(self):
        expected = direct.name(self.r)
        self.assertLessEqual(len(expected), 63)
        p = direct.plan(self.r)
        self.assertNotIn("pilot-direct", p["bucket"])
        self.r.write("direct-created.json", {**p, "kms_key": "fixture"})
        with mock.patch.object(self.r, "aws", side_effect=[{}, {"TagSet": [{"Key": "Project", "Value": "Miles"}]}]):
            with self.assertRaisesRegex(ValueError, "ownership"):
                direct.owned(self.r)

    def test_lost_create_ack_retains_intent_and_cannot_replay(self):
        direct.plan(self.r)
        self.r.write("active-outputs.json", {"runtime_inventory": {"value": {"kms_key_arn": "fixture-kms"}}})
        def aws(service, operation, *args, **kwargs):
            if operation == "list-buckets": return {"Buckets": []}
            if operation == "create-bucket": raise TimeoutError("lost ACK")
            self.fail("unexpected mutation: " + operation)
        with mock.patch.object(approvals, "check"), mock.patch.object(self.r, "identity"), \
             mock.patch.object(self.r, "live_window"), mock.patch.object(self.r, "aws", side_effect=aws):
            with self.assertRaises(TimeoutError): direct.provision(self.r)
            self.assertTrue(self.r.file("direct-create-intent.json").exists())
            self.assertFalse(self.r.file("direct-created.json").exists())
            with self.assertRaisesRegex(ValueError, "already dispatched"):
                direct.provision(self.r)

    def test_existing_bucket_is_rejected_before_create(self):
        direct.plan(self.r)
        with mock.patch.object(approvals, "check"), mock.patch.object(self.r, "identity"), \
             mock.patch.object(self.r, "live_window"), mock.patch.object(self.r, "aws",
                  return_value={"Buckets": [{"Name": direct.name(self.r)}]}) as aws:
            with self.assertRaisesRegex(ValueError, "do not adopt"): direct.provision(self.r)
        self.assertEqual(aws.call_count, 1)
        self.assertFalse(self.r.file("direct-create-intent.json").exists())

    def test_cleanup_refuses_new_versions_and_partial_delete_errors(self):
        versions = [{"Key": "test", "VersionId": "v1"}]
        self.r.write("direct-cleanup-plan.json", {"versions": versions})
        with mock.patch.object(approvals, "check"), mock.patch.object(direct, "owned"), \
             mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "ssh", return_value=b'{"active":[]}'), \
             mock.patch.object(direct, "contents", return_value=versions+[{"Key": "test", "VersionId": "v2"}]), \
             mock.patch.object(self.r, "aws") as aws:
            with self.assertRaisesRegex(ValueError, "changed"): direct.cleanup(self.r)
            aws.assert_not_called()
        with mock.patch.object(approvals, "check"), mock.patch.object(direct, "owned"), \
             mock.patch.object(self.r, "identity"), mock.patch.object(self.r, "ssh", return_value=b'{"active":[]}'), \
             mock.patch.object(direct, "contents", return_value=versions), \
             mock.patch.object(self.r, "aws", return_value={"Errors": [{"Code": "AccessDenied"}]}) as aws:
            with self.assertRaisesRegex(ValueError, "Partial"): direct.cleanup(self.r)
            self.assertEqual(aws.call_count, 1)
            self.assertFalse(self.r.file("direct-absence.json").exists())

    def test_fresh_domains_are_unique_per_slot_and_share_only_within_trial(self):
        c = {"scenario": "domain", "variant": "shared-default"}
        a = trial_state.domains("run-a", "shared-rep1", c)
        b = trial_state.domains("run-a", "shared-rep2", c)
        self.assertEqual(a[0], a[1]); self.assertNotEqual(a[0], b[0])
        c["variant"] = "separated"
        self.assertNotEqual(*trial_state.domains("run-a", "separated-rep1", c))

    def test_node_report_rejects_stale_incomplete_duplicate_and_missing_samples(self):
        timestamp = now().isoformat()
        good = {"complete": True, "observed_at": timestamp,
                "nodes": [{"node_id": "cloud-a", "state": "succeeded", "sample": {"sampled_at": timestamp}}]}
        self.assertEqual(set(trial_state.cache_nodes(good)), {"cloud-a"})
        bad_reports = [dict(good, complete=False), dict(good, nodes=[]),
                       dict(good, nodes=good["nodes"]*2), dict(good, observed_at="2000-01-01T00:00:00Z")]
        missing = copy.deepcopy(good); missing["nodes"][0]["sample"] = None; bad_reports.append(missing)
        for report in bad_reports:
            with self.subTest(report=report), self.assertRaises(ValueError): trial_state.cache_nodes(report)

    def test_direct_lifecycle_reads_back_configuration_then_deletes_only_reviewed_versions(self):
        direct.plan(self.r)
        self.r.write("active-outputs.json", {"runtime_inventory": {"value": {"kms_key_arn": "fixture-kms"}}})
        self.r.write("stop-proof.json", {"stopped": True})
        state = {"created": False, "tags": [], "versions": [{"Key": "warmbench/test", "VersionId": "v1"}]}
        def aws(service, op, *args, **kwargs):
            if op == "list-buckets":
                self.assertTrue(kwargs.get("audit"), "Account listing belongs to the existing read-only auditor")
                return {"Buckets": [{"Name": direct.name(self.r)}] if state["created"] else []}
            if "--bucket" in args:
                self.assertEqual(args[args.index("--bucket")+1], direct.name(self.r))
            if op == "create-bucket": state["created"] = True; return {}
            if op == "put-bucket-tagging": state["tags"] = json.loads(args[args.index("--tagging")+1])["TagSet"]; return {}
            if op.startswith("put-"): return {}
            if op == "head-bucket": return {}
            if op == "get-bucket-tagging": return {"TagSet": state["tags"]}
            if op == "get-bucket-versioning": return {"Status": "Enabled"}
            if op == "get-bucket-encryption": return {"ServerSideEncryptionConfiguration": {"Rules": [{"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "aws:kms", "KMSMasterKeyID": "fixture-kms"}}]}}
            if op == "get-public-access-block": return {"PublicAccessBlockConfiguration": {k: True for k in ("BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets")}}
            if op == "list-object-versions": return {"Versions": state["versions"]}
            if op == "list-multipart-uploads": return {"Uploads": []}
            if op == "delete-objects":
                self.assertEqual(json.loads(args[args.index("--delete")+1])["Objects"], state["versions"])
                state["versions"] = []; return {"Deleted": [{"Key": "warmbench/test", "VersionId": "v1"}]}
            if op == "delete-bucket":
                self.assertEqual(state["versions"], []); state["created"] = False; return {}
            self.fail(op)
        with mock.patch.object(approvals, "check"), mock.patch.object(self.r, "identity"), \
             mock.patch.object(self.r, "live_window"), mock.patch.object(self.r, "aws", side_effect=aws), \
             mock.patch.object(self.r, "ssh", return_value=b'{"active":[]}'):
            direct.provision(self.r)
            direct.cleanup_plan(self.r)
            direct.cleanup(self.r)
        self.assertTrue(self.r.read("direct-absence.json")["absent"])

    def test_cache_reset_needs_exact_three_nodes_and_new_empty_samples(self):
        ids = ["cloud-a", "cloud-b", "edge"]
        at = now()
        def report(time, entries):
            return {"complete": True, "observed_at": time.isoformat(), "nodes": [
                {"node_id": n, "incarnation": "boot-"+n, "state": "succeeded", "sample": {
                    "sampled_at": time.isoformat(), "hits": 1, "misses": 1, "lookups": 2,
                    "errors": 0, "puts": 2, "memory": {"entries": entries, "used_bytes": entries*2},
                    "disk": {"entries": entries, "used_bytes": entries*2}}} for n in ids]}
        purged = report(at, 0)
        def admin(r, path, body=None):
            if path.startswith("cluster/nodes"):
                return {"items": [{"node_id": n, "ready": True, "draining": False, "active_requests": 0} for n in ids]}
            if path == "cache/purge": return {"job_id": "purge-one"}
            if path.startswith("jobs/"):
                return {"state": "succeeded", "finished_at": (at-dt.timedelta(seconds=1)).isoformat(),
                    "result": {"scope": "cluster", "nodes": [{"node_id": n, "state": "succeeded", "result": {"purged": True, "dry_run": False}} for n in ids]}}
            return purged
        def aws(service, op, *args, **kwargs):
            if op == "scan": return {"Items": []}
            if op == "describe-target-health": return {"TargetHealthDescriptions": [{"Target": {"Id": n}, "TargetHealth": {"State": "healthy"}} for n in ids[:2]]}
            self.fail(op)
        def outputs(stage):
            return {"runtime_inventory": {"target_group_arn": "fixture-tg"}} if stage == "active" else {"acceptance_nodes": {"edge": {"instance_id": "edge"}}}
        def wait(label, f, timeout): self.assertTrue(f())
        with mock.patch.object(trial_state, "native_binding", return_value={"tables": {"Chunks": "fixture-chunks"}}), \
             mock.patch.object(self.r, "aws", side_effect=aws), mock.patch.object(self.r, "outputs", side_effect=outputs), \
             mock.patch.object(self.r, "wait", side_effect=wait), mock.patch.object(self.r, "live_window"):
            proof = trial_state.prepare(self.r, "warm-cloud-c1-rep1", {"scenario": "warm", "variant": "warm-c1"}, admin)
        self.assertEqual(proof["cloud_nodes"], ids[:2])
        self.assertTrue(self.r.file("trials/warm-cloud-c1-rep1/purge-intent.json").exists())
        incomplete = copy.deepcopy(purged); incomplete["nodes"].pop()
        with mock.patch.object(trial_state, "native_binding", return_value={"tables": {"Chunks": "fixture-chunks"}}), \
             mock.patch.object(self.r, "aws", side_effect=aws), mock.patch.object(self.r, "outputs", side_effect=outputs):
            with self.assertRaisesRegex(ValueError, "differs"):
                trial_state.prepare(self.r, "warm-cloud-c1-rep2", {"scenario": "warm", "variant": "warm-c1"},
                    lambda r,p,b=None: incomplete if p.startswith("cache/") else admin(r,p,b))


if __name__ == "__main__": unittest.main()
