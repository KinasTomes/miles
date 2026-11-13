"""Fresh Edge writes regional runtime and isolated provider without leaking material."""
import hashlib
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("edge_bootstrap", ROOT / "deploy/edge/bootstrap.py")
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)


class EdgeBootstrapTests(unittest.TestCase):
    def test_installs_pinned_helper_and_separates_bootstrap_identity_from_serving(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            def mapped(value):
                return base / str(value).lstrip("/")
            for name in ("etc/miles", "etc/ssh/sshd_config.d", "etc/nginx", "usr/local/bin", "etc/systemd/system", "home/miles-owner"):
                (base / name).mkdir(parents=True)
            helper = b"fixture helper"
            config = dict(region="ap-southeast-2", account_id="123456789012", bucket="member-data", helper_key="helper-key",
                          helper_version="helper-version", helper_sha256=hashlib.sha256(helper).hexdigest(),
                          bootstrap_secret_arn="edge-secret", trust_anchor_arn="anchor-arn", profile_arn="profile-arn", role_arn="role-arn",
                          table_prefix="member-", kms_key_arn="kms-arn", runtime_secret_arn="runtime-secret",
                          owner_ssh_public_key="ssh-ed25519 fixture-key member")
            calls = []
            def run(arguments, **options):
                calls.append((arguments,options))
                if arguments[:3] == ["aws","s3api","get-object"]:
                    self.assertEqual(arguments[arguments.index("--version-id")+1], "helper-version")
                    Path(arguments[-3]).write_bytes(helper)  # --region follows the output path.
                    return SimpleNamespace(returncode=0, stdout="{}")
                if arguments[:3] == ["aws","secretsmanager","get-secret-value"]:
                    return SimpleNamespace(returncode=0, stdout=json.dumps({"SecretString":json.dumps({"tls_certificate":"test-tls", "tls_private_key":"test-tls-secret", "provider_certificate":"test-provider", "provider_private_key":"test-provider-secret"})}))
                return SimpleNamespace(returncode=0,stdout="")
            with patch.object(bootstrap, "Path", side_effect=mapped), patch.object(bootstrap.subprocess, "run", side_effect=run):
                bootstrap.bootstrap(ROOT, config)
                bootstrap.bootstrap(ROOT, config)  # Retrying setup must retain valid paths/users.
            startup = json.loads((base/"etc/miles/startup.json").read_text())
            self.assertEqual(startup["region"], "ap-southeast-2")
            self.assertEqual(startup["ddb_table_prefix"], "member-")
            self.assertEqual(startup["s3_address"], "127.0.0.1:8080")
            self.assertIn("--trust-anchor-arn anchor-arn", (base/"etc/miles/aws-config").read_text())
            self.assertIn("IPAddressDeny=169.254.169.254", (base/"etc/systemd/system/miles.service.d/identity.conf").read_text())
            self.assertIn('permitopen="127.0.0.1:9000"', (base/"home/miles-owner/.ssh/authorized_keys").read_text())
            self.assertEqual((base/"etc/miles/tls/private-key.pem").stat().st_mode & 0o777, 0o600)
            self.assertEqual((base/"etc/miles/rolesanywhere/private-key.pem").stat().st_mode & 0o777, 0o640)
            for arguments, options in calls:
                self.assertNotIn("test-provider-secret", str(arguments))
                self.assertNotIn("test-tls-secret", str(arguments))
                if arguments[0] == "aws":
                    self.assertEqual(options["env"]["AWS_CONFIG_FILE"], "/dev/null")
                    self.assertIn("--region", arguments)


if __name__ == "__main__":
    unittest.main()
