"""Offline AWS fixtures; no IAM/backend writes or live AWS evidence."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from test_installation import CLOUD, fixture, installation as renderer
from test_deployment_workflow import workflow

spec = importlib.util.spec_from_file_location("existing", CLOUD / "scripts/existing-bootstrap.py")
existing = importlib.util.module_from_spec(spec)
spec.loader.exec_module(existing)


class ExistingBootstrapTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.config = dict(fixture(), bootstrap_mode="existing")
        self.expected = renderer.render(self.config)["bootstrap-inventory.json"]
        self.file = self.root / "inputs.json"
        self.file.write_text(json.dumps(self.config))
        self.app = workflow.Installation(self.file, self.root / "private")
        self.app.state["prepared"] = True
        renderer.write(self.config, self.app.private / "rendered")
        tags = [{"Key": k, "Value": v} for k, v in self.expected["tags"].items()]
        self.responses = {
            "get-caller-identity": {"Account": self.expected["account_id"], "UserId": "ROLEID:session",
                "Arn": "arn:aws:sts::123456789012:assumed-role/miles-member-deploy/session"},
            "get-role": {"Role": {"Arn": self.expected["role_arn"], "RoleId": "ROLEID", "Tags": tags,
                "AssumeRolePolicyDocument": {"Statement": [{"Effect": "Allow", "Principal": {"AWS": self.expected["user_arn"]},
                    "Action": "sts:AssumeRole", "Condition": {"Bool": {"aws:MultiFactorAuthPresent": "true"}}}]}}},
            "get-policy": {"Policy": {"Arn": self.expected["boundary_arn"], "DefaultVersionId": "v1"}},
            "list-policy-tags": {"Tags": tags},
            "get-policy-version": {"PolicyVersion": {"Document": {"Statement": [{"Effect": "Allow", "Action": "s3:GetObject", "Resource": "*"}]}}},
            "get-bucket-location": {"LocationConstraint": self.expected["region"]},
            "get-bucket-versioning": {"Status": "Enabled"},
            "get-public-access-block": {"PublicAccessBlockConfiguration": dict.fromkeys(("BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"), True)},
            "get-bucket-encryption": {"ServerSideEncryptionConfiguration": {"Rules": [{"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}}]}},
            "get-bucket-tagging": {"TagSet": tags},
            "get-bucket-policy": {"Policy": json.dumps({"Statement": [{"Effect": "Deny", "Principal": "*", "Action": "s3:*",
                "Resource": ["arn:aws:s3:::" + self.expected["state_bucket"], "arn:aws:s3:::" + self.expected["state_bucket"] + "/*"],
                "Condition": {"Bool": {"aws:SecureTransport": "false"}}}]})},
        }
        self.remote = None
        self.permission_overrides = {}
        self.calls = []

    def aws(self, *arguments, admin=False):
        self.assertFalse(admin)
        self.calls.append(arguments)
        service, operation = arguments[:2]
        if service == "s3api":
            self.assertEqual(arguments[arguments.index("--expected-bucket-owner") + 1], self.expected["account_id"])
            self.assertEqual(arguments[arguments.index("--bucket") + 1], self.expected["state_bucket"])
        if operation == "get-object":
            if self.remote is None:
                raise workflow.AWSFailure("NoSuchKey")
            Path(arguments[-1]).write_text(json.dumps(self.remote))
            return {}
        if operation == "simulate-principal-policy":
            request = json.loads(Path(arguments[-1].removeprefix("file://")).read_text())
            self.assertIsInstance(request["ContextEntries"], list)
            action, resource = request["ActionNames"][0], request["ResourceArns"][0]
            decision = "allowed"
            if action == "iam:PutRolePolicy":
                decision = "implicitDeny"
            elif action == "s3:DeleteObject" and not resource.endswith(".tflock"):
                decision = "explicitDeny"
            return self.permission_overrides.get((action, resource), {"EvaluationResults": [{"EvalDecision": decision}]})
        return copy.deepcopy(self.responses[operation])

    def verify(self):
        return existing.verify(self.aws, self.expected, self.app.state, self.app.private)

    def test_existing_render_has_only_external_inventory_backend_and_read_policy(self):
        self.config["operator"].update(state_bucket="member-private-backend", state_key="member/cloud.tfstate")
        rendered = renderer.render(self.config)
        self.assertEqual(set(rendered), {"installation.tfvars.json", "bootstrap-inventory.json", "backend.hcl", "iam/deploy-bootstrap-read.json"})
        self.assertNotIn("bootstrap_mode", rendered["installation.tfvars.json"])
        self.assertIn('bucket = "member-private-backend"', rendered["backend.hcl"])
        self.assertIn('key = "member/cloud.tfstate"', rendered["backend.hcl"])
        for statement in rendered["iam/deploy-bootstrap-read.json"]["Statement"]:
            self.assertTrue(all(a.startswith(("iam:Get", "iam:List", "iam:Simulate")) for a in statement["Action"]))
        del self.config["bootstrap_mode"]
        with self.assertRaisesRegex(ValueError, "override"):
            renderer.render(self.config)

    def test_invalid_mode_ownership_and_backend_collision_fail_offline(self):
        cases = [lambda c: c.update(bootstrap_mode="adopt"),
                 lambda c: c.pop("runtime_iam_path"), lambda c: c.pop("environment"),
                 lambda c: c["operator"].update(state_bucket="bad..bucket"),
                 lambda c: c["operator"].update(state_bucket=c["installation_name"] + "-data-" + c["account_id"] + "-" + c["region"])]
        for mutate in cases:
            with self.subTest(mutate=mutate):
                config = copy.deepcopy(self.config)
                mutate(config)
                with self.assertRaises(ValueError):
                    renderer.render(config)
        config = fixture()
        config.pop("bootstrap_mode")
        self.assertIn("operator.tfvars.json", renderer.render(config))

    def test_read_only_verification_and_fingerprint_are_repeatable(self):
        proof = self.verify()
        self.assertEqual(proof, self.verify())
        self.assertFalse(proof["bootstrap_resources_managed"])
        self.assertFalse((self.app.private / "bootstrap-backend-inspection.json").exists())
        self.assertTrue(all(op.startswith(("get-", "list-", "simulate-")) for _, op, *_ in self.calls))

    def test_bucket_delete_allow_requires_explicit_identity_deny_on_state(self):
        state = "arn:aws:s3:::" + self.expected["state_bucket"] + "/" + self.expected["state_key"]
        policy = json.loads(self.responses["get-bucket-policy"]["Policy"])
        policy["Statement"].append({"Effect": "Allow", "Principal": {"AWS": self.responses["get-caller-identity"]["Arn"]},
                                    "Action": "s3:DeleteObject", "Resource": state})
        self.responses["get-bucket-policy"]["Policy"] = json.dumps(policy)
        self.permission_overrides[("s3:DeleteObject", state)] = {"EvaluationResults": [{"EvalDecision": "implicitDeny"}]}
        with self.assertRaisesRegex(ValueError, "permissions"):
            self.verify()
        self.permission_overrides[("s3:DeleteObject", state)] = {"EvaluationResults": [{"EvalDecision": "explicitDeny"}]}
        self.verify()

    def test_singleton_trust_bucket_and_boundary_statements(self):
        trust = self.responses["get-role"]["Role"]["AssumeRolePolicyDocument"]
        trust["Statement"] = trust["Statement"][0]
        policy = json.loads(self.responses["get-bucket-policy"]["Policy"])
        policy["Statement"] = policy["Statement"][0]
        self.responses["get-bucket-policy"]["Policy"] = json.dumps(policy)
        boundary = self.responses["get-policy-version"]["PolicyVersion"]["Document"]
        boundary["Statement"] = boundary["Statement"][0]
        self.verify()

    def test_malformed_policy_statements_fail_with_validation_errors(self):
        for document in (self.responses["get-role"]["Role"]["AssumeRolePolicyDocument"],
                         self.responses["get-policy-version"]["PolicyVersion"]["Document"]):
            original = document["Statement"]
            for invalid in (None, "Allow", ["Allow"], [None]):
                with self.subTest(invalid=invalid):
                    document["Statement"] = invalid
                    with self.assertRaises(ValueError):
                        self.verify()
            document["Statement"] = original
        for invalid in (None, "Deny", ["Deny"], [None]):
            with self.subTest(bucket=invalid):
                self.responses["get-bucket-policy"]["Policy"] = json.dumps({"Statement": invalid})
                with self.assertRaises(ValueError):
                    self.verify()

    def test_wrong_identity_trust_tags_boundary_and_bucket_fail_closed(self):
        mutations = [
            lambda r: r["get-caller-identity"].update(Account="210987654321"),
            lambda r: r["get-caller-identity"].update(UserId="OTHER:session"),
            lambda r: r["get-role"]["Role"].update(Arn="arn:aws:iam::123456789012:role/other"),
            lambda r: r["get-role"]["Role"].update(Tags=[]),
            lambda r: r["get-role"]["Role"]["AssumeRolePolicyDocument"]["Statement"][0].update(Principal="*"),
            lambda r: r["get-role"]["Role"]["AssumeRolePolicyDocument"]["Statement"][0].update(Condition={}),
            lambda r: r["get-policy"]["Policy"].update(Arn="other"),
            lambda r: r["get-policy-version"]["PolicyVersion"].update(Document={}),
            lambda r: r["list-policy-tags"].update(IsTruncated=True),
            lambda r: r["get-bucket-location"].update(LocationConstraint="eu-west-1"),
            lambda r: r["get-bucket-versioning"].update(Status="Suspended"),
            lambda r: r["get-public-access-block"]["PublicAccessBlockConfiguration"].update(BlockPublicAcls=False),
            lambda r: r["get-bucket-encryption"].update(ServerSideEncryptionConfiguration={}),
            lambda r: r["get-bucket-tagging"].update(TagSet=[]),
            lambda r: r["get-bucket-policy"].update(Policy='{"Statement":[]}'),
        ]
        original = copy.deepcopy(self.responses)
        for i, mutate in enumerate(mutations):
            with self.subTest(case=i):
                self.responses = copy.deepcopy(original)
                mutate(self.responses)
                with self.assertRaises(ValueError):
                    self.verify()

    def test_missing_lock_access_state_deletion_and_incomplete_simulation_fail(self):
        state = "arn:aws:s3:::" + self.expected["state_bucket"] + "/" + self.expected["state_key"]
        for action, resource, result in [
            ("s3:PutObject", state + ".tflock", {"EvaluationResults": [{"EvalDecision": "implicitDeny"}]}),
            ("s3:DeleteObject", state, {"EvaluationResults": [{"EvalDecision": "allowed"}]}),
            ("iam:PutRolePolicy", self.expected["role_arn"], {"EvaluationResults": [{"EvalDecision": "allowed"}]}),
            ("s3:GetObject", state, {"EvaluationResults": []}),
            ("s3:GetObject", state, {"IsTruncated": True, "EvaluationResults": [{"EvalDecision": "allowed"}]}),
            ("s3:DeleteObject", state, {"EvaluationResults": [{"EvalDecision": "implicitDeny", "MissingContextValues": ["aws:SourceIp"]}]}),
        ]:
            with self.subTest(action=action, resource=resource):
                self.permission_overrides = {(action, resource): result}
                with self.assertRaises(ValueError):
                    self.verify()

    def test_remote_state_cannot_adopt_another_workload_or_change_lineage(self):
        self.remote = {"version": 4, "lineage": "fixture-lineage", "resources": [{"mode": "managed"}]}
        with self.assertRaisesRegex(ValueError, "already contains"):
            self.verify()
        self.remote["resources"] = []
        self.verify()
        self.assertEqual(self.app.state["backend_lineage"], "fixture-lineage")
        self.app.state["foundation_apply_started"] = True
        self.remote["resources"] = [{"mode": "managed"}]
        self.verify()  # Resume the same run after an uncertain apply outcome.
        self.remote["lineage"] = "replacement"
        with self.assertRaisesRegex(ValueError, "lineage changed"):
            self.verify()
        self.remote = None
        with self.assertRaisesRegex(ValueError, "disappeared"):
            self.verify()

    def test_access_denied_does_not_retain_a_successful_verification(self):
        with patch.object(self.app, "aws", side_effect=self.aws):
            self.app.verify_bootstrap()
        with patch.object(self.app, "aws", side_effect=workflow.AWSFailure("AccessDenied")):
            with self.assertRaises(workflow.AWSFailure):
                self.app.verify_bootstrap()
        self.assertNotIn("existing_bootstrap", self.app.state)
        self.assertFalse((self.app.private / "bootstrap-verification.json").exists())

    def test_external_bootstrap_stages_are_blocked_before_any_dispatch(self):
        with patch.object(self.app, "aws") as aws, patch.object(self.app, "terraform") as tf:
            for stage in ("operator", "state"):
                with self.assertRaisesRegex(ValueError, "externally managed"):
                    self.app.plan(stage)
                with self.assertRaisesRegex(ValueError, "externally managed"):
                    self.app.apply(stage, "unused")
            aws.assert_not_called()
            tf.assert_not_called()

    def test_existing_foundation_uses_common_plan_apply_and_rechecks_bootstrap(self):
        plan = {"format_version": "1.2", "complete": True, "errored": False, "resource_changes": []}
        def terraform(stack, *arguments, **options):
            if arguments[0] == "plan":
                (self.app.private / "foundation.tfplan").write_bytes(b"reviewed foundation")
            return json.dumps(plan)
        with patch.object(self.app, "aws", side_effect=self.aws), patch.object(self.app, "initialize"), patch.object(self.app, "terraform", side_effect=terraform) as tf, patch("builtins.print"):
            self.app.plan("foundation")
            descriptor = workflow.read(self.app.private / "foundation-plan.json")
            self.assertIn("bootstrap_fingerprint", descriptor)
            self.responses["get-policy"]["Policy"]["DefaultVersionId"] = "v2"
            with self.assertRaisesRegex(ValueError, "changed after plan"):
                self.app.apply("foundation", descriptor["sha256"])
            self.assertFalse(any(c.args[1] == "apply" for c in tf.call_args_list))
            self.responses["get-policy"]["Policy"]["DefaultVersionId"] = "v1"
            self.app.apply("foundation", descriptor["sha256"])
            self.assertTrue(self.app.state["foundation_apply_started"])
            self.assertTrue(self.app.state["foundation_applied"])
            self.assertNotIn("operator_applied", self.app.state)
            self.assertNotIn("state_applied", self.app.state)


if __name__ == "__main__":
    unittest.main()
