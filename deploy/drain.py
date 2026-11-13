#!/usr/bin/env python3
"""Stop a deployment node only after recording clean shutdown of its invocation."""
import argparse
import json
from pathlib import Path
import subprocess


def command(*arguments):
    result = subprocess.run(arguments, capture_output=True, text=True, check=True, timeout=1900)
    return result.stdout.strip()


def drain(edge=False):
    proof = Path("/var/lib/miles-bootstrap/drain-proof.json")
    invocation = command("systemctl", "show", "--property=InvocationID", "--value", "miles.service")
    if invocation:
        command("systemctl", "stop", "miles.service")
        if command("systemctl", "show", "--property=Result", "--value", "miles.service") != "success":
            raise ValueError("service stop did not complete cleanly")
        logs = command("journalctl", "--no-pager", "--output=cat", "--lines=200", "_SYSTEMD_INVOCATION_ID=" + invocation)
        complete = False
        for line in logs.splitlines():
            try:
                complete = complete or json.loads(line).get("msg") == "shutdown complete"
            except (ValueError, AttributeError):
                pass
        if not complete:
            raise ValueError("shutdown proof missing")
        proof.write_text(json.dumps({"invocation":invocation, "boot_id":Path("/proc/sys/kernel/random/boot_id").read_text().strip()}))
        proof.chmod(0o600)
    else:
        previous = json.loads(proof.read_text())
        if previous["boot_id"] != Path("/proc/sys/kernel/random/boot_id").read_text().strip():
            raise ValueError("previous shutdown proof belongs to another boot")
    if edge:
        command("systemctl", "stop", "nginx.service")
        if command("systemctl", "show", "--property=Result", "--value", "nginx.service") != "success":
            raise ValueError("proxy did not stop cleanly")
    print("clean shutdown verified")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--edge", action="store_true")
    args = parser.parse_args()
    try:
        drain(args.edge)
    except Exception:
        raise SystemExit("Drain incomplete. Retain the node and inspect its shutdown proof.") from None
