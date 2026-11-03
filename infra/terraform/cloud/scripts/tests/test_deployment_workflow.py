"""Offline proofs for installation identity, retries, secrets and destructive gates."""
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from test_installation import fixture, installation as renderer

CLOUD = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("workflow", CLOUD / "scripts/deploy-installation.py")
workflow = importlib.util.module_from_spec(spec)
spec.loader.exec_module(workflow)


class WorkflowTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.config = fixture()
        self.file = self.root / "inputs.json"
        self.file.write_text(json.dumps(self.config))
        self.app = workflow.Installation(self.file, self.root / "private")

    def test_workload_refuses_root_user_other_account_and_other_role(self):
        for arn in ["arn:aws:iam::123456789012:root", self.config["operator"]["user_arn"],
                    "arn:aws:sts::123456789012:assumed-role/Other/session"]:
            with patch.object(self.app, "aws", return_value={"Account":"123456789012", "Arn":arn}):
                with self.assertRaises(ValueError):
                    self.app.identity()
        arn = "arn:aws:sts::123456789012:assumed-role/miles-member-deploy/session"
        with patch.object(self.app, "aws", return_value={"Account":"123456789012", "Arn":arn}):
            self.app.identity()
        with patch.object(self.app, "aws", return_value={"Account":"210987654321", "Arn":arn}):
            with self.assertRaises(ValueError):
                self.app.identity()

    def test_deploy_role_cannot_collide_with_a_runtime_identity(self):
        self.config["operator"]["deploy_role_arn"] = "arn:aws:iam::123456789012:role/other-path/" + self.app.name + "-gateway"
        with self.assertRaisesRegex(ValueError, "separate"):
            renderer.render(self.config)

    def test_drain_leaves_cloud_invocation_for_its_asg_controller(self):
        with patch.object(self.app, "nodes", return_value=[("cloud-node","cloud"),("edge-node","edge")]), patch.object(self.app, "command") as command:
            self.app.drain()
            self.assertEqual(command.call_count, 1)
            self.assertEqual(command.call_args.args[0], "edge-node")

    def test_input_changes_require_reconciliation_but_grant_expiry_can_change(self):
        self.app.persist()
        config = dict(self.config, owner="another-member")
        self.file.write_text(json.dumps(config))
        with self.assertRaisesRegex(ValueError, "Configuration changed"):
            workflow.Installation(self.file, self.app.private)
        self.config["operator"]["cleanup_expires_at"] = (datetime.now(timezone.utc)+timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.file.write_text(json.dumps(self.config))
        workflow.Installation(self.file, self.app.private)

    def test_aws_removes_ambient_credentials_and_redacts_error_material(self):
        result = subprocess.CompletedProcess([], 1, "", "An error occurred (AccessDenied) secret=do-not-print")
        with patch.dict(os.environ, AWS_ACCESS_KEY_ID="wrong", AWS_SECRET_ACCESS_KEY="wrong", AWS_PROFILE="wrong"), patch.object(workflow.subprocess, "run", return_value=result) as call:
            with self.assertRaisesRegex(workflow.AWSFailure, "^AWS operation failed: AccessDenied$"):
                self.app.aws("sts", "get-caller-identity")
            self.assertNotIn("AWS_ACCESS_KEY_ID", call.call_args.kwargs["env"])
            self.assertIn(self.app.profile, call.call_args.args[0])

    def test_secret_retry_never_rotates_keys_and_rejects_different_value(self):
        with patch.object(self.app, "aws", return_value={}) as aws:
            self.app.secret("test-secret", {"key":"original"}, "keys")
            token = aws.call_args.args[-1]
        with patch.object(self.app, "aws", return_value={}) as aws:
            self.app.secret("test-secret", {"key":"replacement"}, "keys")
            self.assertEqual(aws.call_args.args[-1], token)
            self.assertEqual(json.loads((self.app.private/"keys.json").read_text()), {"key":"original"})
        with patch.object(self.app, "aws", side_effect=[{"VersionIdsToStages":{"v":["AWSCURRENT"]}}, {"SecretString":"{\"key\":\"different\"}"}]):
            with self.assertRaises(ValueError):
                self.app.secret("test-secret", {}, "keys")

    def test_pinned_artifact_retry_reads_exact_version_before_reuse(self):
        artifact = self.root / "artifact"
        artifact.write_bytes(b"release")
        def aws(*arguments):
            if arguments[1] == "head-object":
                return {"VersionId":"version-one"}
            self.assertEqual(arguments[arguments.index("--version-id")+1], "version-one")
            Path(arguments[-1]).write_bytes(b"release")
            return {}
        with patch.object(self.app, "aws", side_effect=aws):
            self.assertEqual(self.app.upload({"data_bucket_name":"owned"}, artifact, "releases/a"), "version-one")
        def corrupt(*arguments):
            if arguments[1] == "head-object":
                return {"VersionId":"version-one"}
            Path(arguments[-1]).write_bytes(b"other-release")
            return {}
        with patch.object(self.app, "aws", side_effect=corrupt):
            with self.assertRaises(ValueError):
                self.app.upload({"data_bucket_name":"owned"}, artifact, "releases/a")

    def test_acm_lost_ack_recovers_same_certificate_without_reimport(self):
        pki = self.app.private / "pki"
        pki.mkdir()
        (pki / "cloud.pem").write_text("test certificate\n")
        self.app.state["certificate_import_pending"] = True
        responses = [{"CertificateSummaryList":[{"CertificateArn":"cert-arn", "DomainName":"cloud."+self.app.name+".test"}]},
                     {"Tags":[{"Key":"Installation", "Value":self.app.name}, {"Key":"Project", "Value":"Miles"}]},
                     {"Certificate":"test certificate\n"}]
        with patch.object(self.app, "aws", side_effect=responses) as aws:
            self.app.import_certificate()
            self.assertEqual(self.app.state["certificate_arn"], "cert-arn")
            self.assertFalse(any("import-certificate" in call.args for call in aws.call_args_list))
        self.app.state.pop("certificate_arn")
        with patch.object(self.app, "aws", return_value={"CertificateSummaryList":[]}):
            with self.assertRaisesRegex(ValueError, "uncertain"):
                self.app.import_certificate()

    def plan_fixture(self, stage):
        file = self.app.private / (stage + ".tfplan")
        file.write_bytes(b"reviewed plan")
        digest = hashlib.sha256(file.read_bytes()).hexdigest()
        workflow.save(self.app.private / (stage + "-plan.json"), {"sha256":digest, "configuration_sha256":self.app.state["configuration_sha256"]})
        return digest

    def test_apply_requires_exact_plan_and_does_not_reset_existing_console_password(self):
        digest = self.plan_fixture("operator")
        with patch.object(self.app, "identity"), patch.object(self.app, "terraform") as tf, patch.object(self.app, "create_login") as login:
            with self.assertRaises(ValueError):
                self.app.apply("operator", "wrong-sha")
            tf.assert_not_called()
            self.app.apply("operator", digest)
            self.app.apply("operator", digest)
            self.assertEqual(tf.call_count, 1)  # Retry resumes post-apply work.
            self.assertEqual(login.call_count, 2)
        with patch.object(self.app, "aws", side_effect=[{"User":{"Arn":self.config["operator"]["user_arn"]}}, {"LoginProfile":{}}]) as aws:
            self.app.create_login()
            self.assertEqual(aws.call_count, 2)
            self.assertFalse((self.app.private/"console-login.json").exists())

    def test_destroy_requires_explicit_data_deletion_and_bounded_grant(self):
        digest = self.plan_fixture("destroy")
        with patch.object(self.app, "identity"), patch.object(self.app, "drain"), patch.object(self.app, "empty_bucket") as empty, patch.object(self.app, "terraform") as tf:
            with self.assertRaises(ValueError):
                self.app.apply("destroy", digest)
            empty.assert_not_called()
            tf.assert_not_called()
        for seconds in (-1, 4*3600+60):
            self.config["operator"]["cleanup_expires_at"] = (datetime.now(timezone.utc)+timedelta(seconds=seconds)).strftime("%Y-%m-%dT%H:%M:%SZ")
            with self.assertRaises(ValueError):
                renderer.render(self.config)

    def test_stage_order_and_pause_before_unprotect(self):
        for stage in ("state", "foundation", "active", "paused", "destroy"):
            with self.assertRaises(ValueError):
                self.app.plan(stage)
        with self.assertRaises(ValueError):
            self.app.prepare_cleanup()

    def test_version_delete_checks_per_object_errors(self):
        with patch.object(self.app, "inventory", return_value={"data_bucket_name":"owned"}), patch.object(self.app, "aws", side_effect=[{"Versions":[{"Key":"synthetic", "VersionId":"1"}]}, {"Errors":[{"Code":"AccessDenied"}]}]):
            with self.assertRaises(RuntimeError):
                self.app.empty_bucket()

    def test_policy_shards_and_cleanup_scope_fit_default_quotas(self):
        self.config["operator"]["cleanup_expires_at"] = (datetime.now(timezone.utc)+timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        output = renderer.render(self.config)
        documents = output["operator.tfvars.json"]["deploy_documents"]
        self.assertLessEqual(len(documents), 9)
        for document in documents.values():
            self.assertLessEqual(len(json.dumps(json.loads(document), separators=(",",":"))), 6144)
        for statement in output["iam/deploy-cleanup.json"]["Statement"]:
            self.assertIn("DateLessThan", statement["Condition"])
            self.assertNotIn("tfstate", json.dumps(statement))
        self.config["operator"].pop("cleanup_expires_at")
        self.assertNotIn("iam/deploy-cleanup.json", renderer.render(self.config))

    def test_pause_marker_cannot_authorize_cleanup_after_resuming(self):
        digest = self.plan_fixture("active")
        self.app.state.update(paused_applied=True, unprotect_applied=True)
        with patch.object(self.app, "identity"), patch.object(self.app, "terraform"):
            self.app.apply("active", digest)
        self.assertFalse(self.app.state["paused_applied"])
        self.assertFalse(self.app.state["unprotect_applied"])
        with self.assertRaises(ValueError):
            self.app.prepare_cleanup()
        self.app.state.update(prepared=True, provisioned=True)
        with self.assertRaisesRegex(ValueError, "cannot replace"):
            self.app.plan("foundation")

    def test_expired_active_window_blocks_reactivation(self):
        digest = self.plan_fixture("active")
        self.app.state["active_started_at"] = "2020-01-01T00:00:00+00:00"
        with patch.object(self.app, "identity"), patch.object(self.app, "terraform") as tf:
            with self.assertRaisesRegex(ValueError, "window elapsed"):
                self.app.apply("active", digest)
            tf.assert_not_called()

    def test_closeout_refuses_resources_still_in_state(self):
        self.app.state["destroy_applied"] = True
        workflow.save(self.app.private / "runtime-inventory.json", {})
        with patch.object(self.app, "identity"), patch.object(self.app, "terraform", return_value=json.dumps({"values":{"root_module":{"resources":[{"mode":"managed"}]}}})):
            with self.assertRaises(ValueError):
                self.app.closeout()

    def test_prepare_builds_distinct_private_pki_and_frozen_stacks_without_aws(self):
        self.config["operator"]["utc_reference"] = "169.254.169.123"
        self.file.write_text(json.dumps(self.config))
        app = workflow.Installation(self.file, self.app.private)
        original = workflow.run
        def local_run(arguments, **options):
            if arguments[0] == "git":
                return "a"*40 if arguments[1] == "rev-parse" else ""
            if arguments[0] == "chronyc":
                return "A9FEA97B,169.254.169.123,3,0,0,0,0,0,0,0,0,0,10,Normal"
            return original(arguments, **options)
        with patch.object(workflow, "run", side_effect=local_run), patch.object(workflow.shutil, "which", return_value="/usr/bin/tool"), patch.object(app, "aws") as aws:
            app.prepare()
            app.prepare()
            aws.assert_not_called()
        self.assertNotEqual((app.private/"pki/tls-ca.pem").read_bytes(), (app.private/"pki/identity-ca.pem").read_bytes())
        for leaf, authority in [("cloud","tls"), ("edge","tls"), ("provider","identity")]:
            subprocess.run(["openssl", "verify", "-CAfile", str(app.private/"pki"/(authority+"-ca.pem")), str(app.private/"pki"/(leaf+".pem"))], check=True, capture_output=True)
        self.assertEqual((app.private/"pki/provider.key").stat().st_mode & 0o777, 0o600)
        inputs = json.loads((app.private/"rendered/installation.tfvars.json").read_text())
        self.assertTrue(inputs["paused"])
        self.assertFalse(inputs["enable_edge"])
        self.assertFalse(inputs["enable_asg"])
        self.assertTrue((app.private/"cloud/modules/edge/main.tf").is_file())

    def test_protection_plan_rejects_any_unrelated_update(self):
        self.app.state.update(prepared=True, paused_applied=True)
        plan = {"format_version":"1.2", "complete":True, "errored":False,
                "resource_changes":[{"address":"module.data.aws_dynamodb_table.metadata[\"Jobs\"]", "mode":"managed", "type":"aws_dynamodb_table", "change":{"actions":["update"], "before":{"deletion_protection_enabled":True,"name":"owned"}, "after":{"deletion_protection_enabled":False,"name":"owned"}}}]}
        def terraform(stack, *arguments, **options):
            if arguments[0] == "plan":
                (self.app.private/"unprotect.tfplan").write_bytes(b"reviewed-plan")
                return ""
            return json.dumps(plan)
        with patch.object(self.app, "initialize"), patch.object(self.app, "prepare_cleanup"), patch.object(self.app, "terraform", side_effect=terraform), patch("builtins.print"):
            self.app.plan("unprotect")
            plan["resource_changes"][0]["change"]["after"]["name"] = "another-installation"
            with self.assertRaisesRegex(ValueError, "only disable"):
                self.app.plan("unprotect")


if __name__ == "__main__":
    unittest.main()
