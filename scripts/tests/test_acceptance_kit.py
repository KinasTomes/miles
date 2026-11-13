"""No credentials/network: assert mutation boundaries and run ownership invariants."""

import importlib
import io
import json
from pathlib import Path
import string
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
common = importlib.import_module("deploy.acceptance.common")
prepare = importlib.import_module("deploy.acceptance.prepare")
deployment = importlib.import_module("deploy.acceptance.deployment")
hosts = importlib.import_module("deploy.acceptance.hosts")
host = importlib.import_module("deploy.acceptance.host")
kit = importlib.import_module("deploy.acceptance.run")
iam = importlib.import_module("deploy.acceptance.iam")


class KitTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="miles-kit-test-")
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.path.chmod(0o700)
        self.config = {
            "account_id": "123456789012",
            "deploy_role_arn": "arn:aws:iam::123456789012:role/test/deploy",
            "runtime_boundary_arn": "arn:aws:iam::123456789012:policy/test/Boundary",
            "backend_bucket": "fixture-state-123456789012",
            "ami_id": "ami-0123456789abcdef0",
        }
        common.save(self.path / "config.json", self.config)
        common.save(
            self.path / "run.json",
            {
                "run_id": "focused-fixture",
                "source": "a" * 40,
                "native_config": "/fixture/config",
                "native_credentials": "/fixture/credentials",
            },
        )
        self.r = common.Run(self.path)

    def test_config_refuses_cross_account_and_oversized_budget(self):
        for patch in [
            {"deploy_role_arn": "arn:aws:iam::999999999999:role/elsewhere"},
            {"hours": 13},
            {"max_usd": 51},
            {"unknown": "ignored"},
            {"zones": ["ap-southeast-1a"]},
            {"ami_id": "latest"},
        ]:
            with self.subTest(patch=patch), self.assertRaises(ValueError):
                common.config({**self.config, **patch})

    def test_private_outputs_atomic_and_refuse_symlinks_existing(self):
        p = self.path / "private.json"
        common.save(p, {"token": "fixture"})
        self.assertEqual(p.stat().st_mode & 0o777, 0o600)
        with self.assertRaises(FileExistsError):
            common.save(p, {}, new=True)
        q = self.path / "link"
        q.symlink_to(p)
        with self.assertRaises(ValueError):
            common.private(q, "bad")
        self.assertEqual(common.load(p), {"token": "fixture"})

    def test_root_cannot_mutate_workload_and_native_profile_is_explicit(self):
        with mock.patch.object(self.r, "command") as call:
            with self.assertRaises(ValueError):
                self.r.aws("ec2", "run-instances", iam=True)
            call.assert_not_called()
            call.return_value = b"{}"
            self.r.aws("iam", "list-roles", iam=True)
            argv = call.call_args.args[0]
            env = call.call_args.kwargs["env"]
            self.assertIn("miles-iam-bootstrap", argv)
            self.assertEqual(env["AWS_CONFIG_FILE"], "/fixture/config")
            self.assertNotIn("AWS_SECRET_ACCESS_KEY", env)

    def test_identity_rejects_root_user_and_other_role_for_deploy(self):
        for arn in [
            "arn:aws:iam::123456789012:root",
            "arn:aws:iam::123456789012:user/fixture",
            "arn:aws:sts::123456789012:assumed-role/other/session",
        ]:
            with (
                mock.patch.object(
                    self.r, "aws", return_value={"Account": "123456789012", "Arn": arn}
                ),
                self.assertRaises(ValueError),
            ):
                self.r.identity()
        with mock.patch.object(
            self.r,
            "aws",
            return_value={
                "Account": "123456789012",
                "Arn": "arn:aws:sts::123456789012:assumed-role/deploy/session",
            },
        ):
            self.r.identity()

    def test_cleanup_reserve_blocks_new_work(self):
        import datetime as dt

        self.r.write(
            "paid-window.json",
            {"cleanup_by": (common.now() + dt.timedelta(minutes=80)).isoformat()},
        )
        with self.assertRaises(ValueError):
            self.r.live_window()
        self.r.write(
            "paid-window.json",
            {"cleanup_by": (common.now() + dt.timedelta(hours=4)).isoformat()},
        )
        self.r.live_window(needed=25)

    def test_changed_plan_never_calls_apply(self):
        self.r.write("iam-activation.json", {"status": "active"})
        self.r.write(
            "foundation-plan.json",
            {
                "plan": "foundation.tfplan",
                "sha256": "wrong",
                "config_sha256": "wrong",
                "vars": "foundation.tfvars.json",
                "vars_sha256": "wrong",
            },
        )
        common.private(self.path / "foundation.tfplan", "changed")
        with mock.patch.object(self.r, "tf") as tf, self.assertRaises(ValueError):
            deployment.apply(self.r, "foundation")
        tf.assert_not_called()

    def test_host_archive_rejects_traversal_links_and_wrong_hash(self):
        for name, kind in [("../outside", tarfile.REGTYPE), ("link", tarfile.SYMTYPE)]:
            archive = self.path / "archive.tar"
            with tarfile.open(archive, "w") as t:
                m = tarfile.TarInfo(name)
                m.type = kind
                m.size = 1 if kind == tarfile.REGTYPE else 0
                t.addfile(m, io.BytesIO(b"x") if m.size else None)
            dest = self.path / ("dest-" + kind.decode())
            with self.assertRaises(ValueError):
                host.unpack(archive, dest, common.digest(archive), 100)
        with self.assertRaises(ValueError):
            host.unpack(archive, self.path / "bad-hash", "0" * 64, 100)
        self.assertFalse((self.path.parent / "outside").exists())

    def test_templates_bind_fresh_account_secret_expiry_and_do_not_include_old_identity(
        self,
    ):
        values = {
            "ACCOUNT_ID": "123456789012",
            "DIRECT_BUCKET_NAME": "fixture-direct-bucket",
            "REGION": "ap-southeast-1",
            "AMI_ID": self.config["ami_id"],
            "DEPLOY_ROLE_ARN": self.config["deploy_role_arn"],
            "RUNTIME_BOUNDARY_ARN": self.config["runtime_boundary_arn"],
            "RUNTIME_SECRET_NAME": "miles-dev/runtime-fixture",
            "AUDIT_ROLE_NAME": "miles-audit-fixture",
            "IAM_EXPIRY": "2030-01-01T00:00:00Z",
        }
        files = list((ROOT / "deploy/acceptance/policies").glob("*.tmpl"))
        self.assertEqual(len(files), 8)
        for p in files:
            document = string.Template(p.read_text()).substitute(values)
            json.loads(document)
            self.assertNotIn("${", document)
            import re

            self.assertTrue(
                all(
                    account == "123456789012"
                    for account in re.findall(
                        r"arn:aws:[^:]*:[^:]*:(\d{12}):", document
                    )
                )
            )
        self.assertIn(
            "s3:ListBucketVersions",
            (
                ROOT / "deploy/acceptance/policies/runtime-boundary.json.tmpl"
            ).read_text(),
        )

    def test_restore_preserves_concurrent_default_change(self):
        self.r.write(
            "iam-activation.json",
            {"status": "active", "managed": {"compute": "v2"}, "inline": "fixture"},
        )
        self.r.write(
            "iam/baseline-compute.json",
            {"arn": "arn:fixture", "default": "v1", "document": {}, "versions": []},
        )
        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(
                self.r, "aws", return_value={"Policy": {"DefaultVersionId": "v3"}}
            ) as aws,
        ):
            with self.assertRaises(ValueError):
                iam.restore(self.r)
            self.assertEqual([c.args[1] for c in aws.call_args_list], ["get-policy"])

    def test_cli_requires_confirmation_and_status_is_offline(self):
        with mock.patch.object(kit.iam, "activate") as activate:
            with self.assertRaises(ValueError):
                kit.main(
                    [
                        "--run-dir",
                        str(self.path),
                        "iam-activate",
                        "--confirm-run",
                        "different",
                    ]
                )
            activate.assert_not_called()
        with (
            mock.patch.object(common.Run, "aws", side_effect=AssertionError("network")),
            mock.patch("sys.stdout", new=io.StringIO()),
        ):
            kit.main(["--run-dir", str(self.path), "status"])

    def test_bootstrap_does_not_replay_uncertain_outcome(self):
        self.r.write("hosts/edge-ready.json", {})
        self.r.write("hosts/client-ready.json", {})
        self.r.write("bootstrap-intent.json", {"at": "fixture"})
        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(self.r, "live_window"),
            mock.patch.object(hosts, "provision_material"),
            mock.patch.object(self.r, "ssh") as ssh,
        ):
            with self.assertRaises(ValueError):
                hosts.bootstrap(self.r)
            ssh.assert_not_called()

    def test_plan_checks_private_fleet_and_no_destroy(self):
        self.r.write("empty-state-proof.json", {})
        self.r.write("foundation.tfvars.json", {})
        changes = [
            {
                "mode": "managed",
                "address": "aws_instance.test",
                "type": "aws_instance",
                "change": {
                    "actions": ["create"],
                    "after": {"associate_public_ip_address": True},
                },
            }
        ]

        def tf(*args, **kwargs):
            if args[0] == "show":
                return json.dumps({"resource_changes": changes}).encode()
            return b""

        with (
            mock.patch.object(self.r, "tf", side_effect=tf),
            self.assertRaises(ValueError),
        ):
            deployment.plan(self.r, "foundation")


