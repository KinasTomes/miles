"""Create a private pre-destroy manifest after checking frozen Terraform state."""

import argparse
import datetime
import hashlib
import hmac
import json
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from scripts.freeze_terraform_inventory import compare_inventory, extract_resources
from scripts.verify_teardown_absence import METADATA_INVENTORY_KEYS, TeardownAbsenceVerifier


def build_manifest(state_bytes, state_sha256, inventory, run_id, account, region):
    actual_hash = hashlib.sha256(state_bytes).hexdigest()
    if not hmac.compare_digest(actual_hash, state_sha256):
        raise ValueError("frozen state hash mismatch")
    if not run_id.strip() or len(account) != 12 or not account.isdigit() or not region.strip():
        raise ValueError("invalid run ID, account, or region")
    if not isinstance(inventory, dict) or inventory.get("expected_missing"):
        raise ValueError("inventory is invalid or incomplete")
    if inventory.get("account_id", account) != account or inventory.get("region", region) != region:
        raise ValueError("inventory account or region mismatch")
    errors = compare_inventory(extract_resources(json.loads(state_bytes)), inventory)
    if errors:
        raise ValueError("state reconciliation failed: " + "; ".join(errors))
    resources = {key: value for key, value in inventory.items() if key not in METADATA_INVENTORY_KEYS and value}
    counts = {key: len(value) if isinstance(value, list) else 1 for key, value in resources.items()}
    manifest = {
        "schema_version": "miles-teardown-manifest/v1", "run_id": run_id,
        "account_id": account, "region": region,
        "ownership_scope": {"project": "miles", "environment": "dev"},
        "expected_types": sorted(resources), "expected_counts": counts, "expected_resources": resources,
        "provenance": {"source": "terraform_pre_destroy", "created_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(), "state_sha256": actual_hash},
    }
    _, validation_errors = TeardownAbsenceVerifier.validate_manifest_schema_and_reconciliation(manifest, inventory, account_id=account, region=region)
    if validation_errors:
        raise ValueError("manifest validation failed: " + "; ".join(validation_errors))
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("state-json", "state-sha256", "inventory-json", "run-id", "account-id", "region", "output"):
        parser.add_argument("--" + option, required=True)
    args = parser.parse_args()
    manifest = build_manifest(Path(args.state_json).read_bytes(), args.state_sha256, json.loads(Path(args.inventory_json).read_text()), args.run_id, args.account_id, args.region)
    payload = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    print(hashlib.sha256(payload).hexdigest())


if __name__ == "__main__":
    main()
