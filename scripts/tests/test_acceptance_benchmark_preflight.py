"""Offline safety regression for full benchmark preflight; no cloud calls."""

import hashlib
import json
from pathlib import Path
import socket
from types import SimpleNamespace
import tempfile
import unittest
from unittest import mock

from deploy.acceptance import host, client


class BenchmarkPreflightTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="miles-offline-preflight-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.dataset = self.base / "dataset"
        self.dataset.mkdir()
        self.payload = b"sample-native-archive-proof"
        self.sha = hashlib.sha256(self.payload).hexdigest()
        (self.dataset / "sample.bin").write_bytes(self.payload)
        self.manifest = self.dataset / "manifest.json"
        self.manifest.write_text(json.dumps({
            "total_bytes": len(self.payload),
            "files": [{"path": "sample.bin", "bytes": len(self.payload), "sha256": self.sha}],
        }))
        self.tenant = {"bucket": "sample-tenant",
                       "credentials": {"access_key_id": "fixture", "secret_access_key": "secret"}}
        self.folder = self.base / "out"
        self.folder.mkdir()

    def test_cloud_alias_resolves_only_private_internal_alb(self):
        resolver = [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("10.30.4.11", 443))]
        with mock.patch.object(host.socket, "getaddrinfo", return_value=resolver):
            self.assertEqual(
                host.cloud_alias_addresses(
                    "internal-miles-dev-123456.ap-southeast-1.elb.amazonaws.com"),
                ["10.30.4.11"])
        for address in ("203.0.113.4", "127.0.0.1", "169.254.169.254"):
            with self.subTest(address=address), mock.patch.object(
                host.socket, "getaddrinfo",
                return_value=[(socket.AF_INET, socket.SOCK_STREAM, 6, "", (address, 443))]
            ), self.assertRaises(ValueError):
                host.cloud_alias_addresses(
                    "internal-miles-dev-123456.ap-southeast-1.elb.amazonaws.com")
        for name in ("evil.example.com", "alb.amazonaws.com", "external-a.region.elb.amazonaws.com"):
            with self.subTest(name=name), self.assertRaises(ValueError):
                host.cloud_alias_addresses(name)

    def test_cold_requires_native_physical_inventory_before_mutation(self):
        with mock.patch.object(client.subprocess, "run") as runner, self.assertRaisesRegex(
            ValueError, "physical-inventory provider not reviewed"
        ):
            client.benchmark({"runner": "coldbench"})
        runner.assert_not_called()

    def test_recovery_requires_approved_fault_and_native_verifier_before_mutation(self):
        with mock.patch.object(client.subprocess, "run") as runner, self.assertRaisesRegex(
            ValueError, "not staged and reviewed"
        ):
            client.benchmark({"runner": "recoverybench"})
        runner.assert_not_called()

    def test_seed_sha_preflight_rejects_tampering_and_escape(self):
        self.assertEqual(len(client.verify_seed_manifest(self.dataset, self.manifest)), 1)
        (self.dataset / "sample.bin").write_bytes(b"modified")
        with self.assertRaisesRegex(ValueError, "SHA/size mismatch"):
            client.verify_seed_manifest(self.dataset, self.manifest)
        payload = json.loads(self.manifest.read_text())
        payload["files"][0]["path"] = "../outside.bin"
        self.manifest.write_text(json.dumps(payload))
        with self.assertRaises(ValueError):
            client.verify_seed_manifest(self.dataset, self.manifest)

    def test_cold_native_class_and_recovery_standard_are_verified(self):
        for runner, variant, expected in (
            ("coldbench", "ir", "GLACIER_IR"),
            ("coldbench", "flexible", "GLACIER"),
            ("recoverybench", "r1", "STANDARD"),
        ):
            with self.subTest(runner=runner, variant=variant):
                def fake_run(argv, **kwargs):
                    if "put-object" in argv:
                        self.assertEqual(argv[argv.index("--storage-class")+1], expected)
                        return SimpleNamespace(returncode=0, stdout='{"VersionId":"v1"}')
                    self.assertIn("head-object", argv)
                    return SimpleNamespace(returncode=0, stdout=json.dumps({
                        "StorageClass": expected,
                        "ContentLength": len(self.payload), "VersionId": "v1"
                    }))
                with mock.patch.object(client.subprocess, "run", side_effect=fake_run) as run:
                    receipts = client.seed_benchmark_dataset(
                        {"runner": runner, "variant": variant, "region": "ap-southeast-1"},
                        self.folder, {}, "https://edge.miles.test", self.tenant, self.manifest)
                self.assertEqual(run.call_count, 2)
                self.assertEqual(receipts[0]["class"], expected)
                self.assertEqual(receipts[0]["sha256"], self.sha)

    def test_backend_class_mismatch_and_unknown_put_fail_closed(self):
        settings = {"runner": "coldbench", "variant": "flexible", "region": "ap-southeast-1"}
        def fake_run(argv, **kwargs):
            if "put-object" in argv:
                return SimpleNamespace(returncode=0, stdout='{"VersionId":"v1"}')
            return SimpleNamespace(returncode=0, stdout=json.dumps({
                "StorageClass": "STANDARD", "ContentLength": len(self.payload)
            }))
        with mock.patch.object(client.subprocess, "run", side_effect=fake_run), self.assertRaisesRegex(ValueError, "class/length"):
            client.seed_benchmark_dataset(settings, self.folder, {}, "https://edge.miles.test", self.tenant, self.manifest)
        with mock.patch.object(client.subprocess, "run",
                               return_value=SimpleNamespace(returncode=254, stdout="")), self.assertRaisesRegex(ValueError, "do not replay"):
            client.seed_benchmark_dataset(settings, self.folder, {}, "https://edge.miles.test", self.tenant, self.manifest)


if __name__ == "__main__":
    unittest.main()
