import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch, Mock
import signal
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from socketserver import UnixStreamServer
import threading

path = Path(__file__).resolve().parents[1] / "admin_tunnel.py"
spec = importlib.util.spec_from_file_location("admin_tunnel", path)
tunnel = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tunnel)


class TunnelTest(unittest.TestCase):
    def test_live_process_without_forwarding_never_sends_token(self):
        captured = []
        class ForeignOrigin(BaseHTTPRequestHandler):
            def do_GET(self):
                captured.append(self.headers.get("Authorization"))
                self.send_response(200)
                self.end_headers()
            def log_message(self, *_):
                pass
        server = HTTPServer(("127.0.0.1", 0), ForeignOrigin)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        callbacks = {}
        process = Mock()
        process.poll.return_value = None
        args = ["admin_tunnel.py", "--ssh-config", "/private/config", "--target", "cloud-a",
                "--token-file", "/private/token", "--user-id", "owner", "--local-port", str(server.server_port)]
        try:
            with patch.object(sys, "argv", args), patch.object(tunnel, "token_file", return_value="test-only-owner-token"), \
                    patch.object(tunnel.signal, "signal", side_effect=lambda sig, cb: callbacks.update({sig: cb})), \
                    patch.object(tunnel.subprocess, "Popen", return_value=process), \
                    patch.object(tunnel, "control", return_value=False, create=True), \
                    patch.object(tunnel.time, "sleep", side_effect=lambda _: callbacks[signal.SIGTERM]()), \
                    patch.object(tunnel, "stop"), patch("builtins.print"):
                tunnel.main()
            self.assertEqual(captured, [])
        finally:
            server.shutdown()
            worker.join()
            server.server_close()

    def test_tcp_probe_does_not_forward_bearer_on_redirect(self):
        captured = []
        class ForeignOrigin(BaseHTTPRequestHandler):
            def do_GET(self):
                captured.append(self.headers.get("Authorization"))
                self.send_response(200)
                self.end_headers()
            def log_message(self, *_):
                pass
        destination = HTTPServer(("127.0.0.1", 0), ForeignOrigin)
        class Redirect(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(302)
                self.send_header("Location", f"http://127.0.0.1:{destination.server_port}/capture")
                self.end_headers()
            def log_message(self, *_):
                pass
        source = HTTPServer(("127.0.0.1", 0), Redirect)
        workers = [threading.Thread(target=s.serve_forever, daemon=True) for s in (source, destination)]
        for worker in workers:
            worker.start()
        try:
            self.assertFalse(tunnel.healthy(source.server_port, "owner", "test-only-owner-token"))
            self.assertEqual(captured, [])
        finally:
            for server, worker in zip((source, destination), workers):
                server.shutdown()
                worker.join()
                server.server_close()

    def test_ssh_binds_only_loopback_and_requires_host_keys(self):
        cmd = tunnel.command(Path("/private/ssh-config"), "cloud-b", Path("/private/control"))
        self.assertIn("StrictHostKeyChecking=yes", cmd)
        self.assertIn("ForkAfterAuthentication=no", cmd)
        self.assertIn("ControlPersist=no", cmd)
        self.assertEqual(cmd[-2:], ["--", "cloud-b"])
        self.assertFalse(any("Bearer" in item for item in cmd))
        with patch.object(tunnel.subprocess, "run", return_value=Mock(returncode=0)) as run:
            self.assertTrue(tunnel.control(Path("/private/control"), "cloud-b", "forward",
                                           ("127.0.0.1:9000:127.0.0.1:9000", "/private/probe:127.0.0.1:9000")))
        forwarded = run.call_args.args[0]
        self.assertIn("127.0.0.1:9000:127.0.0.1:9000", forwarded)
        self.assertIn("/private/probe:127.0.0.1:9000", forwarded)
        self.assertFalse(any("Bearer" in item for item in forwarded))

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
            control = Path(cmd[cmd.index("-S") + 1])
            self.assertEqual(control.parent.stat().st_mode & 0o777, 0o700)
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
                patch.object(tunnel, "control", return_value=True), \
                patch.object(tunnel, "healthy", side_effect=probe), patch.object(tunnel.time, "sleep"), \
                patch.object(tunnel, "stop") as stop, patch("builtins.print"):
            tunnel.main()
        self.assertEqual(started, ["cloud-a", "cloud-b"])
        self.assertEqual(stop.call_count, 2)

    def test_owner_probe_uses_self_usage_without_system_admin(self):
        class AdminContract(BaseHTTPRequestHandler):
            def do_GET(self):
                allowed = self.path == "/admin/v1/users/owner/usage" and self.headers.get("Authorization") == "Bearer owner-token"
                self.send_response(200 if allowed else 403)
                self.end_headers()

            def log_message(self, *_):
                pass

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "probe"
            server = UnixStreamServer(str(path), AdminContract)
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            try:
                self.assertTrue(tunnel.healthy(path, "owner", "owner-token"))
                self.assertFalse(tunnel.healthy(path, "other", "owner-token"))
                self.assertFalse(tunnel.healthy(path, "owner", "revoked-token"))
            finally:
                server.shutdown()
                worker.join()
                server.server_close()

    def test_private_probe_refuses_redirect_without_contacting_destination(self):
        captured, source_tokens = [], []
        class ForeignOrigin(BaseHTTPRequestHandler):
            def do_GET(self):
                captured.append(self.headers.get("Authorization"))
                self.send_response(200)
                self.end_headers()
            def log_message(self, *_):
                pass
        destination = HTTPServer(("127.0.0.1", 0), ForeignOrigin)
        class Redirect(BaseHTTPRequestHandler):
            def do_GET(self):
                source_tokens.append(self.headers.get("Authorization"))
                self.send_response(302)
                self.send_header("Location", f"http://127.0.0.1:{destination.server_port}/capture")
                self.end_headers()
            def log_message(self, *_):
                pass
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "probe"
            source = UnixStreamServer(str(path), Redirect)
            workers = [threading.Thread(target=s.serve_forever, daemon=True) for s in (source, destination)]
            for worker in workers:
                worker.start()
            try:
                self.assertFalse(tunnel.healthy(path, "owner", "test-only-owner-token"))
                self.assertEqual(source_tokens, ["Bearer test-only-owner-token"])
                self.assertEqual(captured, [])
            finally:
                for server, worker in zip((source, destination), workers):
                    server.shutdown()
                    worker.join()
                    server.server_close()

    def test_forward_bind_failure_prevents_authenticated_probe(self):
        callbacks = {}
        process = Mock()
        process.poll.return_value = None
        args = ["admin_tunnel.py", "--ssh-config", "/private/config", "--target", "cloud-a",
                "--token-file", "/private/token", "--user-id", "owner"]
        with patch.object(sys, "argv", args), patch.object(tunnel, "token_file", return_value="hidden"), \
                patch.object(tunnel.signal, "signal", side_effect=lambda sig, cb: callbacks.update({sig: cb})), \
                patch.object(tunnel.subprocess, "Popen", return_value=process), \
                patch.object(tunnel, "control", side_effect=lambda path, alias, op, *args: op == "check") as control, \
                patch.object(tunnel, "healthy") as probe, patch.object(tunnel, "stop"), \
                patch.object(tunnel.time, "sleep", side_effect=lambda _: callbacks[signal.SIGTERM]()):
            tunnel.main()
        self.assertEqual([c.args[2] for c in control.call_args_list], ["check", "forward"])
        probe.assert_not_called()
