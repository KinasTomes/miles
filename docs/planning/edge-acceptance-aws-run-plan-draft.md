# Miles — Edge / recovery / alarms AWS live acceptance run plan

**Status:** DRAFT / DO NOT EXECUTE / NOT OWNER-APPROVED
**Prepared:** 2026-11-10, after merged PR #74 (Gate F, run harness), PR #75 (Gate R, offline evidence), and PR #77 (Gate D0/E diagnostic sampling and teardown verifier).
**Related:** [Owner approval package](edge-aws-approval-package.md), [follow-up plan](edge-acceptance-followup-plan.md), [offline RCA](../testing/edge-clock-rca-followup.md), [previous live report](../testing/live-acceptance-20261108-report.md).
**Candidate code baseline:** PR #76 incorporates current `main` and contains verifier/telemetry fixes awaiting independent review. Select and pin the final reviewed release **at Gate C**.

## 1. Scope and explicitly excluded claims

- **Goal 1 / Edge stability:** distinguish the real cause of `clock_uncertainty_exceeded` at PUT #47, then prove Edge C1 and only then Edge C4 complete without hidden retry, weakened fail-closed policy, or unknown commits.
- **Goal 2 / native recovery:** prove R1 lease/fencing/closed-writer and R2 outbox export/seal/cut, checkpoint/GC using native AWS and application evidence; DDB ACTIVE/PITR alone does not prove recovery.
- **Goal 3 / alarms:** prove 8 shallow CloudWatch alarms traverse `OK → ALARM → OK` from real, isolated signals and thresholds.
- **Goal 4 / teardown:** verify owned resources absent, disclose pending deletion and reconcile estimated, observed and eventually billed costs.
- Earlier Cloud C1/C4 and direct-S3 PASS may be referenced with scope and date, but are **not** a fresh run result. Edge C1 had 46/90 PUT PASS and 44/90 503 at the 2026-11-08 session. Gate R confirmed 44/44 gateway correlations; **missed Chrony polls versus local chronyc command timeout remain hypotheses**.
- This document authorizes **zero** AWS operations. Do not reuse previous approvals, release artifact, Terraform saved plans, profile sessions or credentials. No root account. No change to `internal/coordination/chrony.go`, `minpoll 4 maxpoll 4`, or fail-closed R1 policy.

## 2. Owner decisions / immutable run envelope

All `TBD` items must be filled in **private, versioned signed approval** before any AWS API call, including read-only inventory. A separate owner go/no-go is required before any paid provisioning.

| Field | Prepared candidate / condition | Approval state |
|---|---|---|
| Run ID, responsible operator, approving owner, UTC approval timestamp | Fresh ID; record named people privately | **TBD** |
| Account, Region | Account `499388132734`; `ap-southeast-1` (Singapore), confirm after read-only authorization | **TBD confirmation** |
| Target role + identity/expiry | Scoped non-root `miles-dev-deploy` session, MFA/role policy and required trust verified; no sharing access keys | **TBD** |
| Exact release source + architecture | Select reviewed source; build/test **linux/arm64** release; persist binary SHA-256 and source tree provenance | **TBD / not pinned** |
| Manifest and dataset | Proposed `backup30`: 90 × approximately 8 MiB; regenerate/verify and pin actual manifest SHA-256; previous hash is historical only | **TBD** |
| Infrastructure ownership | Planned <=4 EC2 `t4g.small` (2 Cloud, 1 Edge, 1 client); EBS encrypted; 0 NAT; private SSM; DDB/ALB/endpoints/alarms only as reviewed plans | **TBD quotas/plans** |
| Ingress and data | Explicit operator `/32`, no public SSH/metrics/admin, synthetic fixtures only; network access paths reviewed | **TBD** |
| Allowed UTC window and responsible teardown operator | Start, latest trial start, stop-new-work, absolute teardown deadline and fallback contact; must fit all 8 alarm transitions | **TBD** |
| Cost envelope and triggers | Fresh estimate by resource × hours, S3 ops/storage, DDB, network, Logs/metrics, KMS and possible pending costs. Proposed soft/hard triggers require owner values; billing metrics are delayed and **not** a hard cap | **TBD** |
| State ownership, exclusions | Owned Terraform state and resource tags; bootstrap/state bucket exceptions documented; KMS/Secrets pending deletion explicitly handled | **TBD** |
| Run mode | `diagnostic-only` if sub-reason instrumentation is unproven, or `full-acceptance` if Gate C and D0 complete | **TBD** |

