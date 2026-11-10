import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from scripts.freeze_terraform_inventory import compare_inventory, extract_resources


class FreezeInventoryTests(unittest.TestCase):
    def test_nested_managed_resources_and_no_secret_values(self):
        state = {"values": {"root_module": {"resources": [
            {"mode": "data", "address": "data.aws_x.a", "type": "aws_x", "values": {"id": "shared"}},
        ], "child_modules": [{"resources": [
            {"mode": "managed", "address": "module.logs.aws_cloudwatch_log_group.runtime", "type": "aws_cloudwatch_log_group", "values": {"id": "/miles/logs", "secret": "never-copy"}},
        ]}]}}}
        resources = extract_resources(state)
        self.assertEqual(resources, [{"address": "module.logs.aws_cloudwatch_log_group.runtime", "type": "aws_cloudwatch_log_group", "id": "/miles/logs"}])
        self.assertNotIn("never-copy", json.dumps(resources))

    def test_reject_empty_missing_id_and_duplicate_addresses(self):
        resource = {"mode": "managed", "address": "aws_x.one", "type": "aws_x", "values": {"id": "x"}}
        for items in ([], [dict(resource, values={})], [resource, resource]):
            with self.subTest(items=items), self.assertRaises(ValueError):
                extract_resources({"values": {"root_module": {"resources": items}}})

    def test_state_reconciliation_rejects_co_truncated_inventory(self):
        resources = [
            {"address": "module.data.aws_dynamodb_table.metadata", "type": "aws_dynamodb_table", "id": "table"},
            {"address": "module.security.aws_cloudwatch_log_group.runtime", "type": "aws_cloudwatch_log_group", "id": "/miles/logs"},
        ]
        errors = compare_inventory(resources, {"dynamodb_tables": ["table"]})
        self.assertTrue(any("unsupported_state_resource:module.security.aws_cloudwatch_log_group.runtime" in error for error in errors))
        self.assertEqual(compare_inventory(resources[:1], {"dynamodb_tables": ["table"]}), [])
        self.assertTrue(compare_inventory(resources[:1], {"dynamodb_tables": []}))

    def test_cli_creates_private_file_without_overwriting(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "state.json"
            output = Path(directory) / "inventory.json"
            state.write_text(json.dumps({"values": {"root_module": {"resources": [{"mode": "managed", "address": "aws_x.one", "type": "aws_x", "values": {"id": "x"}}]}}}))
            command = [sys.executable, "scripts/freeze_terraform_inventory.py", "--state-json", str(state), "--output", str(output), "--run-id", "run", "--account-id", "123456789012", "--region", "ap-southeast-1"]
            subprocess.run(command, check=True, capture_output=True)
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            original = output.read_bytes()
            self.assertNotEqual(subprocess.run(command, capture_output=True).returncode, 0)
            self.assertEqual(output.read_bytes(), original)


if __name__ == "__main__":
    unittest.main()
