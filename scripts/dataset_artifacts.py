#!/usr/bin/env python3
"""Verify measured native smoke artifacts and immutable historical evidence.

No service/AWS calls. Publishing copies only a verified, secret-free allowlist
into a NEW directory. Hashes cover exact file bytes; CRLF is never normalized.
"""
import argparse
from datetime import datetime
import hashlib
import json
import math
from pathlib import Path, PurePosixPath
import re
import shutil
import tarfile

HEX = re.compile(r"[0-9a-f]{64}")
ALIASES = ("owner-a", "owner-b", "owner-c")
PHASES = ("empty",) + ALIASES
FAMILIES = ("low-entropy", "compressed-identical", "random-distinct")
README = """# Native engine/metadata smoke evidence

Measured with a committed native collector, DynamoDB Local and S3/Moto.
This is a small controlled domain-identity demonstration, not a performance
benchmark or live AWS pilot. Raw Admin response JSON is retained; payloads,
credential responses and runtime directories are excluded.

Run metadata pins the source, Go version, test executable and dataset manifest.
The four phases retain distinct object keys: empty, first owner, second owner in
the same domain, third owner in another domain. They are sequential observations,
not independent throughput trials. Physical totals include setup/control/history;
layout is metadata-only and full GET hashes are separate. No ratio implies cost
saving, and absent restore/infrastructure billing is not a measured zero.

Verify from the repository root:
`python3 -B scripts/dataset_artifacts.py verify benchmarks/evidence/engine-metadata-pilot`

Reproduce using `benchmarks/engine-metadata-artifacts.md`. Actual timestamps,
request IDs, bucket incarnations and pack generations vary between runs.
"""


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(body):
    return hashlib.sha256(body).hexdigest()


def unique_object(pairs):
    out = {}
    for key, value in pairs:
        require(key not in out, "duplicate JSON field")
        out[key] = value
    return out


def checked_file(root, name):
    p = PurePosixPath(name)
    require(not p.is_absolute() and ".." not in p.parts and "\\" not in name, "unsafe artifact path")
    current = root
    require(root.is_dir() and not root.is_symlink(), "invalid artifact root")
    for part in p.parts:
        current = current / part
        require(not current.is_symlink(), "symlink in artifact path")
    require(current.is_file(), "missing/nonregular artifact")
    return current


def load(root, name):
    with checked_file(root, name).open("rb") as stream:
        body = stream.read((2 << 20) + 1)
    require(len(body) <= 2 << 20, "artifact exceeds byte budget")
    value = json.loads(body, object_pairs_hook=unique_object,
                       parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))
    def public(v):
        if isinstance(v, dict):
            require(not {str(k).lower() for k in v} & {"secret", "secret_access_key", "authorization", "token", "access_key_id", "ciphertext", "account_id", "kms_key_arn"}, "sensitive field in public artifact")
            for child in v.values(): public(child)
        elif isinstance(v, list):
            for child in v: public(child)
        elif isinstance(v, str):
            require(not v.startswith("arn:aws:"), "AWS ARN in public artifact")
    public(value)
    return value


def integer(n):
    require(type(n) is int and n >= 0, "expected nonnegative integer")
    return n


def stamp(text):
    at = datetime.fromisoformat(text.replace("Z", "+00:00"))
    require(at.tzinfo is not None, "timestamp must include timezone")
    return at


def check_ratio(value, numerator, denominator):
    if denominator == 0:
        require(value is None, "zero denominator must be null")
    else:
        require(type(value) in (float, int) and math.isfinite(value)
                and math.isclose(value, numerator / denominator, rel_tol=1e-12, abs_tol=1e-12),
                "incorrect ratio or denominator")


def check_tree(root, allowed):
    require(root.is_dir() and not root.is_symlink(), "bundle root must be a directory")
    seen = set()
    for path in root.rglob("*"):
        require(not path.is_symlink(), "symlink in artifact bundle")
        if path.is_dir():
            continue
        require(path.is_file(), "nonregular artifact")
        name = path.relative_to(root).as_posix()
        require(name in allowed, "unexpected artifact: " + name)
        seen.add(name)
    require(seen == set(allowed), "missing artifact")


