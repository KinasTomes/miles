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
from scripts.freeze_terraform_inventory import augment_inventory, compare_inventory, extract_resources
from scripts.capture_runtime_inventory import reconcile_snapshot
from scripts.capture_account_inventory import validate_baseline
from scripts.verify_teardown_absence import METADATA_INVENTORY_KEYS, TeardownAbsenceVerifier, validate_pending_deletion_approvals


def build_manifest(state_bytes, state_sha256, inventory, run_id, account, region, runtime_bytes=None, runtime_sha256=None, account_baseline_bytes=None, account_baseline_sha256=None, approved_pending=None, deploy_role=None, audit_role=None):
    actual_hash = hashlib.sha256(state_bytes).hexdigest()
    if not hmac.compare_digest(actual_hash, state_sha256):
        raise ValueError("frozen state hash mismatch")
    if not run_id.strip() or len(account) != 12 or not account.isdigit() or not region.strip():
        raise ValueError("invalid run ID, account, or region")
    if not isinstance(inventory, dict) or inventory.get("expected_missing"):
        raise ValueError("inventory is invalid or incomplete")
    if inventory.get("account_id", account) != account or inventory.get("region", region) != region:
        raise ValueError("inventory account or region mismatch")
    state_resources = extract_resources(json.loads(state_bytes))
    inventory = augment_inventory(state_resources, inventory)
    if (runtime_bytes is None) != (runtime_sha256 is None):
        raise ValueError("runtime inventory and hash must be provided together")
    if (account_baseline_bytes is None) != (account_baseline_sha256 is None):
        raise ValueError("account baseline and hash must be provided together")
    account_baseline = None
    if account_baseline_bytes is not None:
        account_baseline = validate_baseline(account_baseline_bytes, account_baseline_sha256, run_id, account, region)
    if runtime_bytes is not None:
        if not hmac.compare_digest(hashlib.sha256(runtime_bytes).hexdigest(), runtime_sha256):
            raise ValueError("runtime inventory hash mismatch")
        snapshot = json.loads(runtime_bytes)
        errors = reconcile_snapshot(snapshot, state_resources, snapshot, run_id, account, region)
        if errors:
            raise ValueError("runtime inventory invalid: " + "; ".join(errors))
        for key in ("asg_names", "instances", "volumes"):
            inventory[key] = sorted(set(inventory.get(key, [])) | set(snapshot[key]))
    errors = compare_inventory(state_resources, inventory)
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
    if runtime_bytes is not None:
        manifest["provenance"]["runtime_sha256"] = runtime_sha256
    if account_baseline_bytes is not None:
        manifest["provenance"]["account_baseline_sha256"] = account_baseline_sha256
    if deploy_role:
        manifest["provenance"]["deploy_role"] = deploy_role
    if audit_role:
        if account_baseline is None or audit_role in account_baseline["roles"]:
            raise ValueError("temporary audit role must be new since the account baseline")
        manifest["provenance"]["audit_role"] = audit_role
    pending = validate_pending_deletion_approvals(
        [] if approved_pending is None else approved_pending, resources, state_resources, account_baseline,
    )
    manifest["provenance"]["approved_pending_deletion"] = [f"{kind}={identity}" for kind, identity in sorted(pending)]
    _, validation_errors = TeardownAbsenceVerifier.validate_manifest_schema_and_reconciliation(manifest, inventory, account_id=account, region=region)
    if validation_errors:
        raise ValueError("manifest validation failed: " + "; ".join(validation_errors))
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("state-json", "state-sha256", "inventory-json", "run-id", "account-id", "region", "output"):
        parser.add_argument("--" + option, required=True)
    parser.add_argument("--runtime-json")
    parser.add_argument("--runtime-sha256")
    parser.add_argument("--account-baseline")
    parser.add_argument("--account-baseline-sha256")
    parser.add_argument("--approve-pending-deletion", action="append", metavar="TYPE=ID", default=[],
                        help="Exact approved resource, kms_key=<key ID or ARN> or secret=<secret ARN>")
    parser.add_argument("--deploy-role", help="Persistent deploy role whose IAM policy must be restored")
    parser.add_argument("--audit-role", help="Temporary read-only role that must be deleted before final root audit")
    args = parser.parse_args()
    manifest = build_manifest(Path(args.state_json).read_bytes(), args.state_sha256, json.loads(Path(args.inventory_json).read_text()), args.run_id, args.account_id, args.region, Path(args.runtime_json).read_bytes() if args.runtime_json else None, args.runtime_sha256, Path(args.account_baseline).read_bytes() if args.account_baseline else None, args.account_baseline_sha256, args.approve_pending_deletion, args.deploy_role, args.audit_role)
    payload = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    print(hashlib.sha256(payload).hexdigest())


if __name__ == "__main__":
    main()
