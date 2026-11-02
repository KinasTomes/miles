import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch, Mock
import signal
import sys

path = Path(__file__).resolve().parents[1] / "admin_tunnel.py"
spec = importlib.util.spec_from_file_location("admin_tunnel", path)
tunnel = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tunnel)


class TunnelTest(unittest.TestCase):
    def test_ssh_binds_only_loopback_and_requires_host_keys(self):
        cmd = tunnel.command(Path("/private/ssh-config"), "cloud-b", 9000)
        self.assertIn("127.0.0.1:9000:127.0.0.1:9000", cmd)
        self.assertIn("StrictHostKeyChecking=yes", cmd)
        self.assertEqual(cmd[-2:], ["--", "cloud-b"])
        self.assertFalse(any("Bearer" in item for item in cmd))

    def test_token_permissions_and_symlinks(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / "token"
            token = "a" * 32 + "." + "A" * 43
            p.write_text(token + "\n")
            p.chmod(0o600)
            self.assertEqual(tunnel.token_file(p), token)
            p.chmod(0o644)
            with self.assertRaises(ValueError):
                tunnel.token_file(p)
            link = Path(tmp) / "link"
            link.symlink_to(p)
            with self.assertRaises(OSError):
                tunnel.token_file(link)

    def test_unhealthy_node_switches_to_next_pinned_alias(self):
        callbacks = {}
        started = []
        process = Mock()
        process.poll.return_value = None
        probes = 0

        def launch(cmd, **_):
            started.append(cmd[-1])
            return process

        def probe(*_):
            nonlocal probes
            probes += 1
            if probes == 3:
                callbacks[signal.SIGTERM]()
            return probes != 2

        args = ["admin_tunnel.py", "--ssh-config", "/private/config", "--target", "cloud-a",
                "--target", "cloud-b", "--token-file", "/private/token", "--user-id", "owner"]
        with patch.object(sys, "argv", args), patch.object(tunnel, "token_file", return_value="hidden"), \
                patch.object(tunnel.signal, "signal", side_effect=lambda sig, cb: callbacks.update({sig: cb})), \
                patch.object(tunnel.subprocess, "Popen", side_effect=launch), \
                patch.object(tunnel, "healthy", side_effect=probe), patch.object(tunnel.time, "sleep"), \
                patch.object(tunnel, "stop") as stop, patch("builtins.print"):
            tunnel.main()
        self.assertEqual(started, ["cloud-a", "cloud-b"])
        self.assertEqual(stop.call_count, 2)
