#!/usr/bin/env python3
"""Stage a verified release without starting services or overwriting operator config."""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path, PurePosixPath
import platform
import shutil


def install(bundle, root, profile, address):
    manifest_bytes = (bundle / "manifest.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    native = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if manifest["schema_version"] != 1 or manifest["os"] != "linux" or manifest["arch"] != native:
        raise ValueError("release architecture does not match this Linux host")
    ip = ipaddress.ip_address(address)
    private = any(ip in ipaddress.ip_network(cidr) for cidr in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
    if ip.version != 4 or not (ip.is_loopback if profile == "edge" else private):
        raise ValueError("Cloud needs a private IPv4 address; Edge needs loopback")
    files = manifest["files"]
    if not files or len(files) > 100:
        raise ValueError("invalid release file inventory")
    for name, expected in files.items():
        path = PurePosixPath(name)
        source = bundle / name
        if path.is_absolute() or ".." in path.parts or source.is_symlink() or not source.is_file():
            raise ValueError("unsafe release member")
        if hashlib.sha256(source.read_bytes()).hexdigest() != expected:
            raise ValueError("release file checksum mismatch: " + name)
    release_id = hashlib.sha256(manifest_bytes).hexdigest()
    prefix, config = root / "opt/miles", root / "etc/miles"
    release = prefix / "releases" / release_id
    release.mkdir(parents=True, exist_ok=True)
    for name in [*files, "manifest.json"]:
        target = release / name
        target.parent.mkdir(parents=True, exist_ok=True)
        data = (bundle / name).read_bytes()
        if target.exists():
            if target.is_symlink() or target.read_bytes() != data:
                raise ValueError("existing immutable release differs")
        else:
            with target.open("xb") as stream:
                stream.write(data)
            target.chmod(0o755 if name.startswith("bin/") else 0o644)
    config.mkdir(parents=True, exist_ok=True)
    startup = json.loads((release / ("configs/" + profile + ".example.json")).read_text())
    startup["health_address"] = address + ":9100"
    initial = {
        "startup.json": (json.dumps(startup, indent=2) + "\n").encode(),
        "aws-config": (release / ("deploy/" + profile + "/aws-config.example")).read_bytes(),
        "runtime.env": (release / ("deploy/" + profile + "/runtime.env.example")).read_bytes(),
    }
    for name, data in initial.items():
        target = config / name
        if not target.exists():
            with target.open("xb") as stream:
                stream.write(data)
            target.chmod(0o644)
        elif target.is_symlink():
            raise ValueError("operator config must not be a symlink")
    actual = json.loads((config / "startup.json").read_text())
    if actual.get("profile") != profile or actual.get("mode") != "runtime" or actual.get("shutdown_seconds") != 1800:
        raise ValueError("operator config must retain the runtime profile and 1800-second drain")
    units = root / "etc/systemd/system"
    units.mkdir(parents=True, exist_ok=True)
    for name in ("miles.service", "miles-lifecycle.service"):
        target = units / name
        if target.is_symlink():
            raise ValueError("unit destination must not be a symlink")
        shutil.copyfile(release / ("deploy/systemd/" + name), target)
    temporary = prefix / "current.new"
    if temporary.exists() or temporary.is_symlink():
        raise ValueError("unfinished release activation exists")
    temporary.symlink_to("releases/" + release_id, target_is_directory=True)
    os.replace(temporary, prefix / "current")
    return release_id


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--profile", choices=["cloud", "edge"], required=True)
    parser.add_argument("--address", required=True)
    parser.add_argument("--root", type=Path, default=Path("/"), help="Staging root; does not start services")
    args = parser.parse_args()
    print(install(args.bundle, args.root, args.profile, args.address))
