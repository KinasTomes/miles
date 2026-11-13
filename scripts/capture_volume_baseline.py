"""Freeze account/region EBS IDs before a test creates resources."""

import argparse
import datetime
import hashlib
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from scripts.capture_runtime_inventory import aws_json, volume_ids


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("profile", "region", "account-id", "run-id", "output"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    if len(args.account_id) != 12 or not args.account_id.isdigit() or not args.run_id.strip():
        parser.error("12-digit account ID and run ID required")
    if aws_json(args.profile, args.region, "sts", "get-caller-identity").get("Account") != args.account_id:
        raise ValueError("AWS account mismatch")
    volumes = volume_ids(aws_json(args.profile, args.region, "ec2", "describe-volumes"))
    baseline = {"schema_version": "miles-volume-baseline/v1", "run_id": args.run_id, "account_id": args.account_id, "region": args.region, "captured_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(), "volumes": sorted(volumes)}
    payload = (json.dumps(baseline, indent=2, sort_keys=True) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    print(hashlib.sha256(payload).hexdigest())


if __name__ == "__main__":
    main()