class WorkflowTests(unittest.TestCase):
    setUp = KitTests.setUp

    def test_configure_discovers_inputs_without_mutations(self):
        def response(argv, **kwargs):
            profile = argv[argv.index("--profile") + 1]
            service, operation = argv[6:8]
            self.assertTrue(operation.startswith(("get-", "describe-", "head-")))
            if operation == "get-caller-identity":
                arn = (
                    "arn:aws:iam::123456789012:root"
                    if profile == "iam"
                    else "arn:aws:sts::123456789012:assumed-role/deploy/session"
                )
                return json.dumps({"Account": "123456789012", "Arn": arn}).encode()
            if service == "iam" and operation == "get-role":
                return json.dumps(
                    {"Role": {"Arn": self.config["deploy_role_arn"]}}
                ).encode()
            if operation == "describe-images":
                return json.dumps(
                    {
                        "Images": [
                            {
                                "ImageId": self.config["ami_id"],
                                "CreationDate": "2026-01-01T00:00:00Z",
                            }
                        ]
                    }
                ).encode()
            return b"{}"

        dest = self.path / "discovered.json"
        with (
            mock.patch("subprocess.check_output", side_effect=response),
            mock.patch("sys.stdout", new=io.StringIO()),
        ):
            prepare.configure(dest, "ap-southeast-1", "deploy", "iam")
        self.assertEqual(
            common.load(dest)["deploy_role_arn"], self.config["deploy_role_arn"]
        )
        self.assertEqual(dest.stat().st_mode & 0o777, 0o600)

    def test_snapshot_verifies_pending_source_bytes_without_claiming_receipt(self):
        import base64, hashlib

        cleanup = importlib.import_module("deploy.acceptance.cleanup")
        self.r.write(
            "frozen/inventory.json",
            {"dynamodb_tables": ["miles-dev-SystemConfig", "miles-dev-Outbox"]},
        )
        payload = b"fixture-source"

        def aws(service, operation, *args, **kwargs):
            self.assertEqual((service, operation), ("dynamodb", "scan"))
            table = args[args.index("--table-name") + 1]
            if table.endswith("SystemConfig"):
                return {
                    "Items": [
                        {
                            "config_id": {"S": "storage/payload/pointer"},
                            "body": {"B": base64.b64encode(payload).decode()},
                        }
                    ]
                }
            return {
                "Items": [
                    {
                        "payload_pointer": {"S": "pointer"},
                        "payload_sha256": {"S": hashlib.sha256(payload).hexdigest()},
                    }
                ]
            }

        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(self.r, "aws", side_effect=aws),
        ):
            cleanup.snapshot(self.r)
        proof = self.r.read("latest-snapshot.json")["payload_proof"]
        self.assertEqual(proof["pending_events"], 1)
        self.assertEqual(proof["missing_payloads"], 0)
        self.assertEqual(proof["sha_mismatch"], 0)
        self.assertFalse(proof["proves_s3_receipts"])

    def test_destroy_plan_cannot_create_or_delete_another_address(self):
        cleanup = importlib.import_module("deploy.acceptance.cleanup")
        self.r.write("unprotected.json", {})
        self.r.write("active.tfvars.json", {})
        self.r.write(
            "frozen/state.json",
            {
                "values": {
                    "root_module": {
                        "resources": [
                            {
                                "mode": "managed",
                                "address": "aws_s3_bucket.owned",
                                "type": "aws_s3_bucket",
                                "values": {"id": "owned"},
                            }
                        ]
                    }
                }
            },
        )
        source = self.path / "source/infra/terraform/cloud"
        for n in range(4):
            p = source / str(n) / "main.tf"
            p.parent.mkdir(parents=True)
            p.write_text("lifecycle { prevent_destroy = true }")
        changes = {
            "resource_changes": [
                {
                    "address": "aws_s3_bucket.someone_else",
                    "mode": "managed",
                    "change": {"actions": ["delete"]},
                }
            ]
        }

        def tf(*args, **kwargs):
            return json.dumps(changes).encode() if args[0] == "show" else b""

        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(self.r, "tf", side_effect=tf),
            self.assertRaises(ValueError),
        ):
            cleanup.plan(self.r)
        self.assertFalse(self.r.file("destroy-reviewed.json").exists())

    def test_trial_admission_waits_for_matching_durable_cut_schedule(self):
        import datetime as dt

        workload = importlib.import_module("deploy.acceptance.workload")
        gate = {
            "history_id": {"S": "installation"},
            "epoch": {"N": "3"},
            "state": {"S": "Open"},
        }
        self.r.write("bootstrap-gate.json", {"Item": gate})
        due = (common.now() + dt.timedelta(minutes=20)).isoformat()
        recovery = {"next_cut_at": {"S": due}, "next_cut_epoch": {"N": "2"}}
        calls = []

        def get(*args, **kwargs):
            calls.append(args[1])
            key = json.loads(args[args.index("--key") + 1])["config_id"]["S"]
            return {"Item": gate if key == "coordination/r1/current" else recovery}

        def wait(label, check, timeout):
            self.assertFalse(check())
            recovery["next_cut_epoch"]["N"] = "3"
            self.assertTrue(check())

        with (
            mock.patch.object(self.r, "aws", side_effect=get),
            mock.patch.object(self.r, "live_window"),
            mock.patch.object(self.r, "wait", side_effect=wait),
        ):
            workload.admission(self.r)
        self.assertEqual(set(calls), {"get-item"})
        self.assertTrue(self.r.file("admission-proof.json").exists())

    def test_partial_destroy_retries_only_remaining_frozen_identities(self):
        cleanup = importlib.import_module("deploy.acceptance.cleanup")

        def state(*names):
            return {
                "values": {
                    "root_module": {
                        "resources": [
                            {
                                "mode": "managed",
                                "address": "aws_s3_bucket." + name,
                                "type": "aws_s3_bucket",
                                "values": {"id": name},
                            }
                            for name in names
                        ]
                    }
                }
            }

        frozen = state("first", "remaining")
        plan = {
            "prior_state": state("remaining"),
            "resource_changes": [
                {
                    "mode": "managed",
                    "address": "aws_s3_bucket.remaining",
                    "type": "aws_s3_bucket",
                    "change": {"actions": ["delete"], "before": {"id": "remaining"}},
                }
            ],
        }
        cleanup.validate_destroy(plan, frozen)
        cleanup.validate_destroy({"prior_state": state()}, frozen)
        plan["prior_state"]["values"]["root_module"]["resources"][0]["values"]["id"] = (
            "replacement"
        )
        with self.assertRaises(ValueError):
            cleanup.validate_destroy(plan, frozen)
        plan["prior_state"] = state("remaining")
        plan["resource_changes"][0]["change"]["before"]["id"] = "replacement"
        with self.assertRaises(ValueError):
            cleanup.validate_destroy(plan, frozen)

    def test_cleanup_resumes_after_purge_without_reaching_deleted_hosts(self):
        cleanup = importlib.import_module("deploy.acceptance.cleanup")
        common.private(self.r.file("destroy.tfplan"), "reviewed")
        self.r.write("frozen/manifest.json", {})
        review = {
            "plan_sha256": common.digest(self.r.file("destroy.tfplan")),
            "manifest_sha256": common.digest(self.r.file("frozen/manifest.json")),
        }
        self.r.write("destroy-reviewed.json", review)
        self.r.write("frozen/inventory.json", {"acm_certificates": ["owned"]})
        self.r.write("iam/cleanup-created.json", {})
        self.r.write("data-purged.json", {"manifest_sha256": review["manifest_sha256"]})
        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(self.r, "ssh") as ssh,
            mock.patch.object(self.r, "tf") as tf,
            mock.patch.object(
                self.r, "aws", return_value={"CertificateSummaryList": []}
            ) as aws,
        ):
            cleanup.execute(self.r)
            ssh.assert_not_called()
            self.assertEqual(tf.call_args.args[0], "apply")
            self.assertEqual(aws.call_args.args, ("acm", "list-certificates"))
            self.assertTrue(self.r.file("destroy-complete.json").exists())
            cleanup.execute(self.r)
            self.assertEqual(tf.call_count, 1)

    def test_native_audit_preserves_parallel_resources_and_actual_trust_policy(self):
        cleanup = importlib.import_module("deploy.acceptance.cleanup")
        import subprocess

        for name in [
            "iam-finished.json",
            "frozen/state.json",
            "frozen/runtime.json",
            "frozen/inventory.json",
            "volume-baseline.json",
            "account-baseline.json",
        ]:
            self.r.write(name, {})
        bucket = {"Buckets": [{"Name": "miles-dev-unexpected-chien-data"}]}
        role = {
            "Role": {
                "RoleName": "miles-dev-deploy",
                "AssumeRolePolicyDocument": {
                    "Version": "2012-10-17",
                    "Statement": [
                        {
                            "Effect": "Allow",
                            "Principal": "*",
                            "Action": "sts:AssumeRole",
                        }
                    ],
                },
            }
        }
        verifier = mock.Mock()

        def export(path):
            for args, expected in [
                (["s3api", "list-buckets"], bucket),
                (["iam", "get-role", "--role-name", "miles-dev-deploy"], role),
            ]:
                with mock.patch(
                    "subprocess.run",
                    return_value=subprocess.CompletedProcess(
                        [], 0, stdout=json.dumps(expected), stderr=""
                    ),
                ):
                    code, stdout, stderr = verifier._runner(args)
                self.assertEqual(code, 0)
                self.assertEqual(json.loads(stdout), expected)
                self.assertEqual(stderr, "")
            self.r.write("final-absence-summary.json", {"accepted": True})

        verifier.export_summary.side_effect = export
        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(
                cleanup, "TeardownAbsenceVerifier", return_value=verifier
            ),
        ):
            cleanup.audit(self.r)

    def test_cleanup_grant_rejects_changed_manifest_before_iam_write(self):
        cleanup = importlib.import_module("deploy.acceptance.cleanup")
        self.r.write("iam/cleanup.json", {})
        self.r.write("frozen/manifest.json", {})
        self.r.write(
            "cleanup-review.json", {"sha256": "modified", "manifest_sha256": "modified"}
        )
        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(self.r, "aws") as aws,
            self.assertRaises(ValueError),
        ):
            cleanup.grant_cleanup(self.r)
        aws.assert_not_called()


