"""No AWS required: strict full-matrix scheduling and approval-fail-closed tests."""

import copy
import hashlib
import json
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch

from deploy.acceptance import matrix_plan
from deploy.acceptance.common import ROOT

MATRIX = ROOT / "benchmarks/workload-matrix.json"
TEMPLATE = ROOT / "deploy/acceptance/templates/focused-get-shutdown.json"
SHA = "b" * 40


class FullMatrixOfflinePlanTests(unittest.TestCase):
    def setUp(self):
        self.matrix = json.loads(MATRIX.read_text())
        self.recipe = json.loads(TEMPLATE.read_text())

    def build(self, recipe=TEMPLATE):
        return matrix_plan.build(self.matrix, recipe, SHA)

    def test_frozen_manifest_covers_60_slots_without_claiming_approval(self):
        out = self.build()
        self.assertEqual(out["total_trial_slots"], 60)
        self.assertEqual(len({x["slot"] for x in out["slots"]}), 60)
        self.assertEqual(out["matched_direct_s3_pairs"], 24)
        self.assertEqual(out["warmbench_slots_not_integrated"], 36)
        self.assertEqual(out["cold_recovery_slots_blocked"], 24)
        self.assertFalse(out["aws_approval"])
        self.assertFalse(out["benchmark_executed"])
        self.assertFalse(out["full_run_executable"])
        self.assertEqual(len(out["slots"][:3]), 3)
        self.assertEqual([x["repetition"] for x in out["slots"][:3]], [1, 2, 3])
        self.assertTrue(all(x["result"] == "not-executed" for x in out["slots"]))
        self.assertTrue(all(x["kit_adapter_status"] != "integrated"
                            for x in out["slots"]))

    def test_non_executable_argv_templates_match_each_warmbench_variant(self):
        out = self.build()
        cases = {c["case_id"]: c for c in out["slots"]}
        warm = cases["warm-cloud-c4"]["non_executable_argv_templates"]
        self.assertEqual([x["role"] for x in warm], ["miles", "direct-s3"])
        self.assertIn("warm-c4", warm[0]["argv_template"])
        self.assertIn("4", warm[0]["argv_template"])
        self.assertIn("<PRIVATE_DIRECT_S3_CONFIG_0600>", warm[1]["argv_template"])
        multipart = cases["multipart-threshold-plus-one"]["non_executable_argv_templates"]
        self.assertIn("threshold-plus-one", multipart[0]["argv_template"])
        clone = cases["edge-clone-upload"]["non_executable_argv_templates"]
        self.assertEqual(len(clone), 1)
        self.assertIn("--source-prefix", clone[0]["argv_template"])
        self.assertIn("--second-config", clone[0]["argv_template"])
        domain = cases["domain-separated"]["non_executable_argv_templates"]
        self.assertIn("--second-config", domain[0]["argv_template"])
        self.assertEqual(cases["cold-cloud-flexible"]["non_executable_argv_templates"], [])
        self.assertEqual(cases["recovery-edge-r2"]["non_executable_argv_templates"], [])

    def test_cold_and_recovery_proofs_remain_blocked(self):
        out = self.build()
        for slot in out["slots"]:
            if slot["phase"] != "G4":
                continue
            self.assertEqual(slot["runner"], "requires-runner")
            self.assertEqual(slot["kit_adapter_status"], "specialized-runner-missing")
            proofs = slot["evidence_requirements"]
            if slot["scenario"] == "cold":
                self.assertIn("restoration-completion-expiry-and-revival", proofs)
            else:
                self.assertIn("lease-fence-and-journal-event-samples", proofs)

    def test_data_and_comparator_bindings(self):
        out = self.build()
        cases = {c["case_id"]: c for c in out["slots"]}
        self.assertEqual(cases["multipart-threshold-equal"]["dataset"], "boundary-8mib")
        self.assertIn("8mib-boundary-manifest",
                      cases["multipart-threshold-equal"]["evidence_requirements"])
        self.assertEqual(cases["warm-cloud-c4"]["concurrency"], 4)
        self.assertTrue(cases["warm-cloud-c4"]["needs_matched_direct_s3"])
        self.assertFalse(cases["domain-separated"]["needs_matched_direct_s3"])
        self.assertIn("runtime-dedup-domain-and-per-put-physical-counters",
                      cases["domain-separated"]["evidence_requirements"])
        self.assertIn("source-prefix-and-verified-get-put-hash-receipt",
                      cases["edge-clone-upload"]["evidence_requirements"])

    def test_reject_matrix_false_approval_and_altered_runner(self):
        for mutate in (
            lambda m: m.update(aws_approval=True),
            lambda m: m["cases"][10].update(runner="warmbench"),
            lambda m: m["protocol"].update(repetitions=1),
            lambda m: m["cases"].append(copy.deepcopy(m["cases"][0])),
        ):
            with self.subTest(mutate=mutate):
                modified = copy.deepcopy(self.matrix)
                mutate(modified)
                with self.assertRaises(ValueError):
                    matrix_plan.build(modified, TEMPLATE, SHA)

    def test_reject_invalid_source_sha(self):
        with self.assertRaises(ValueError):
            matrix_plan.build(self.matrix, TEMPLATE, "short")
        with self.assertRaises(ValueError):
            matrix_plan.build(self.matrix, TEMPLATE, "A" * 40)

    def test_reject_tampered_recipe_missing_cleanup(self):
        with tempfile.TemporaryDirectory() as temp:
            modified = copy.deepcopy(self.recipe)
            modified["cleanup_steps"] = [
                s for s in modified["cleanup_steps"]
                if s.get("command") != ["cleanup"]
            ]
            path = Path(temp) / "recipe.json"
            path.write_text(json.dumps(modified))
            with self.assertRaisesRegex(ValueError, "lacks full cleanup"):
                self.build(path)

    def test_reject_recipe_missing_approval(self):
        with tempfile.TemporaryDirectory() as temp:
            modified = copy.deepcopy(self.recipe)
            modified["steps"] = [
                s for s in modified["steps"] if s["id"] != "approve-active"
            ]
            path = Path(temp) / "recipe.json"
            path.write_text(json.dumps(modified))
            with self.assertRaises(ValueError):
                self.build(path)

    def test_reject_recipe_with_unreviewed_shell_step(self):
        with tempfile.TemporaryDirectory() as temp:
            modified = copy.deepcopy(self.recipe)
            modified["steps"].insert(0, {
                "id": "unsafe",
                "command": ["sh", "-c", "aws s3 rm s3://other"],
                "timeout_seconds": 60,
            })
            path = Path(temp) / "recipe.json"
            path.write_text(json.dumps(modified))
            with self.assertRaises(ValueError):
                self.build(path)

    def test_private_manifest_is_complete_and_hashed(self):
        with tempfile.TemporaryDirectory() as temp:
            out = Path(temp) / "run-private"
            result = matrix_plan.write_private(out, MATRIX, TEMPLATE, SHA)
            path = out / "offline-execution-manifest.json"
            self.assertEqual(stat.S_IMODE(out.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            stored = json.loads(path.read_text())
            self.assertEqual(stored["slots"], result["slots"])
            self.assertEqual(stored["input_sha256"]["matrix"],
                             hashlib.sha256(MATRIX.read_bytes()).hexdigest())
            self.assertFalse(stored["full_run_executable"])
            with self.assertRaisesRegex(ValueError, "new private"):
                matrix_plan.write_private(out, MATRIX, TEMPLATE, SHA)

    def test_reject_symlink_output_directory_and_parent(self):
        with tempfile.TemporaryDirectory() as temp:
            parent = Path(temp)
            alias = parent / "alias"
            alias.symlink_to(parent, target_is_directory=True)
            with self.assertRaises(ValueError):
                matrix_plan.write_private(alias / "new", MATRIX, TEMPLATE, SHA)
            existing = parent / "existing"
            existing.symlink_to(parent, target_is_directory=True)
            with self.assertRaises(ValueError):
                matrix_plan.write_private(existing, MATRIX, TEMPLATE, SHA)

    def test_build_is_pure_and_does_not_call_network_or_shell(self):
        with patch("subprocess.run", side_effect=AssertionError("shell forbidden")),              patch("subprocess.check_output", side_effect=AssertionError("shell forbidden")):
            result = self.build()
        self.assertFalse(result["full_run_executable"])


if __name__ == "__main__":
    unittest.main()
