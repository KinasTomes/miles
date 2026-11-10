import hashlib
import json
import unittest

from scripts.prepare_teardown_manifest import build_manifest


class PrepareManifestTests(unittest.TestCase):
    def setUp(self):
        self.inventory = {"account_id": "123456789012", "vpc_id": "vpc-1", "dynamodb_tables": ["table"], "s3_buckets": ["bucket"], "iam_roles": ["role"], "cloudwatch_alarms": ["alarm"]}
        kinds = [("aws_vpc", "vpc-1"), ("aws_dynamodb_table", "table"), ("aws_s3_bucket", "bucket"), ("aws_iam_role", "role"), ("aws_cloudwatch_metric_alarm", "alarm")]
        resources = [{"mode": "managed", "address": f"{kind}.example", "type": kind, "values": {"id": identity}} for kind, identity in kinds]
        self.state = json.dumps({"values": {"root_module": {"resources": resources}}}).encode()
        self.digest = hashlib.sha256(self.state).hexdigest()

    def prepare(self, inventory=None, digest=None):
        return build_manifest(self.state, digest or self.digest, inventory or self.inventory, "run", "123456789012", "ap-southeast-1")

    def test_manifest_binds_state_and_exact_inventory(self):
        manifest = self.prepare()
        self.assertEqual(manifest["provenance"]["state_sha256"], self.digest)
        self.assertEqual(manifest["expected_resources"]["dynamodb_tables"], ["table"])

    def test_rejects_modified_state_and_missing_resource(self):
        with self.assertRaises(ValueError):
            self.prepare(digest="0" * 64)
        incomplete = dict(self.inventory, dynamodb_tables=[])
        self.assertEqual(self.prepare(inventory=incomplete)["expected_resources"]["dynamodb_tables"], ["table"])
        with self.assertRaises(ValueError):
            self.prepare(inventory=dict(self.inventory, expected_missing=["incomplete"]))


if __name__ == "__main__":
    unittest.main()