class IAMWorkflowTests(unittest.TestCase):
    setUp = KitTests.setUp

    def test_activation_recovers_lost_version_response_and_restores_defaults(self):
        import datetime as dt

        values = {
            "ACCOUNT_ID": "123456789012",
            "DIRECT_BUCKET_NAME": "fixture-direct-bucket",
            "REGION": "ap-southeast-1",
            "AMI_ID": self.config["ami_id"],
            "DEPLOY_ROLE_ARN": self.config["deploy_role_arn"],
            "RUNTIME_BOUNDARY_ARN": self.config["runtime_boundary_arn"],
            "RUNTIME_SECRET_NAME": "miles-dev/runtime-fixture",
            "AUDIT_ROLE_NAME": "miles-audit-fixture",
            "IAM_EXPIRY": (common.now() + dt.timedelta(hours=6)).isoformat(),
        }
        self.r.write(
            "names.json", {"inline": "fixture-inline", "auditor": "miles-audit-fixture"}
        )
        self.r.write("account-baseline.json", {"resources": {"roles": []}})
        self.r.write(
            "cost-review.json",
            {
                "at": common.now().isoformat(),
                "estimate_usd": "1.0",
                "envelope_usd": self.r.c["max_usd"],
                "hours": 4,
            },
        )
        hashes = {}
        for p in (ROOT / "deploy/acceptance/policies").glob("*.tmpl"):
            name = p.name.removesuffix(".json.tmpl")
            path = self.r.write(
                "iam/" + name + ".json",
                json.loads(string.Template(p.read_text()).substitute(values)),
            )
            if name != "auditor-trust":
                hashes[name] = common.digest(path)
        self.r.write(
            "iam-review.json", {"expires_at": values["IAM_EXPIRY"], "hashes": hashes}
        )
        policies = {}
        for key in iam.MANAGED:
            arn = "arn:aws:iam::123456789012:policy/test/" + key
            self.r.write(
                "iam/baseline-" + key + ".json",
                {"arn": arn, "default": "v1", "document": {"baseline": key}},
            )
            policies[arn] = {"key": key, "default": "v1", "versions": ["v1"]}
        inline = {}
        roles = {
            "AWSServiceRoleForElasticLoadBalancing": {
                "RoleName": "AWSServiceRoleForElasticLoadBalancing",
                "RoleId": "created-by-another-actor",
            }
        }
        lost = [False]
        mutations = []

        def aws(service, operation, *args, **kwargs):
            self.assertTrue(kwargs.get("iam"))
            self.assertEqual(service, "iam")

            def value(flag):
                return args[args.index(flag) + 1]

            if operation == "get-policy":
                return {
                    "Policy": {
                        "DefaultVersionId": policies[value("--policy-arn")]["default"]
                    }
                }
            if operation == "get-policy-version":
                p = policies[value("--policy-arn")]
                return {
                    "PolicyVersion": {
                        "Document": {"baseline": p["key"]}
                        if value("--version-id") == "v1"
                        else self.r.read("iam/" + p["key"] + ".json")
                    }
                }
            if operation == "create-policy-version":
                p = policies[value("--policy-arn")]
                p["default"] = "v2"
                p["versions"].append("v2")
                mutations.append(operation)
                if not lost[0]:
                    lost[0] = True
                    raise ValueError("fixture: response lost AFTER AWS applied")
                return {"PolicyVersion": {"VersionId": "v2"}}
            if operation == "list-role-policies":
                return {"PolicyNames": list(inline)}
            if operation == "get-role-policy":
                return {"PolicyDocument": inline[value("--policy-name")]}
            if operation == "put-role-policy":
                if value("--role-name") == "deploy":
                    inline[value("--policy-name")] = self.r.read("iam/deploy.json")
                return {}
            if operation == "list-roles":
                return {"Roles": list(roles.values())}
            if operation == "create-role":
                name = value("--role-name")
                roles[name] = {
                    "RoleName": name,
                    "RoleId": "fixture-role",
                    "AssumeRolePolicyDocument": self.r.read("iam/auditor-trust.json"),
                }
                return {"Role": roles[name]}
            if operation == "create-service-linked-role":
                name = (
                    "AWSServiceRoleForRolesAnywhere"
                    if value("--aws-service-name") == "rolesanywhere.amazonaws.com"
                    else "AWSServiceRoleForElasticLoadBalancing"
                )
                roles[name] = {"RoleName": name, "RoleId": "fixture-" + name}
                return {"Role": roles[name]}
            if operation == "set-default-policy-version":
                policies[value("--policy-arn")]["default"] = value("--version-id")
                return {}
            if operation == "list-policy-versions":
                return {
                    "Versions": [
                        {"VersionId": v}
                        for v in policies[value("--policy-arn")]["versions"]
                    ]
                }
            if operation == "delete-policy-version":
                policies[value("--policy-arn")]["versions"].remove(
                    value("--version-id")
                )
                return {}
            if operation == "delete-role-policy":
                inline.pop(value("--policy-name"), None)
                return {}
            self.fail("Unexpected IAM call: " + operation)

        with (
            mock.patch.object(self.r, "identity"),
            mock.patch.object(self.r, "aws", side_effect=aws),
        ):
            with self.assertRaises(ValueError):
                iam.activate(self.r)
            self.assertTrue(self.r.read("iam-activation.json")["pending"])
            iam.activate(self.r)
            self.assertEqual(len(mutations), len(iam.MANAGED))
            self.assertEqual(self.r.read("iam-activation.json")["status"], "active")
            self.assertFalse(
                self.r.read("iam/AWSServiceRoleForElasticLoadBalancing.json")["created"]
            )
            iam.restore(self.r)
        self.assertTrue(
            all(
                p["default"] == "v1" and p["versions"] == ["v1"]
                for p in policies.values()
            )
        )
        self.assertFalse(inline)
        self.assertEqual(self.r.read("iam-activation.json")["status"], "restored")