**Hard NO-GO:** absent owner signature, uncertain caller/Region, unreviewed plan, missing arm64 hashes, missing clock-source verification, inadequate teardown capacity, unavailable fresh inventory, missing evidence capture or inability to finish before the approved deadline.

## 3. Offline preparation and Gate C (requires separately authorized read-only AWS preflight)

**Before any AWS API:** review PR #74/#75 merged SHAs, rerun `make check` / `make test-local` / Go race for benchmark, verify `git diff --check`, inspect IaC and billable resource inventory locally. Freeze evidence schema and hashes. Prepare **but do not use** the read-only preflight checklist.

**After owner explicitly authorizes read-only Gate C only:**

1. Confirm role/account/Region with scoped `sts:GetCallerIdentity` and record sanitized identity / MFA session expiry. Verify effective service permissions and any SCP/boundary, not just IAM simulator; prevent accidental use of root or Sydney Glacier profile.
2. Take a **fresh** service-native baseline of owned resources, bootstrap exceptions and pending deletions. The 41/41 absence result from 2026-11-08 is historical and cannot substitute.
3. Validate refreshed Terraform root/module configuration and generate **new saved plans** from approved variables, including a review of IAM, KMS, Secrets, endpoints, DDB, logging, all 8 alarms and teardown; no blind `apply`. Verify that plan outputs do not leak secrets.
4. Check EC2 limits, disk, subnet IPs, profile capacity, client network, chrony Amazon Time Sync source and DNS dependency, S3 bucket availability and diagnostics access. Calculate costs within owner envelope, reserve teardown time and explicitly approve final plans/hashes.
5. Only after second recorded owner authorization to spend may D0 provisioning begin.

**Gate C evidence:** `run-id`, auth scope & expiry, repository/release/dataset hashes, saved-plan hashes, reviewed resource counts/tags, preflight decision, private owner approvals, UTC spend/stop/deadline. No unapproved ad hoc plan substitutions.

## 4. Host evidence contract / Gate D0 instrumentation

