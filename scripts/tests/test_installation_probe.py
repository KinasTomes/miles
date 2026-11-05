"""Installation smoke keeps credentials private and checks real data assertions."""
from contextlib import contextmanager
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("installation_probe", ROOT / "scripts/installation_probe.py")
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
spec = importlib.util.spec_from_file_location("node_drain", ROOT / "deploy/drain.py")
drain = importlib.util.module_from_spec(spec)
spec.loader.exec_module(drain)


class ProbeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.app = SimpleNamespace(private=self.root, name="member", region="ap-southeast-2", state={"source_commit":"a"*40}, persist=Mock())
        self.inventory = {"alb_dns":"fixture-alb", "edge":{"public_ip":"192.0.2.1"}}
        self.nodes = [("i-0123456789abcdef0", "cloud"), ("i-0123456789abcdef1", "edge")]

    def test_signed_curl_keeps_secret_in_stdin_and_verifies_tls_host_and_region(self):
        credentials = {"access_key_id":"test-access-key", "secret_access_key":"test-secret"}
        with patch.object(probe.subprocess, "run", return_value=subprocess.CompletedProcess([],0,"200","")) as run:
            self.assertEqual(probe.signed(self.app, credentials, "cloud.member.test", "fixture-alb", "GET", "/bucket/key", self.root/"response"), 200)
        self.assertEqual(run.call_args.args[0], ["curl","--config","-"])
        config = run.call_args.kwargs["input"]
        self.assertIn('cacert = ', config)
        self.assertIn('cloud.member.test:443:fixture-alb:443', config)
        self.assertIn('aws:amz:ap-southeast-2:s3', config)
        self.assertNotIn('insecure', config)
        self.assertNotIn('location', config)

    def request(self, method, route, body=None):
        if method == "GET" and route.startswith("/admin/v1/users/"):
            return 404, {}
        if route == "/admin/v1/users":
            return 201, {}
        if route.startswith("/admin/v1/credentials?"):
            return 200, {"items":[]}
        if route == "/admin/v1/credentials":
            return 201, {"access_key_id":"test-key", "secret_access_key":"test-secret", "user_id":body["user_id"]}
        return 200, {}

    @contextmanager
    def admin(self, *_):
        yield self.request

    def signed(self, installation, credentials, hostname, destination, method, path, output, body=None):
        if method == "GET":
            output.write_bytes((self.root/"test-payload.bin").read_bytes())
        return 200

    def test_smoke_checks_both_directions_and_can_repeat_without_rotating_credentials(self):
        with patch.object(probe, "admin", self.admin), patch.object(probe, "signed", side_effect=self.signed) as signed:
            probe.verify(self.app, self.nodes, self.inventory)
            credential = (self.root/"test-credential.json").read_bytes()
            probe.verify(self.app, self.nodes, self.inventory)
            self.assertEqual((self.root/"test-credential.json").read_bytes(), credential)
            self.assertEqual(signed.call_count, 10)
        evidence = json.loads((self.root/"signed-data-evidence.json").read_text())
        self.assertEqual(evidence["directions"], ["cloud-to-edge", "edge-to-cloud"])
        self.assertTrue(evidence["tls_verified"])
        self.assertEqual((self.root/"test-credential.json").stat().st_mode & 0o777, 0o600)

    def test_wrong_owner_conflict_and_corrupted_payload_fail_smoke(self):
        def conflict(*arguments):
            arguments[-2].write_text('<Error><Code>BucketAlreadyExists</Code></Error>')
            return 409
        with patch.object(probe, "admin", self.admin), patch.object(probe, "signed", side_effect=conflict):
            with self.assertRaisesRegex(RuntimeError, "another owner"):
                probe.verify(self.app, self.nodes, self.inventory)
        def corrupt(installation, credentials, hostname, destination, method, path, output, body=None):
            if method == "GET":
                output.write_bytes(b"corrupt")
            return 200
        with patch.object(probe, "admin", self.admin), patch.object(probe, "signed", side_effect=corrupt):
            with self.assertRaisesRegex(RuntimeError, "hash mismatch"):
                probe.verify(self.app, self.nodes, self.inventory)

    def test_edge_drain_requires_current_boot_and_invocation_shutdown_proof(self):
        mapping = {"/var/lib/miles-bootstrap/drain-proof.json":self.root/"proof.json", "/proc/sys/kernel/random/boot_id":self.root/"boot-id"}
        (self.root/"boot-id").write_text("current-boot")
        def path(value):
            return mapping[value]
        responses = ["a"*32, "", "success", '{"msg":"shutdown complete"}', "", "success"]
        with patch.object(drain, "Path", side_effect=path), patch.object(drain, "command", side_effect=responses), patch("builtins.print"):
            drain.drain(edge=True)
        self.assertEqual(json.loads((self.root/"proof.json").read_text())["boot_id"], "current-boot")
        with patch.object(drain, "Path", side_effect=path), patch.object(drain, "command", return_value=""), patch("builtins.print"):
            (self.root/"boot-id").write_text("new-boot")
            with self.assertRaisesRegex(ValueError, "another boot"):
                drain.drain()
        with patch.object(drain, "Path", side_effect=path), patch.object(drain, "command", side_effect=["a"*32, "", "success", '{"msg":"other"}']):
            with self.assertRaisesRegex(ValueError, "proof missing"):
                drain.drain()


if __name__ == "__main__":
    unittest.main()
