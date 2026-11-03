#!/usr/bin/env python3
"""Keep an authenticated owner Admin tunnel on one of the explicitly trusted SSH aliases."""
import argparse
import base64
from http.client import HTTPConnection, HTTPException
import os
from pathlib import Path
import re
import signal
import socket
import stat
import subprocess
import tempfile
import time
from urllib.parse import quote


def token_file(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != os.geteuid():
            raise ValueError("token file must be an owned regular 0600 file")
        token = os.read(fd, 256).decode().strip()
        if not re.fullmatch(r"[a-f0-9]{32}\.[A-Za-z0-9_-]{43}", token):
            raise ValueError("invalid Admin token file")
        secret = token.split(".")[1]
        if base64.urlsafe_b64encode(base64.urlsafe_b64decode(secret + "=")).decode().rstrip("=") != secret:
            raise ValueError("noncanonical Admin token file")
        return token
    finally:
        os.close(fd)


def command(config, alias, control_path):
    return ["ssh", "-F", str(config), "-N", "-T", "-o", "BatchMode=yes",
            "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes",
            "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2",
            "-M", "-S", str(control_path), "-o", "ControlPersist=no",
            "-o", "ForkAfterAuthentication=no", "-o", "ClearAllForwardings=yes", "--", alias]


def control(path, alias, operation, forwards=()):
    cmd = ["ssh", "-F", "none", "-S", str(path), "-O", operation]
    for address in forwards:
        cmd.extend(["-L", address])
    try:
        return subprocess.run(cmd + ["--", alias], stdin=subprocess.DEVNULL,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              timeout=2).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


def healthy(path, user, token):
    # A localhost TCP listener can outlive or predate SSH. The private socket
    # cannot be replaced by another user between the ownership check and send.
    if not isinstance(path, Path):
        return False
    connection = HTTPConnection("localhost", timeout=2)
    try:
        connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        connection.sock.settimeout(2)
        connection.sock.connect(str(path))
        connection.request("GET", f"/admin/v1/users/{quote(user, safe='')}/usage",
                           headers={"Authorization": "Bearer " + token})
        # HTTPConnection returns redirects as responses; it never follows them.
        return connection.getresponse().status == 200
    except (OSError, ValueError, HTTPException):
        return False
    finally:
        connection.close()


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ssh-config", type=Path, required=True)
    parser.add_argument("--target", action="append", required=True, help="Pinned SSH alias; repeat for fallback nodes")
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--user-id", required=True)
    parser.add_argument("--local-port", type=int, default=9000)
    args = parser.parse_args()
    if not 1024 <= args.local_port <= 65535 or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", args.user_id):
        parser.error("invalid local port/user")
    if not all(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", target) for target in args.target):
        parser.error("targets must be explicit SSH aliases")
    token_file(args.token_file)  # Validate before starting any process.
    running = True

    def interrupt(*_):
        nonlocal running
        running = False

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, interrupt)
    while running:
        for alias in args.target:
            if not running:
                break
            with tempfile.TemporaryDirectory(prefix="miles-admin-") as directory:
                control_path, probe = Path(directory) / "control", Path(directory) / "probe"
                # The SSH config and its host keys are operator-trusted inputs.
                process = subprocess.Popen(command(args.ssh_config, alias, control_path),
                                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL)
                try:
                    deadline = time.monotonic() + 15
                    good = bound = False
                    while running and process.poll() is None and time.monotonic() < deadline:
                        if not bound:
                            if not control(control_path, alias, "check"):
                                time.sleep(0.25)
                                continue
                            forwards = (f"127.0.0.1:{args.local_port}:127.0.0.1:9000",
                                        f"{probe}:127.0.0.1:9000")
                            if not control(control_path, alias, "forward", forwards):
                                break
                            bound = True  # SSH acknowledged both listener binds.
                        good = healthy(probe, args.user_id, token_file(args.token_file))
                        if good:
                            break
                        time.sleep(0.25)
                    if good:
                        print("Admin tunnel ready: " + alias, flush=True)
                    while good and running and process.poll() is None:
                        time.sleep(2)
                        good = healthy(probe, args.user_id, token_file(args.token_file))
                finally:
                    stop(process)
        if running:
            time.sleep(2)


if __name__ == "__main__":
    main()
