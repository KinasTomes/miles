#!/usr/bin/env python3
"""Build one immutable Linux release for both Cloud and Edge; no AWS operations."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
FILES = [
    "deploy/install.py", "deploy/cloud/lifecycle.py",
    "deploy/systemd/miles.service", "deploy/systemd/miles-lifecycle.service",
    "deploy/cloud/aws-config.example", "deploy/cloud/runtime.env.example",
    "deploy/edge/aws-config.example", "deploy/edge/runtime.env.example",
    "deploy/edge/nginx.conf", "configs/cloud.example.json", "configs/edge.example.json",
]


def build(output, arch):
    if output.exists() or output.is_symlink():
        raise ValueError("release output must be a new file")
    if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip():
        raise ValueError("build from a clean committed worktree")
    revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    version = subprocess.check_output(["go", "version"], cwd=ROOT, text=True).strip()
    files = {name: (ROOT / name).read_bytes() for name in FILES}
    env = dict(os.environ, GOOS="linux", GOARCH=arch, CGO_ENABLED="0")
    with tempfile.TemporaryDirectory(prefix="miles-release-") as temporary:
        for name in ("miles", "dsgctl"):
            binary = Path(temporary) / name
            subprocess.run(["go", "build", "-trimpath", "-ldflags",
                            "-X github.com/KinasTomes/miles/internal/buildinfo.Version=" + revision,
                            "-o", str(binary), "./cmd/" + name], cwd=ROOT, env=env, check=True)
            files["bin/" + name] = binary.read_bytes()
    manifest = {"schema_version": 1, "source_commit": revision, "go_version": version,
                "os": "linux", "arch": arch,
                "files": {name: hashlib.sha256(data).hexdigest() for name, data in sorted(files.items())}}
    files["manifest.json"] = (json.dumps(manifest, indent=2) + "\n").encode()
    with output.open("xb") as destination:
        with gzip.GzipFile(fileobj=destination, mode="wb", filename="", mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w") as archive:
                for name, data in sorted(files.items()):
                    item = tarfile.TarInfo(name)
                    item.size, item.mtime = len(data), 0
                    item.mode = 0o755 if name.startswith("bin/") else 0o644
                    archive.addfile(item, io.BytesIO(data))
    return {"source_commit": revision, "arch": arch,
            "sha256": hashlib.sha256(output.read_bytes()).hexdigest(), "bytes": output.stat().st_size}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--arch", choices=["amd64", "arm64"],
                        default={"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine(), "amd64"))
    arguments = parser.parse_args()
    print(json.dumps(build(arguments.out, arguments.arch), indent=2))
