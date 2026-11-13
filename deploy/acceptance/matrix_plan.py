"""Offline full-matrix execution manifest. No AWS, Terraform or workload dispatch.

This module deliberately does NOT claim that the focused acceptance kit can
execute the new matrix. It produces a frozen, reviewable schedule and identifies
the adapters/proofs still required before an account owner may approve a run.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from deploy.acceptance.common import ROOT, require
from deploy.acceptance.process import read_recipe
from scripts.validate_benchmark_matrix import load as load_matrix, validate as validate_matrix

SCHEMA_VERSION = 1
EXPECTED_GATE_REVIEWS = (
    "approve-foundation",
    "approve-hosts",
    "approve-active",
    "approve-cleanup-grant",
    "approve-destroy",
)
PAIRED_COMPARATORS = frozenset({"direct-s3"})
G3_SCENARIOS = frozenset({"warm", "overwrite", "domain", "multipart", "edge-clone"})
G4_SCENARIOS = frozenset({"cold", "recovery"})
COLD_PROOF = (
    "observed-backend-storage-class-and-version",
    "transition-timestamps-and-minimum-retention-cost",
    "IR-immediate-read-or-Flexible-restore-state",
    "restoration-completion-expiry-and-revival",
    "full-get-sha256-and-authoritative-physical-inventory",
)
RECOVERY_PROOF = (
    "fault-injection-scope-approval",
    "epoch-and-history-before-after",
    "lease-fence-and-journal-event-samples",
    "sealed-cut-and-checkpoint-identity",
    "rebuild-or-replay-correctness-and-full-get-sha256",
)


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def validate_focused_recipe(path):
    """Verify that the reused template has explicit rollout/cleanup gates."""
    recipe = read_recipe(Path(path))
    reviews = {
        s["id"] for section in ("steps", "cleanup_steps")
        for s in recipe[section] if "review" in s
    }
    require(set(EXPECTED_GATE_REVIEWS) <= reviews, "Focused template lacks an approval gate")
    commands = {
        tuple(s["command"]) for section in ("steps", "cleanup_steps")
        for s in recipe[section] if "command" in s
    }
    require(("trial", "--concurrency", "1") in commands, "Focused template has no C1 proof")
    require(("trial", "--concurrency", "4") in commands, "Focused template has no C4 proof")
    require(("cleanup",) in commands and ("finish",) in commands
            and ("logout",) in commands, "Focused template lacks full cleanup/audit")
    return recipe


def case_proofs(case):
    """Evidence requirements are obligations, not assertions of implementation."""
    base = [
        "source-release-and-run-config-hashes",
        "verified-dataset-manifest-and-full-file-sha256",
        "per-sample-success-failure-and-s3-request-correlation",
        "before-after-storage-cache-journal-and-node-observations",
        "accounting-window-and-no-false-zero-costs",
        "fresh-backend-or-verified-reset-not-just-a-new-prefix",
    ]
    if case["scenario"] in G3_SCENARIOS:
        base.append("warmbench-result-and-sample-counts")
    if case["scenario"] == "domain":
        base.extend((
            "two-independent-signed-debug-principals-and-bucket-bindings",
            "runtime-dedup-domain-and-per-put-physical-counters",
            "inter-tenant-vs-intra-tenant-contrast",
        ))
    if case["scenario"] == "edge-clone":
        base.extend((
            "distinct-source-destination-credentials-and-buckets",
            "source-prefix-and-verified-get-put-hash-receipt",
            "source-destination-domain-readbacks-and-network-path",
        ))
    if case["scenario"] == "multipart":
        base.extend(("8mib-boundary-manifest", "actual-api-dispatch-and-part-count"))
    if case["scenario"] == "warm":
        base.append("verified-cache-reset-and-per-node-hit-miss")
    if case["scenario"] == "cold":
        base.extend(COLD_PROOF)
    if case["scenario"] == "recovery":
        base.extend(RECOVERY_PROOF)
    if case["comparator"] == "direct-s3":
        base.append("paired-direct-s3-same-transport-payload-region-and-time-window")
    elif case["comparator"] == "domain-contrast":
        base.append("matched-shared-and-separated-domain-baselines")
    elif case["comparator"] == "matched-get-put":
        base.append("matched-client-mediated-get-put-direct-s3-route")
    elif case["comparator"] == "policy-control":
        base.append("matched-cold-policy-control")
    elif case["comparator"] == "fault-contract":
        base.append("native-r1-r2-fault-and-recovery-proof")
    return base


def planned_commands(case, repetition, read_repeats):
    """Safe placeholders for the agent to adapt, never an executable recipe.

    These are only warmbench argument *templates*: no auth, bucket setup,
    route/network readiness, AWS approval, rollback or teardown is implied.
    """
    if case["runner"] != "warmbench":
        return []
    case_name = case["id"]
    common = [
        "warmbench", "--config", "<PRIVATE_CASE_CONFIG_0600>",
        "--manifest", "<PINNED_DATASET_MANIFEST>",
        "--out", f"<NEW_PRIVATE_OUT_DIR>/{case_name}/rep{repetition}/miles",
        "--scenario", case["scenario"], "--variant", case["variant"],
        "--domain", case["domain"],
        "--concurrency", str(case["concurrency"]),
        "--read-repeats", str(read_repeats),
        "--fail-fast", "--retain-objects",
    ]
    if case["scenario"] in {"domain", "edge-clone"}:
        common.extend(("--second-config", "<SECOND_PRIVATE_TENANT_CONFIG_0600>"))
    if case["scenario"] == "edge-clone":
        common.extend(("--source-prefix", "<VERIFIED_SEEDED_SOURCE_PREFIX>"))
    invocations = [{"role": "miles", "argv_template": common}]
    if case["comparator"] == "direct-s3":
        # Explicit separate direct-S3 run: must be paired and identical in
        # manifest/route/schedule, never inferred from a simulator.
        direct = common.copy()
        direct[direct.index("--config") + 1] = "<PRIVATE_DIRECT_S3_CONFIG_0600>"
        direct[direct.index("--out") + 1] = (
            f"<NEW_PRIVATE_OUT_DIR>/{case_name}/rep{repetition}/direct-s3"
        )
        invocations.append({"role": "direct-s3", "argv_template": direct})
    return invocations


def build(matrix, recipe_path, source_sha):
    validation = validate_matrix(matrix)
    validate_focused_recipe(recipe_path)
    require(isinstance(source_sha, str) and len(source_sha) == 40
            and all(c in "0123456789abcdef" for c in source_sha),
            "Pin a full 40-character lowercase source commit")
    slots = []
    for case in matrix["cases"]:
        scenario = case["scenario"]
        phase = "G3" if scenario in G3_SCENARIOS else "G4"
        require(phase == "G3" or scenario in G4_SCENARIOS, "Unsupported scenario")
        for repetition in range(1, matrix["protocol"]["repetitions"] + 1):
            # Slots are operator choices, never an instruction to auto-dispatch.
            slots.append({
                "slot": f'{case["id"]}-rep{repetition}',
                "case_id": case["id"],
                "repetition": repetition,
                "phase": phase,
                "scenario": scenario,
                "topology": case["topology"],
                "variant": case["variant"],
                "concurrency": case["concurrency"],
                "dataset": case["dataset"],
                "storage_class": case["storage_class"],
                "domain": case["domain"],
                "comparator": case["comparator"],
                "needs_matched_direct_s3": case["comparator"] in PAIRED_COMPARATORS,
                "read_repeats": matrix["protocol"]["read_repeats"],
                "runner": case["runner"],
                "kit_adapter_status": (
                    "cold-read-inventory-only" if scenario == "cold"
                    else "native-provider-implemented-offline" if scenario == "recovery"
                    else "operator-command-implemented-offline"
                ),
                "operator_command_template": [
                    "python3", "<FROZEN_SOURCE>/deploy/acceptance/run.py", "--run-dir", "<PRIVATE_RUN>",
                    "benchmark-case", "--case-id", case["id"], "--repetition", str(repetition), "--confirm-run", "<RUN_ID>",
                ],
                "non_executable_argv_templates": planned_commands(
                    case, repetition, matrix["protocol"]["read_repeats"]
                ),
                "evidence_requirements": case_proofs(case),
                "result": "not-executed",
            })
    require(len(slots) == 60 and len({x["slot"] for x in slots}) == len(slots),
            "Unexpected/duplicate trial slots")
    return {
        "schema_version": SCHEMA_VERSION,
        "source_commit": source_sha,
        "status": "offline-plan-only",
        "aws_approval": False,
        "benchmark_executed": False,
        "full_run_executable": False,
        "execution_mode": "operator-driven",
        "matrix_validation": validation,
        "repetitions": matrix["protocol"]["repetitions"],
        "total_trial_slots": len(slots),
        "matched_direct_s3_pairs": sum(x["needs_matched_direct_s3"] for x in slots),
        "warmbench_slots_not_integrated": 0,
        "cold_recovery_slots_blocked": sum(x["scenario"] == "cold" for x in slots),
        "focused_kit_supported": [
            "infrastructure-staging-with-reviewed-iam-and-saved-plans",
            "focused-edge-get-c1-and-c4",
            "native-throttled-shutdown",
            "ownership-bound-cleanup-and-final-audit",
        ],
        "blocking_gates": [
            "review-independent-command-modules-and-native-evidence-on-pinned-source",
            "implement-full-cold-revival-protection-window-acceptance",
            "approve-direct-s3-lifecycle-and-verify-live-per-node-reset-and-isolation",
            "verify-real-edge-topology-or-report-aws-edge-profile-only",
            "re-estimate-time-aws-prices-iam-expiry-and-cleanup-reserve-for-60-trials",
            "approve-source-matrix-thresholds-iam-cost-and-exact-destroy-plan",
        ],
        "slots": slots,
    }


def write_private(directory, matrix_path, recipe_path, source_sha):
    """Emit new 0700 folder with 0600 immutable-planning inputs only."""
    output = Path(directory).expanduser().absolute()
    require(not output.exists() and not output.is_symlink() and not output.is_relative_to(ROOT),
            "Choose a new private output directory outside the repository")
    require(output.parent.is_dir() and not output.parent.is_symlink(), "Invalid output parent")
    require(not output.parent.resolve().is_relative_to(ROOT),
            "Private output parent must be outside the repository")
    matrix = load_matrix(matrix_path)
    manifest = build(matrix, recipe_path, source_sha)
    manifest["input_sha256"] = {
        "matrix": sha256(matrix_path),
        "focused_recipe": sha256(recipe_path),
    }
    output.mkdir(mode=0o700)
    try:
        path = output / "offline-execution-manifest.json"
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as handle:
            json.dump(manifest, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
    except BaseException:
        path.unlink(missing_ok=True)
        output.rmdir()
        raise
    return manifest


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--matrix", type=Path, default=ROOT / "benchmarks/workload-matrix.json")
    parser.add_argument("--focused-template", type=Path,
                        default=ROOT / "deploy/acceptance/templates/focused-get-shutdown.json")
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--source-commit", default=None)
    args = parser.parse_args(argv)
    try:
        head = subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True
        ).strip()
        require(not subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT).strip(),
                "Commit source before freezing a new execution manifest")
        require(args.source_commit is None or args.source_commit == head,
                "Declared source commit differs from checkout HEAD")
        source = head
        result = write_private(args.out_dir, args.matrix, args.focused_template, source)
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        parser.exit(1, "BLOCKED (offline): " + str(exc) + "\n")
    print(json.dumps({
        "source_commit": result["source_commit"],
        "case_count": result["matrix_validation"]["validated_cases"],
        "trial_slots": result["total_trial_slots"],
        "matched_direct_s3_pairs": result["matched_direct_s3_pairs"],
        "warmbench_not_integrated_slots": result["warmbench_slots_not_integrated"],
        "cold_recovery_blocked_slots": result["cold_recovery_slots_blocked"],
        "full_run_executable": False,
        "aws_approval": False,
    }, sort_keys=True))


if __name__ == "__main__":
    main()
