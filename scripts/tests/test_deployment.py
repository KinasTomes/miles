import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import platform
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace


ROOT = Path(__file__).resolve().parents[2]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


release = load("release", "deploy/build-release.py")
installer = load("installer", "deploy/install.py")
lifecycle = load("lifecycle", "deploy/cloud/lifecycle.py")


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.bundle = self.base / "bundle"
        self.bundle.mkdir()
        self.arch = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
        files = {name: (ROOT / name).read_bytes() for name in release.FILES}
        files.update({"bin/miles": b"fixture-miles", "bin/dsgctl": b"fixture-dsgctl"})
        for name, data in files.items():
            target = self.bundle / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        self.manifest = {"schema_version": 1, "os": "linux", "arch": self.arch,
                         "source_commit": "a" * 40,
                         "files": {name: hashlib.sha256(data).hexdigest() for name, data in files.items()}}
        self.write_manifest()

    def write_manifest(self):
        (self.bundle / "manifest.json").write_text(json.dumps(self.manifest))

    def test_profiles_install_identical_binaries_and_preserve_operator_config(self):
        cloud, edge = self.base / "cloud", self.base / "edge"
        installer.install(self.bundle, cloud, "cloud", "10.0.1.5")
        installer.install(self.bundle, edge, "edge", "127.0.0.1")
        for binary in ["miles", "dsgctl"]:
            self.assertEqual((cloud / "opt/miles/current/bin" / binary).read_bytes(),
                             (edge / "opt/miles/current/bin" / binary).read_bytes())
        config = cloud / "etc/miles/startup.json"
        initial = json.loads(config.read_text())
        initial["health_address"] = "10.0.2.5:9100"
        config.write_text(json.dumps(initial))
        installer.install(self.bundle, cloud, "cloud", "10.0.1.5")
        self.assertEqual(json.loads(config.read_text())["health_address"], "10.0.2.5:9100")
        self.assertFalse((edge / "etc/miles/private-key.pem").exists())

    def test_restrictive_umask_keeps_release_readable_without_opening_staging_root(self):
        root = self.base / "private-stage"
        root.mkdir(mode=0o700)
        previous = os.umask(0o077)
        try:
            installer.install(self.bundle, root, "edge", "127.0.0.1")
        finally:
            os.umask(previous)
        self.assertEqual(root.stat().st_mode & 0o777, 0o700)
        for directory in [root / "opt", root / "etc", *root.rglob("*")]:
            if directory.is_dir() and not directory.is_symlink():
                self.assertEqual(directory.stat().st_mode & 0o777, 0o755, str(directory))
        self.assertEqual((root / "opt/miles/current/bin/miles").stat().st_mode & 0o777, 0o755)

    def test_tamper_path_architecture_and_public_bind_rejected(self):
        (self.bundle / "bin/miles").write_bytes(b"corrupt")
        with self.assertRaisesRegex(ValueError, "checksum"):
            installer.install(self.bundle, self.base / "bad", "edge", "127.0.0.1")
        self.assertFalse((self.base / "bad/opt/miles/current").exists())
        (self.bundle / "bin/miles").write_bytes(b"fixture-miles")
        self.manifest["files"]["../escape"] = "a" * 64
        self.write_manifest()
        with self.assertRaisesRegex(ValueError, "unsafe"):
            installer.install(self.bundle, self.base / "bad", "edge", "127.0.0.1")
        del self.manifest["files"]["../escape"]
        self.manifest["arch"] = "amd64" if self.arch == "arm64" else "arm64"
        self.write_manifest()
        with self.assertRaisesRegex(ValueError, "architecture"):
            installer.install(self.bundle, self.base / "bad", "edge", "127.0.0.1")
        self.manifest["arch"] = self.arch
        self.write_manifest()
        for profile, address in [("cloud", "8.8.8.8"), ("edge", "10.0.1.1"), ("edge", "0.0.0.0")]:
            with self.assertRaises(ValueError):
                installer.install(self.bundle, self.base / "bad", profile, address)

    def test_existing_dev_fixture_cannot_be_activated_as_deployment(self):
        root = self.base / "staged"
        config = root / "etc/miles/startup.json"
        config.parent.mkdir(parents=True)
        config.write_text(json.dumps({"profile": "edge", "mode": "dev", "shutdown_seconds": 1800}))
        with self.assertRaisesRegex(ValueError, "runtime"):
            installer.install(self.bundle, root, "edge", "127.0.0.1")
        self.assertFalse((root / "opt/miles/current").exists())

    def test_builder_requires_clean_source_and_protects_existing_output(self):
        out = self.base / "release.tar.gz"
        with patch.object(release.subprocess, "check_output", return_value=" M source.go\n"):
            with self.assertRaisesRegex(ValueError, "clean"):
                release.build(out, self.arch)
        out.write_bytes(b"keep")
        with self.assertRaisesRegex(ValueError, "new file"):
            release.build(out, self.arch)
        self.assertEqual(out.read_bytes(), b"keep")

    def test_archive_is_allowlisted_and_records_actual_file_hashes(self):
        out = self.base / "release.tar.gz"

        def fake_go(argv, **kwargs):
            Path(argv[argv.index("-o") + 1]).write_bytes(b"binary:" + argv[-1].encode())

        with patch.object(release.subprocess, "check_output", side_effect=["", "a" * 40 + "\n", "go version test\n"]), \
                patch.object(release.subprocess, "run", side_effect=fake_go):
            result = release.build(out, self.arch)
        self.assertEqual(result["sha256"], hashlib.sha256(out.read_bytes()).hexdigest())
        with tarfile.open(out, "r:gz") as archive:
            manifest = json.load(archive.extractfile("manifest.json"))
            self.assertEqual(set(archive.getnames()), set(release.FILES) | {"manifest.json", "bin/miles", "bin/dsgctl"})
            for name, expected in manifest["files"].items():
                self.assertEqual(hashlib.sha256(archive.extractfile(name).read()).hexdigest(), expected)
            self.assertTrue(all(m.isfile() and not m.uid and not m.gid and not m.mtime for m in archive.getmembers()))


class DrainTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        state = Path(temp.name)
        self.addCleanup(patch.stopall)
        patch.object(lifecycle, "STATE", state).start()
        patch.object(lifecycle, "event").start()
        self.now, self.stop_after, self.unused_after = 0, 15, 65
        self.failed, self.proof, self.heartbeat_failure = False, True, False
        self.actions = []
        self.identity = {"instanceId": "i-" + "a" * 17, "accountId": "123456789012", "region": "ap-southeast-1"}
        self.controller = lifecycle.Lifecycle({"account_id": "123456789012", "region": "ap-southeast-1",
                                             "asg_name": "miles-dev-gateway", "hook_name": "miles-dev-drain",
                                             "target_group_arn": "fixture"})
        patch.object(lifecycle.time, "monotonic", side_effect=lambda: self.now).start()
        patch.object(lifecycle.time, "sleep", side_effect=self.sleep).start()
        patch.object(self.controller, "metadata", return_value=json.dumps(self.identity)).start()
        patch.object(self.controller, "command", side_effect=self.command).start()
        patch.object(self.controller, "aws", side_effect=self.aws).start()

    def sleep(self, seconds):
        self.now += seconds

    def command(self, argv):
        self.actions.append((argv[0] + " " + argv[1], self.now))
        if argv[:2] == ["systemctl", "show"]:
            running = self.now < self.stop_after
            return "InvocationID=" + "b" * 32 + "\nActiveState=" + ("active" if running else "inactive") + "\nMainPID=" + ("100" if running else "0") + "\nResult=" + ("failed" if self.failed else "success") + "\nExecMainStatus=" + ("1" if self.failed else "0")
        if argv[0] == "journalctl":
            self.assertIn("_SYSTEMD_INVOCATION_ID=" + "b" * 32, argv)
            return json.dumps({"MESSAGE": json.dumps({"msg": "shutdown complete" if self.proof else "incomplete shutdown"})})
        self.assertEqual(argv, ["systemctl", "stop", "--no-block", "miles.service"])
        return ""

    def aws(self, service, operation, *args):
        self.actions.append((operation, self.now))
        if operation == "record-lifecycle-action-heartbeat" and self.heartbeat_failure:
            self.heartbeat_failure = False
            raise RuntimeError("transient")
        if operation == "describe-target-health":
            return {"TargetHealthDescriptions": [{"TargetHealth": {
                "State": "unused" if self.now >= self.unused_after else "draining"}}]}
        return {}

    def test_manual_drain_publishes_only_after_process_and_alb_proof(self):
        request = {"node_id": "cloud-node", "incarnation": "boot-a",
                   "service_incarnation": "history-a", "invocation": "b" * 32}
        self.controller.config["table_prefix"] = "fixture-"
        row = {"incarnation": {"S": "boot-a"}, "service_incarnation": {"S": "history-a"},
               "draining": {"BOOL": True}, "drain_applied": {"BOOL": True},
               "revision": {"N": "4"}, "record_revision": {"S": "a" * 64},
               "drain_state": {"S": "gated"}}
        updates = []
        original = self.aws
        def aws(service, operation, *args):
            if operation == "get-item":
                return {"Item": row}
            if operation == "update-item":
                self.assertGreaterEqual(self.now, self.unused_after)
                self.assertGreaterEqual(self.now, self.stop_after)
                self.assertIn("#token=:old", args[args.index("--condition-expression") + 1])
                updates.append(self.now)
                row["drain_state"] = {"S": "complete"}
                if len(updates) == 1:
                    raise RuntimeError("lost update acknowledgement")
                return {}
            return original(service, operation, *args)
        self.controller.aws.side_effect = aws
        self.controller.drain(manual=True, request=request)
        self.assertEqual(len(updates), 1)  # retry reconciles the committed proof
        names = [name for name, _ in self.actions]
        self.assertNotIn("record-lifecycle-action-heartbeat", names)
        self.assertNotIn("complete-lifecycle-action", names)
        self.assertEqual(names.count("systemctl stop"), 1)

    def test_manual_drain_adopts_asg_hook_during_replacement(self):
        def metadata(path):
            if path == "meta-data/autoscaling/target-lifecycle-state":
                return "Terminated" if self.now >= 30 else "InService"
            return json.dumps(self.identity)
        self.controller.metadata.side_effect = metadata
        self.controller.drain(manual=True)
        self.assertEqual([t for name, t in self.actions if name == "record-lifecycle-action-heartbeat"], [30, 60])
        self.assertEqual([t for name, t in self.actions if name == "complete-lifecycle-action"], [65])

    def test_manual_drain_timeout_never_publishes_node_completion(self):
        request = {"node_id": "cloud-node", "incarnation": "boot-a",
                   "service_incarnation": "history-a", "invocation": "b" * 32}
        self.stop_after = 999
        with patch.object(lifecycle, "TOTAL_SECONDS", 20):
            with self.assertRaises(lifecycle.IncompleteDrain):
                self.controller.drain(manual=True, request=request)
        self.assertNotIn("update-item", [name for name, _ in self.actions])

    def test_request_is_private_bounded_and_bound_to_invocation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "request.json"
            request = {"node_id": "cloud-node", "incarnation": "boot-a",
                       "service_incarnation": "history-a", "invocation": "b" * 32}
            path.write_text(json.dumps(request))
            path.chmod(0o600)
            with patch.object(lifecycle, "REQUEST", path), patch.object(lifecycle.pwd, "getpwnam", return_value=SimpleNamespace(pw_uid=os.getuid())):
                self.assertEqual(self.controller.requested(), request)
                path.chmod(0o644)
                with self.assertRaises(lifecycle.IncompleteDrain):
                    self.controller.requested()
                path.chmod(0o600)
                path.unlink()
                path.symlink_to(Path(directory) / "foreign")
                with self.assertRaises(OSError):
                    self.controller.requested()
        request["invocation"] = "c" * 32
        with self.assertRaisesRegex(lifecycle.IncompleteDrain, "another invocation"):
            self.controller.drain(manual=True, request=request)
        self.assertNotIn("systemctl stop", [name for name, _ in self.actions])

    def test_completion_waits_for_process_proof_and_alb_and_keeps_heartbeat(self):
        self.controller.drain()
        names = [name for name, _ in self.actions]
        self.assertLess(names.index("systemctl stop"), names.index("deregister-targets"))
        completed = [t for name, t in self.actions if name == "complete-lifecycle-action"]
        self.assertEqual(completed, [65])
        heartbeats = [t for name, t in self.actions if name == "record-lifecycle-action-heartbeat"]
        self.assertEqual(heartbeats, [0, 30, 60])

    def test_active_process_blocks_completion_and_restart_cannot_reset_deadline(self):
        self.stop_after, self.unused_after = 999, 0
        with patch.object(lifecycle, "TOTAL_SECONDS", 70):
            with self.assertRaises(lifecycle.IncompleteDrain):
                self.controller.drain()
            before = len(self.actions)
            with self.assertRaisesRegex(lifecycle.IncompleteDrain, "cannot be reset"):
                self.controller.drain()
            self.assertEqual(len(self.actions), before)
        self.assertNotIn("complete-lifecycle-action", [name for name, _ in self.actions])

    def test_successful_exit_without_this_invocation_proof_is_incomplete(self):
        self.proof = False
        with self.assertRaisesRegex(lifecycle.IncompleteDrain, "proof"):
            self.controller.drain()
        self.assertNotIn("complete-lifecycle-action", [name for name, _ in self.actions])

    def test_failed_exit_cannot_be_relabelled_as_clean_drain(self):
        self.failed = True
        with self.assertRaisesRegex(lifecycle.IncompleteDrain, "successful shutdown"):
            self.controller.drain()
        self.assertNotIn("complete-lifecycle-action", [name for name, _ in self.actions])

    def test_transient_heartbeat_failure_retries_without_restarting_the_gateway(self):
        self.heartbeat_failure = True
        self.controller.drain()
        self.assertEqual(len([x for x, _ in self.actions if x == "systemctl stop"]), 1)
        self.assertEqual(len([x for x, _ in self.actions if x == "complete-lifecycle-action"]), 1)

    def test_wrong_identity_does_not_stop_services_or_call_aws(self):
        self.identity["accountId"] = "999999999999"
        self.controller.metadata.return_value = json.dumps(self.identity)
        with self.assertRaisesRegex(lifecycle.IncompleteDrain, "identity"):
            self.controller.drain()
        self.assertEqual(self.actions, [])

    def test_metadata_uses_target_terminated_not_current_wait_state(self):
        self.controller.metadata.side_effect = ["InService", "Terminated"]
        with patch.object(self.controller, "drain") as drain:
            self.controller.run()
        drain.assert_called_once()

    def test_second_controller_logs_lock_contention_without_side_effects(self):
        with (lifecycle.STATE / "owner.lock").open("w") as owner:
            lifecycle.fcntl.flock(owner, lifecycle.fcntl.LOCK_EX | lifecycle.fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                self.controller.run()
        lifecycle.event.assert_called_once_with("controller-lock-held", completion="withheld")
        self.controller.metadata.assert_not_called()
        self.assertEqual(self.actions, [])
        self.assertFalse((lifecycle.STATE / "state.json").exists())


if __name__ == "__main__":
    unittest.main()