def measurements(root, require_clean=True):
    run, manifest, workload = (load(root, p) for p in ("run.json", "dataset-manifest.json", "workload.json"))
    require(run["schema_version"] == 1 and run["status"] == "passed"
            and run["kind"] == "local-native-engine-metadata-smoke"
            and run["backend"] == "dynamodb-local+s3-moto", "incorrect evidence scope")
    require(re.fullmatch(r"[0-9a-f]{40}", run["source_commit"]) and HEX.fullmatch(run["test_binary_sha256"]), "missing source/binary identity")
    require(type(run["source_dirty"]) is bool, "missing source cleanliness")
    require(not require_clean or run["source_dirty"] is False, "dirty-source capture cannot be published")
    require(stamp(run["finished_at"]) >= stamp(run["started_at"]), "invalid capture window")
    require(manifest["profile"] == "smoke" and manifest["go_version"] == run["go_version"]
            and manifest["generator"] == "miles-aesctr-gzip-v1", "generator/toolchain mismatch")
    require(digest((root / "dataset-manifest.json").read_bytes()) == run["dataset_manifest_sha256"], "manifest byte checksum mismatch")
    files = {f["path"]: f for f in manifest["files"]}
    require(len(files) == len(manifest["files"]) == 9, "smoke file coverage mismatch")
    for name, f in files.items():
        require(PurePosixPath(name).name == name and f["group"] in FAMILIES
                and f["version"] in range(3) and HEX.fullmatch(f["sha256"]), "bad dataset entry")
        integer(f["bytes"])
    require(sum(f["bytes"] for f in files.values()) == manifest["total_bytes"], "dataset total mismatch")
    require({(f["group"], f["version"]) for f in files.values()} == {(g, v) for g in FAMILIES for v in range(3)}, "entropy/version coverage mismatch")
    cohorts = workload["cohorts"]
    require([c["alias"] for c in cohorts] == list(ALIASES), "cohort order mismatch")
    require(cohorts[0]["domain"] == cohorts[1]["domain"] != cohorts[2]["domain"], "domain comparison mapping mismatch")
    require(len({c["owner"] for c in cohorts}) == len({c["bucket_id"] for c in cohorts}) == 3, "owners/incarnations not distinct")
    require([p["name"] for p in run["phases"]] == list(PHASES), "phase coverage mismatch")
    identities = load(root, "identities.json")
    canonical = {(r["domain"], r["hash"]): r for r in identities}
    require(len(canonical) == len(identities), "duplicate canonical identity")
    expected = {(c["domain"], f["sha256"]) for c in cohorts for f in files.values()}
    require(set(canonical) == expected, "missing or extra domain identity")
    ranges = {}
    for row in identities:
        require(integer(row["stored_bytes"]) > 0 and row["stored_bytes"] <= integer(row["raw_bytes"]), "invalid canonical lengths")
        require(integer(row["location_revision"]) > 0 and row["pack_id"] and row["generation"], "unbound canonical generation")
        offset = integer(row["offset"])
        ranges.setdefault((row["pack_id"], row["generation"]), []).append((offset, offset+row["stored_bytes"]))
    for spans in ranges.values():
        spans.sort()
        require(all(b[0] >= a[1] for a, b in zip(spans, spans[1:])), "overlapping distinct canonical ranges")
    for f in files.values():
        a, c = (canonical[(x["domain"], f["sha256"])] for x in (cohorts[0], cohorts[2]))
        require(a["raw_bytes"] == c["raw_bytes"] == f["bytes"], "canonical raw length mismatch")
        require((a["pack_id"], a["generation"], a["offset"], a["stored_bytes"]) != (c["pack_id"], c["generation"], c["offset"], c["stored_bytes"]), "cross-domain identity reused source range")
    policy = load(root, "policy.json")
    require(policy["chunking"]["min_bytes"] > max(f["bytes"] for f in files.values()), "one-chunk smoke assumption not valid")
    allowed = {"run.json", "dataset-manifest.json", "workload.json", "policy.json", "identities.json", "verified-gets.json"}
    allowed.update("policies/" + a + ".json" for a in ALIASES)
    summaries = []
    for count, phase in enumerate(run["phases"]):
        require(phase["owners"] == count, "phase owner count mismatch")
        prefix = "phases/" + phase["name"] + "/"
        g = load(root, prefix + "global.json")
        allowed.add(prefix + "global.json")
        require(g["physical_complete"] is True and g["bytes_stored"] is not None, "incomplete physical snapshot")
        physical = integer(g["bytes_stored"])
        require(g["consistency"] == "approximate" and g["snapshot_id"] == phase["snapshot_id"], "snapshot identity/scope mismatch")
        require(stamp(g["collection_started_at"]) >= stamp(phase["workload_finished_at"])
                and stamp(g["as_of"]) >= stamp(g["collection_started_at"]), "stale collection")
        selected = {(c["domain"], f["sha256"]) for c in cohorts[:count] for f in files.values()}
        raw = sum(canonical[k]["raw_bytes"] for k in selected)
        stored = sum(canonical[k]["stored_bytes"] for k in selected)
        logical = count * manifest["total_bytes"]
        require((g["bytes_logical"], g["bytes_unique_raw"], g["bytes_unique_stored"], g["objects"], g["chunks"])
                == (logical, raw, stored, count * len(files), len(selected)), "global measured accounting mismatch")
        check_ratio(g["dedup_ratio"], logical, raw)
        check_ratio(g["compression_ratio"], raw, stored)
        check_ratio(g["total_reduction"], logical, physical)
        allocated = 0
        for i, c in enumerate(cohorts):
            bp, up = prefix + "bucket-" + c["alias"] + ".json", prefix + "usage-" + c["alias"] + ".json"
            allowed.update((bp, up))
            b, usage = load(root, bp), load(root, up)
            expected_logical = manifest["total_bytes"] if i < count else 0
            require(b["bucket"] == c["bucket"] and b["bucket_id"] == c["bucket_id"]
                    and b["snapshot_id"] == g["snapshot_id"] and b["as_of"] == g["as_of"], "bucket snapshot/incarnation mismatch")
            require(b["bytes_logical"] == expected_logical and usage["user_id"] == c["owner"]
                    and usage["bytes_logical_used"] == expected_logical, "logical quota mismatch")
            bucket_keys = {(c["domain"], f["sha256"]) for f in files.values()} if i < count else set()
            require(b["bytes_unique_raw"] == sum(canonical[k]["raw_bytes"] for k in bucket_keys)
                    and b["bytes_unique_stored"] == sum(canonical[k]["stored_bytes"] for k in bucket_keys), "bucket unique totals mismatch")
            require(usage["bytes_logical_reserved"] == usage["bytes_logical_staged"] == usage["active_upload_count"] == 0, "unfinished quota ledger")
            allocated += integer(b["bytes_stored"])
            for field, n, d in (("dedup_ratio", b["bytes_logical"], b["bytes_unique_raw"]), ("compression_ratio", b["bytes_unique_raw"], b["bytes_unique_stored"]), ("total_reduction", b["bytes_logical"], b["bytes_stored"])):
                check_ratio(b[field], n, d)
            if count == 3:
                for family in FAMILIES:
                    name = prefix + "layout-" + c["alias"] + "-" + family + ".json"
                    allowed.add(name)
                    layout = load(root, name)
                    f = next(f for f in files.values() if f["group"] == family and f["version"] == 0)
                    row = canonical[(c["domain"], f["sha256"])]
                    require(layout["bucket"] == c["bucket"] and layout["domain_id"] == c["domain"]
                            and layout["key"] == "dataset/"+f["path"] and layout["revision"] == 1
                            and layout["size"] == f["bytes"] and layout["as_of"] == g["as_of"]
                            and layout["chunk_occurrences"] == 1 and layout["integrity"] == "metadata-only/not-verified", "layout binding mismatch")
                    require(len(layout["packs"]) == 1 and (layout["packs"][0]["pack_id"], layout["packs"][0]["generation"]) == (row["pack_id"], row["generation"]), "layout canonical mismatch")
        require(allocated + integer(g["unallocated_physical_bytes"]) == physical, "physical allocation double count")
        summaries.append({k: g[k] for k in ("bytes_logical", "bytes_unique_raw", "bytes_unique_stored", "bytes_stored", "dedup_ratio", "compression_ratio", "total_reduction")})
    gets = load(root, "verified-gets.json")
    observed = {(g["cohort"], g["file"]): g for g in gets}
    require(len(observed) == len(gets) == 3 * len(files), "GET coverage mismatch")
    for c in cohorts:
        for name, f in files.items():
            require(observed[(c["alias"], name)]["sha256"] == f["sha256"] and observed[(c["alias"], name)]["bytes"] == f["bytes"], "GET checksum mismatch")
    return allowed, {"kind": run["kind"], "source_commit": run["source_commit"], "verified_gets": len(gets), "identities": len(identities), "phases": summaries}