class ColdPrepareTests(unittest.TestCase):
    setUp = KitTests.setUp

    def test_generated_secret_name_matches_existing_boundary_scope(self):
        import fnmatch

        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w"):
            pass

        def command(run, argv, **kwargs):
            if argv[0] == "git":
                return archive.getvalue()
            if argv[0] == "python3":
                target = Path(argv[argv.index("--out") + 1])
                target.write_bytes(b"fixture-release")
                return json.dumps(
                    {
                        "sha256": common.digest(target),
                        "source_commit": "a" * 40,
                        "arch": "arm64",
                    }
                ).encode()
            if argv[0] == "go":
                Path(argv[argv.index("-o") + 1]).write_bytes(b"fixture-binary")
                return b""
            self.fail("Unexpected external command")

        def pki(run):
            common.private(run.file("operator-key.pub"), "ssh-ed25519 AAAA fixture")
            common.private(run.file("identity-ca.pem"), "fixture-public-ca")

        def git(argv, **kwargs):
            return "a" * 40 if "rev-parse" in argv else b""

        dest = self.path / "cold"
        with (
            mock.patch.object(common.Run, "command", command),
            mock.patch.object(prepare, "pki", pki),
            mock.patch("subprocess.check_output", side_effect=git),
            mock.patch("shutil.which", return_value="/fixture/aws"),
            mock.patch("sys.stdout", new=io.StringIO()),
        ):
            prepare.init(dest, self.path / "config.json")
        secret = common.load(dest / "foundation.tfvars.json")["runtime_secret_name"]
        arn = (
            "arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:"
            + secret
            + "-abcdef"
        )
        policy = (
            (ROOT / "deploy/acceptance/policies/runtime-boundary.json.tmpl")
            .read_text()
            .replace("${ACCOUNT_ID}", "123456789012")
            .replace("${REGION}", "ap-southeast-1")
        )
        document = json.loads(policy)
        allowed = [
            resource
            for statement in document["Statement"]
            for resource in (
                statement["Resource"]
                if isinstance(statement["Resource"], list)
                else [statement["Resource"]]
            )
            if ":secret:" in resource
        ]
        self.assertTrue(any(fnmatch.fnmatchcase(arn, resource) for resource in allowed))
        self.assertEqual((dest / "identity-ca.pem").stat().st_mode & 0o777, 0o600)


if __name__ == "__main__":
    unittest.main()