The host diagnostic collector is implemented in `scripts/edge_host_telemetry.py` (PR #77) and verified offline with 6 unit tests covering Chrony allowlist commands, monotonic execution timing, `/proc/stat` CPU steal deltas, PSI pressure, and bounded 0600 ring-buffer compaction. The runtime sampling contract is:

| Host stream | Sampling / fields | Provenance and guardrails |
|---|---|---|
| Chrony | Every ~2 s on Edge: `chronyc -c tracking`, `sources`, `sourcestats`, source reach/poll, last reference, sample age, stratum/leap, offset/drift/dispersion | Sample `host_observed_utc` **per command**, monotonic start/end and execution duration, success/return code/timeout. No global frozen T0; no SSH/SSM RTT conflated with host execution |
| CPU/IO scheduling | Every ~1 s: `/proc/stat` CPU user/system/steal deltas, `/proc/loadavg`, `/proc/pressure/{cpu,io}`, memory and disk pressure | Measure on host before/during/after load; report missing counters explicitly |
| AWS credit context | `CPUCreditBalance` and `CPUCreditUsage` with actual CloudWatch dimensions, periods and timestamps when approved/available | Low-resolution CloudWatch credit measurements cannot establish a per-request scheduling cause |
| Gateway/client | Stable **private** join of client request ID, gateway journal UTC + stage (`prepare`/`writer_admission`), HTTP status, failure class, benchmark `planned/dispatched/completed/skipped` and `indeterminate` | Export only pseudonyms and redacted aggregate evidence to Git; raw join remains operator-private (0600) |
| Post-run reference | Sample source health before, during, after and after recovery; include expected versus measured cadence / dropped samples | Avoid using post-workload healthy Chrony as proof of status at error onset |

Collector is read-only to Chrony and kernel metrics; no repeated `makestep`, forced sync, synthetic clock adjustments or altering policy thresholds. Keep a bounded private ring buffer (design target ≤10 MiB in `/run/miles/`, permissions 0600), verify overhead and dropped-sample behavior offline. Preserve a private copy on failure **before** teardown, sanitize before any Git publication. Verify fields against the actual host Chrony version and timestamp contract, not just a schema mock.

The telemetry summary contract (`summarize_window`) strictly distinguishes:
- **(a) Unknown/unobserved interval before acquisition**: When a requested `--since` precedes recorder start or first retained sample by more than the expected startup lag, the verdict is `UNVERIFIED` with reason `window_requires_unobserved_pre_recording_interval` (never falsely claiming eviction when zero records were dropped).
- **(b) Expected cadence / startup grace**: When `--since` falls within normal startup grace (default 2.0 s, bounding the initial sampling tick) of the initial sample (sequence 1, zero drops), coverage is verified intact with status `OK` and `startup_grace_applied_seconds` recorded.
- **(c) Provable eviction**: When sequence gaps exist within the queried window or earliest retained sequence > 1 / buffer dropped records > 0, the verdict is `UNVERIFIED` with reason `window_completeness_broken_by_record_eviction` or `window_completeness_broken_by_internal_sequence_gap`.

**D0 GO:** approved setup, release and chrony host provenance, host recorder running with correct durations and timestamps, 2 Cloud + 1 Edge readiness where provisioned, private ALB, PITR, isolated synthetic test fixtures, metrics and private evidence paths all verified. Diagnostic collector unable to capture reliable host timing → **do not** treat Edge PASS as conclusive RCA.

## 5. Staged acceptance and stop-the-line

| Stage | Action | Native PASS evidence | Failure action |
|---|---|---|---|
| **D2-A / Edge C1** | `warmbench --fail-fast --concurrency=1 --read-repeats=3` using pinned synthetic `backup30` | 90/90 PUT; 90/90 HEAD; **360/360 SHA-256 GET** (initial + 3 repeats); 0 500/503/checksum errors; clock source trusted | Stop new dispatch, drain accepted requests, capture trigger and host clock sample; skip C4/D3/D4 |
| **D2-B / Edge C4** | Only after **C1 PASS**; same release/manifest/policy and `--concurrency=4` | Same 90/90 PUT, 90/90 HEAD, 360/360 GET; no unaccounted requests or unknown commit | Stop, persist/reconcile indeterminate PUTs, skip remaining gated work |
| **D3 / native R1/R2** | Isolated lease/fence/closed-writer tests; R2 export/seal/cut, checkpoint/GC; DDB capacity/PITR/throttles | Time-stamped native lease and cut proof, DDB responses and logs, expected state transitions and no cross-fixture corruption | Stop, preserve proof and teardown. DDB `ACTIVE` alone is not R1/R2 PASS |
| **D4 / 8 shallow alarms** | Cloud/Edge × recovery_cut_age, integrity, throttling, telemetry_age; derive transitions from real, safe fixture signals | 8 alarm ARNs/dimensions and complete native `OK → ALARM → OK` histories with actual metric periods/thresholds; `deep_observation=false` | Record FAIL/DEFERRED; no forced `SetAlarmState` and no false healthy-zero metrics |

Example benchmark arguments only (sensitive config and output directories are resolved privately and must be newly created per trial):

`warmbench --config <PRIVATE_CONFIG_0600> --manifest <PINNED_MANIFEST> --out <NEW_PRIVATE_RUN_DIR> --fail-fast --concurrency 1 --read-repeats 3 --drain-timeout 120s`

`--drain-timeout` bounds an **operator interruption**, not the initial request-failure latch. Any HTTP 500/503, mismatched checksum, clock rejection or lease failure is a hard stop for **new** work. Previously claimed requests must finish or be marked `indeterminate`; never retry or bulk-delete before durable reconciliation. Every trial records `planned = dispatched + skipped` and `completed = dispatched`, per-request errors, and run result status. Do not change production clock thresholds to force PASS.

**Actual Terraform shallow alarm contract** (`infra/terraform/cloud/modules/observability/main.tf`): metric `miles_recovery_cut_age_seconds` >3600 (missing=breaching); `miles_warm_read_integrity_failures_total` >0 (missing=notBreaching); `miles_ddb_throttles_total` >0 (missing=notBreaching); `miles_runtime_observation_age_seconds` >60 (missing=breaching). Evaluation periods=3, datapoints-to-alarm=1, period=60 seconds, dimensions `profile` + `installation`. Reserve enough time for both alarm and recovery evaluation before paid-window deadline. Use only safe isolated signals, not harmful live data tampering.

## 6. Phase E — mandatory teardown, including failed runs

1. Stop new workload; resolve/drain in-flight request outcomes within approved limits; freeze private logs, telemetry, S3/version and DDB state and sanitized per-gate evidence. If an outcome is indeterminate, keep it explicitly tracked for native reconciliation.
2. **Freeze pre-teardown authoritative manifest**: Prior to `terraform destroy`, capture and seal the pre-teardown authoritative manifest from active Terraform state/outputs (`--manifest pre-teardown-manifest.json`). This manifest is strictly mandatory in live acceptance mode (`mode == "live"`).
   - **Schema (`miles-teardown-manifest/v1`)**:
     - `schema_version`: `"miles-teardown-manifest/v1"`
     - `run_id`: non-empty string matching the Gate C session identifier
     - `account_id`: 12-digit AWS account ID
     - `region`: AWS region (e.g. `ap-southeast-1`)
     - `ownership_scope`: `{"project": "miles", "environment": "dev", ...}`
     - `expected_types`: array of resource type keys (`["dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms", "vpc_id", ...]`)
     - `expected_counts`: object with exact non-negative integer count per expected resource type
     - `expected_resources`: object with exact resource IDs, ARNs, and names
     - `provenance`: `{"source": "terraform_pre_destroy", "created_at_utc": "...", "sha256": "..."}`
   - **Generation & Freezing**:
     - Extract pre-destroy resources from active Terraform outputs (`terraform output -json`) and state.
     - Calculate SHA-256 (`sha256sum pre-teardown-manifest.json > pre-teardown-manifest.json.sha256`) and seal into the operator evidence vault before executing `terraform destroy`.
   - **Validation Contract**:
     - In live mode, verifier validates STS caller identity (`aws sts get-caller-identity`) against manifest `account_id` before querying native APIs.
     - Unknown, malformed, or missing required schema fields fail closed with `UNVERIFIED`.
     - Two-way reconciliation enforces that all manifest resources exist in inventory and all inventory resources exist in manifest (partial allowlists or concealed provisioned resources fail closed).
     - Essential Miles stack resource types (`dynamodb_tables`, `s3_buckets`, `iam_roles`, `cloudwatch_alarms`, `vpc_id`) are strictly required.
     - Supported resource types whitelist enforcement (`SUPPORTED_RESOURCE_TYPES`: 24 resource types, including `nat_gateways` and `elastic_ips`); any unsupported resource type in manifest or inventory fails closed with `UNVERIFIED`.
     - Resource ID uniqueness enforcement: duplicate resource IDs in manifest (`expected_resources`) or `inventory` are strictly prohibited; count reconciliation checks unique non-empty resource count against `expected_counts` (`manifest_duplicate_resource_ids`, `manifest_count_mismatch`).
     - Strict NAT Gateway & Elastic IP cardinality reconciliation: declared `nat_count` in network inventory or manifest `ownership_scope` is reconciled against extracted `nat_gateway_ids` and `elastic_ip_allocation_ids` per the Terraform network contract (`modules/network/main.tf`: 1:1 NAT and dedicated EIP per zone). Any missing, partial, or mismatched cardinality (e.g. `nat_count=2` with only 1 NAT ID or EIP ID) fails closed with `UNVERIFIED` before absence checks and in `verify_all()` guards (`NAT_EIP_CARDINALITY_MISMATCH`), preventing false State 0 confirmations.
     - Unchecked declared resource guard: any declared resource in manifest or inventory not actively queried by a native checker triggers fail-closed `UNCHECKED_RESOURCES_DETECTED`.
3. Destroy **only owned resources** per reviewed Terraform destroy plan and verified resource tags; include S3 versions/markers/multipart, ALB/ASG/EC2, DDB, endpoints, networking (including NAT Gateways and Elastic IPs), IAM/SLRs as created, KMS/Secrets deletion schedules. Preserve documented bootstrap/state resources. Never delete shared Hung/Phong IAM roles/policies without explicit ownership.
4. Perform **new service-native final absence checks** matching the owned resource inventory using `python3 scripts/verify_teardown_absence.py --outputs-json active-outputs.json --manifest pre-teardown-manifest.json --output-ledger gate-e-absence-summary.json` (PR #77 + PR #76 hotfix). The verifier cross-checks the 24-resource inventory (including NAT Gateways via `describe-nat-gateways` and Elastic IPs via `describe-addresses`) against the frozen manifest and native AWS APIs. In live mode, omitting `--manifest` fails closed with exit code 1 / `UNVERIFIED`.
5. Record `PASS / FAIL / DEFERRED / UNVERIFIED` by gate, actual runtime state (including pending deletions), resource counts and Terraform drift, cost estimate at shutdown, later observed/billed reconciliations and owner acknowledgment.

**Escalation:** teardown incomplete, unknown billing, missing native proofs or absent final verifier → label **OPEN / ATTENTION REQUIRED**, notify owner through the approved escalation path; do not claim State 0 merely because Terraform returned 0.

## 7. Next decision

This draft is **ready for operator/owner review, not execution**. The associated [approval package](edge-aws-approval-package.md) captures the remaining release hashes, IAM, fresh baseline, budget/deadline, diagnostic implementation gate and signature. Neither a green CI nor an earlier signed approval authorizes this future live run.
