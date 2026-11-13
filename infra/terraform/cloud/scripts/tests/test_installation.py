"""Installation inputs must scope every generated artifact to the same identity."""
import copy
from datetime import datetime, timedelta, timezone
from fnmatch import fnmatchcase
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest


CLOUD = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("installation", CLOUD / "scripts/render-installation.py")
installation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installation)


def fixture():
    return json.loads((CLOUD / "environments/dev/installation.example.json").read_text())


class InstallationTests(unittest.TestCase):
    def test_explicit_state_deletion_deny_preserves_lock_and_state_writes(self):
        config = fixture()
        rendered = installation.render(config)
        policy = rendered["iam/deploy-state.json"]["Statement"]
        deny = next(s for s in policy if s["Sid"] == "NeverDeleteBackendState")
        bucket = config["installation_name"] + "-tfstate-" + config["account_id"] + "-" + config["region"]
        state = "arn:aws:s3:::" + bucket + "/cloud/" + config["environment"] + "/terraform.tfstate"
        self.assertEqual(deny, {"Sid": "NeverDeleteBackendState", "Effect": "Deny", "Action": ["s3:DeleteObject"], "Resource": [state]})
        self.assertNotIn("Condition", deny)
        unlock = next(s for s in policy if s["Sid"] == "UnlockDevState")
        self.assertEqual(unlock["Resource"], [state + ".tflock"])
        self.assertEqual(unlock["Effect"], "Allow")
        writes = next(s for s in policy if s["Sid"] == "ReadAndWriteDevState")
        self.assertIn("s3:PutObject", writes["Action"])
        self.assertEqual(writes["Resource"], [state, state + ".tflock"])
        shards = [json.loads(document) for document in rendered["operator.tfvars.json"]["deploy_documents"].values()]
        self.assertIn(deny, [s for document in shards for s in document["Statement"]])

    def test_operator_cannot_assume_other_roles_or_reassign_resource_ownership(self):
        config = fixture()
        output = installation.render(config)
        user = output["iam/user-policy.json"]["Statement"]
        deny = next(s for s in user if s["Sid"] == "NeverAssumeOtherRoles")
        self.assertEqual(deny["Effect"], "Deny")
        self.assertEqual(deny["Action"], "sts:AssumeRole")
        self.assertEqual(deny["NotResource"], config["operator"]["deploy_role_arn"])
        guard = output["iam/deploy-isolation.json"]["Statement"]
        for key, value in [("Project", "Miles"), ("Environment", config["environment"]), ("Installation", config["installation_name"])]:
            for kind, sid in [("Resource", "DoNotClaimForeign"), ("Request", "DoNotChangeOwnership")]:
                statement = next(s for s in guard if s["Sid"] == sid + key)
                tag = "aws:" + kind + "Tag/" + key
                self.assertEqual(statement["Effect"], "Deny")
                self.assertIn("rolesanywhere:TagResource", statement["Action"])
                self.assertIn("acm:AddTagsToCertificate", statement["Action"])
                self.assertEqual(statement["Condition"], {"Null": {tag:"false"}, "StringNotEquals": {tag:value}})
        removal = next(s for s in guard if s["Sid"] == "KeepOwnershipLabels")
        self.assertEqual(removal["Condition"]["ForAnyValue:StringEqualsIgnoreCase"]["aws:TagKeys"], ["Project", "Environment", "Installation"])

    def test_profile_cleanup_is_scoped_to_the_installation_profiles(self):
        config = fixture()
        policy = installation.render(config)["iam/deploy-runtime-roles.json"]
        allowed_resources = [resource for statement in policy["Statement"]
                             if statement["Effect"] == "Allow"
                             and "iam:DeleteInstanceProfile" in statement["Action"]
                             for resource in statement["Resource"]]
        prefix = "arn:aws:iam::123456789012:instance-profile/miles/member/"
        self.assertEqual(set(allowed_resources), {prefix + "miles-member-test-gateway",
                                                  prefix + "miles-member-test-jobs",
                                                  prefix + "miles-member-test-edge-bootstrap"})
        for denied in (prefix + "another-installation-gateway",
                       prefix + "miles-member-test-edge",
                       prefix.replace("123456789012", "210987654321") + "miles-member-test-gateway",
                       config["operator"]["deploy_role_arn"]):
            self.assertFalse(any(fnmatchcase(denied, resource) for resource in allowed_resources))

    def test_private_acceptance_identities_are_opt_in_and_bounded(self):
        config = fixture()
        original = installation.render(config)
        self.assertNotIn("-acceptance-", json.dumps(original["iam/deploy-runtime-roles.json"]))
        config["enable_acceptance_nodes"] = True
        output = installation.render(config)
        prefix = "arn:aws:iam::123456789012:"
        roles = [prefix + "role/miles/member/miles-member-test-acceptance-" + node for node in ("edge", "client")]
        profiles = [role.replace(":role/", ":instance-profile/") for role in roles]
        for statement in output["iam/deploy-runtime-roles.json"]["Statement"]:
            if statement["Sid"] in ("CreateBoundedRuntimeRoles", "ManageRuntimeRoles", "PassOnlyRuntimeRoles"):
                self.assertTrue(set(roles).issubset(statement["Resource"]))
            if statement["Sid"] in ("ManageRuntimeProfiles", "DeleteRuntimeProfiles"):
                self.assertTrue(set(profiles).issubset(statement["Resource"]))
            if "iam:CreateRole" in statement["Action"]:
                self.assertEqual(statement["Condition"]["StringEquals"]["iam:PermissionsBoundary"], config["runtime_permissions_boundary_arn"])
        launch = next(s for s in output["iam/deploy-compute.json"]["Statement"] if s["Sid"] == "LaunchTaggedRuntime")
        self.assertTrue(set(profiles).issubset(launch["Condition"]["StringEquals"]["ec2:InstanceProfile"]))
        assets = next(s for s in output["iam/runtime-boundary.json"]["Statement"] if s["Sid"] == "ReadPinnedAcceptanceAssets")
        self.assertEqual(assets["Resource"], ["arn:aws:s3:::miles-member-test-data-123456789012-ap-southeast-1/acceptance-assets/*"])
        self.assertLessEqual(len(output["operator.tfvars.json"]["deploy_documents"]), 9)
        config["operator"]["cleanup_expires_at"] = (datetime.now(timezone.utc) + timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.assertLessEqual(len(installation.render(config)["operator.tfvars.json"]["deploy_documents"]), 9)
        config["operator"]["deploy_role_arn"] = roles[0]
        with self.assertRaisesRegex(ValueError, "separate"):
            installation.render(config)

    def test_two_accounts_regions_and_prefixes_stay_independent(self):
        for account, region, name in [("123456789012", "ap-southeast-1", "miles-member-test"),
                                      ("210987654321", "ap-southeast-2", "miles-hai-test")]:
            config = fixture()
            for key in ("runtime_permissions_boundary_arn",):
                config[key] = config[key].replace("123456789012", account)
            for key in ("user_arn", "mfa_arn", "deploy_role_arn"):
                config["operator"][key] = config["operator"][key].replace("123456789012", account)
            config.update(account_id=account, region=region, installation_name=name,
                          availability_zones=[region + "a", region + "b"])
            result = installation.render(config)
            self.assertEqual(result["bootstrap-state.tfvars.json"]["installation_name"], name)
            self.assertIn(name + "-tfstate-" + account + "-" + region, result["backend.hcl"])
            combined = json.dumps(result)
            for wrong in ("{{", "hung-deploy", "miles-dev", "Owner = Hung"):
                self.assertNotIn(wrong, combined)
            if region != "ap-southeast-1":
                self.assertNotIn("ap-southeast-1", combined)
                self.assertNotIn("123456789012", combined)
            trust = result["iam/trust-policy.json"]["Statement"][0]
            self.assertEqual(trust["Principal"]["AWS"], config["operator"]["user_arn"])
            self.assertEqual(trust["Condition"]["Bool"]["aws:MultiFactorAuthPresent"], "true")
            roles = result["iam/deploy-runtime-roles.json"]["Statement"]
            self.assertEqual(roles[0]["Condition"]["StringEquals"]["iam:PermissionsBoundary"], config["runtime_permissions_boundary_arn"])
            self.assertNotIn("arn:aws:iam::" + account + ":role/miles/member/" + name + "-edge", roles[-1]["Resource"])
            unlock = result["iam/deploy-state.json"]["Statement"][-1]
            self.assertTrue(all(r.endswith(".tflock") for r in unlock["Resource"]))
            for policy_name in ("deploy-network-create", "deploy-network", "deploy-compute", "runtime-boundary"):
                for statement in result["iam/" + policy_name + ".json"]["Statement"]:
                    for condition in statement.get("Condition", {}).values():
                        for prefix in ("aws:RequestTag/", "aws:ResourceTag/"):
                            if prefix + "Environment" in condition:
                                self.assertEqual(condition[prefix + "Installation"], name)
            for file, policy in result.items():
                if file.startswith("iam/"):
                    self.assertLessEqual(len(json.dumps(policy, separators=(",", ":"))), 6144, file)

    def test_legacy_defaults_and_selected_compute_type(self):
        config = fixture()
        for key in ("installation_name", "region", "environment", "runtime_iam_path", "edge_certificate_common_name"):
            config.pop(key)
        config.update(fleet_instance_type="m7g.large")
        result = installation.render(config)
        self.assertIn("miles-dev-tfstate-123456789012-ap-southeast-1", result["backend.hcl"])
        self.assertIn("cloud/dev/terraform.tfstate", result["backend.hcl"])
        policy = result["iam/deploy-compute.json"]
        types = [s.get("Condition", {}).get("StringEquals", {}).get("ec2:InstanceType") for s in policy["Statement"]]
        self.assertIn(["m7g.large", "t4g.small"], types)

    def test_invalid_or_cross_account_authority_is_rejected(self):
        original = fixture()
        cases = [
            lambda c: c.update(installation_name="miles-*"),
            lambda c: c.update(region="us-gov-west-1"),
            lambda c: c.update(runtime_iam_path="/miles/*/"),
            lambda c: c.update(runtime_permissions_boundary_arn="arn:aws:iam::210987654321:policy/Other"),
            lambda c: c["operator"].update(user_arn="arn:aws:iam::210987654321:user/Other"),
            lambda c: c["operator"].update(terraform_profile="bad\nprofile"),
            lambda c: c["operator"].update(state_key="../another-state"),
            lambda c: c["operator"].update(audit_expires_at="2020-01-01T00:00:00Z"),
            lambda c: c.update(installation_nam="typo"),
            lambda c: c["operator"].update(unknown="typo"),
            lambda c: c.update(deploy_role_arn=c["operator"]["deploy_role_arn"]),
        ]
        for mutate in cases:
            config = copy.deepcopy(original)
            mutate(config)
            with self.assertRaises(ValueError):
                installation.render(config)

    def test_new_private_directory_only_and_no_partial_invalid_render(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "rendered"
            config = fixture()
            mask = os.umask(0)
            try:
                installation.write(config, output)
            finally:
                os.umask(mask)
            self.assertEqual(output.stat().st_mode & 0o777, 0o700)
            for file in output.rglob("*"):
                self.assertEqual(file.stat().st_mode & 0o777, 0o700 if file.is_dir() else 0o600)
            with self.assertRaises(ValueError):
                installation.write(config, output)
            link = Path(directory) / "link"
            link.symlink_to(output)
            with self.assertRaises(ValueError):
                installation.write(config, link)
            config["owner"] = ""
            with self.assertRaises(ValueError):
                installation.write(config, Path(directory) / "invalid")
            self.assertFalse((Path(directory) / "invalid").exists())

    def test_output_inside_checkout_is_rejected(self):
        with self.assertRaises(ValueError):
            installation.write(fixture(), CLOUD / "private-installation")