def checksums(root, expected=None):
    names = set()
    index = checked_file(root, "sha256sums.txt")
    require(index.stat().st_size <= 128 << 10, "checksum index exceeds budget")
    for line in index.read_text().splitlines():
        sha, name = line.split("  ", 1)
        p = PurePosixPath(name)
        require(HEX.fullmatch(sha) and not p.is_absolute() and ".." not in p.parts
                and "\\" not in name and name not in names, "invalid checksum member")
        path = checked_file(root, name)
        with path.open("rb") as stream:
            actual = hashlib.file_digest(stream, "sha256").hexdigest()
        require(actual == sha, "byte checksum mismatch: " + name)
        names.add(name)
    require(expected is None or names == set(expected), "checksum coverage mismatch")


def verify(root):
    allowed, summary = measurements(root)
    allowed.add("README.md")
    check_tree(root, allowed | {"sha256sums.txt"})
    checksums(root, allowed)
    return summary


def publish(source, out):
    if out.exists() or out.is_symlink():
        raise FileExistsError("publish output must not exist")
    allowed, _ = measurements(source)
    check_tree(source, allowed)
    out.mkdir(mode=0o700)  # refuse any existing output
    for name in sorted(allowed):
        target = out / name
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        shutil.copyfile(source / name, target)
    (out / "README.md").write_text(README, encoding="utf-8", newline="\n")
    allowed.add("README.md")
    (out / "sha256sums.txt").write_text("".join(digest((out / name).read_bytes()) + "  " + name + "\n" for name in sorted(allowed)), encoding="utf-8", newline="\n")
    return verify(out)


def historical(root):
    checksums(root, {"README.md", "artifacts.json", "cleanup.json", "dataset-manifest.json", "orchestration.py", "reproduction-config.example.json", "runs.tar.gz", "summary.json"})
    manifest, summary = load(root, "dataset-manifest.json"), load(root, "summary.json")
    manifest_sha = digest((root / "dataset-manifest.json").read_bytes())
    require(manifest_sha == summary["manifest_sha256"], "historical manifest mismatch")
    expected = {f["path"]: f["sha256"] for f in manifest["files"]}
    require(len(expected) == 90 and sum(f["bytes"] for f in manifest["files"]) == manifest["total_bytes"], "historical input coverage")
    artifacts = {a["path"]: a["sha256"] for a in load(root, "artifacts.json")}
    seen, gets = set(), 0
    with tarfile.open(root / "runs.tar.gz") as archive:
        for member in archive:
            if member.isdir():
                continue
            require(member.isfile() and member.name in artifacts and member.name not in seen and member.size <= 32 << 20, "unexpected archive member")
            body = archive.extractfile(member).read()
            require(digest(body) == artifacts[member.name], "historical raw result checksum")
            run = json.loads(body, object_pairs_hook=unique_object)
            require(run["status"] == "passed" and run["manifest_sha256"] == manifest_sha, "historical run status/manifest mismatch")
            samples = [s for s in run["samples"] if s["operation"] == "GetObject"]
            require(len(samples) == 360 and all(s["sha256"] == expected[s["file"]] for s in samples)
                    and all(not s.get("error") for s in run["samples"]), "historical GET verification failure")
            gets += len(samples)
            seen.add(member.name)
    require(seen == set(artifacts) and gets == summary["verified_full_file_gets"], "historical archive coverage mismatch")
    return {"kind": "historical-single-node-badger-warm", "source_commit": summary["source_commit"], "manifest_sha256": manifest_sha, "files": 90, "bytes": manifest["total_bytes"], "raw_runs": len(seen), "verified_gets": gets, "current_release_measurement": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("verify", "publish", "audit-historical"))
    parser.add_argument("root", type=Path)
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    try:
        if args.action == "publish":
            require(args.out is not None, "publish requires --out")
            result = publish(args.root, args.out)
        elif args.action == "verify":
            result = verify(args.root)
        else:
            result = historical(args.root)
        print(json.dumps(result, indent=2, allow_nan=False))
    except (ValueError, KeyError, TypeError, OSError, StopIteration, tarfile.TarError) as exc:
        parser.exit(1, f"artifact verification failed: {exc}\n")


if __name__ == "__main__":
    main()
