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

The current proposed scope, four-hour window and 10 USD budget are recorded in the [approval package](edge-aws-approval-package.md). Read-only preflight was authorized and performed on 2026-11-10. Record the owner's approval from the conversation privately, complete the technical conditions in that package, and fill in the run-specific hashes and timestamps before paid provisioning.

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

For each workload, start the recorder before recording the UTC workload start; record its UTC end after the final response. Stop the recorder only after that end, retain its raw JSONL in the private evidence directory, and run `summarize --input-file <raw.jsonl> --since <workload-start-utc> --until <workload-end-utc>`. Keep its JSON output and SHA-256 hashes of both files; a nonzero recorder or summary exit, incomplete coverage, or a missing artifact is NO-GO. Do not replace a failed window with an unbounded summary.

## 5. Staged acceptance and stop-the-line

| Stage | Action | Native PASS evidence | Failure action |
|---|---|---|---|
| **D2-A / Edge C1** | `warmbench --fail-fast --concurrency=1 --read-repeats=3` using pinned synthetic `backup30` | 90/90 PUT; 90/90 HEAD; **360/360 SHA-256 GET** (initial + 3 repeats); 0 500/503/checksum errors; clock source trusted | Stop new dispatch, drain accepted requests, capture trigger and host clock sample; skip C4/D3/D4 |
| **D2-B / Edge C4** | Only after **C1 PASS**; same release/manifest/policy and `--concurrency=4` | Same 90/90 PUT, 90/90 HEAD, 360/360 GET; no unaccounted requests or unknown commit | Stop, persist/reconcile indeterminate PUTs, skip remaining gated work |
| **D3 / native R1/R2** | Isolated lease/fence/closed-writer tests; R2 export/seal/cut, checkpoint/GC; DDB capacity/PITR/throttles | Time-stamped native lease and cut proof, DDB responses and logs, expected state transitions and no cross-fixture corruption | Stop, preserve proof and teardown. DDB `ACTIVE` alone is not R1/R2 PASS |
| **D4 / 8 shallow alarms** | Cloud/Edge × recovery_cut_age, integrity, throttling, telemetry_age; derive transitions from real, safe fixture signals | 8 alarm ARNs/dimensions and complete native `OK → ALARM → OK` histories with actual metric periods/thresholds; `deep_observation=false` | Record FAIL/DEFERRED; no forced `SetAlarmState` and no false healthy-zero metrics |

Opt-in native-backend fixtures `TestAWSDevR1AdmissionClosureRenewalAndTakeover` and `TestAWSDevRecoveryFencedSealCheckpoint` use `MILES_RECOVERY_DEV_ACCEPTANCE=1`, `MILES_AWS_DEV_ACCOUNT`, `MILES_AWS_DEV_ROLE`, `AWS_REGION`, temporary credentials from the AWS provider chain, `MILES_S3_DEV_BUCKET`, and optionally `MILES_S3_DEV_KMS_KEY`. They verify account and assumed role before creating randomized isolated tables and an S3 journal prefix. A separate `TestAWSDevWarmGCObjectFlows` uses `MILES_GC_DEV_ACCEPTANCE=1` and the same account/role/region guard, creates its own versioned bucket and randomized tables, and exercises real S3 versions, R1 pins, compaction, checkpoint and GC in the test process. Each fixture has a separate table inventory; include extra tables, bucket, permissions, cleanup and cost in the plan. These tests use modeled clocks and in-process nodes; they do **not** prove production node takeover, real clock bounds or the deployed multi-node runtime. Run on the approved AWS test host, preserve output privately, verify table/bucket/version cleanup independently, and do not count skipped tests as PASS. Lost create acknowledgements require independent inventory reconciliation before cleanup.

The live GC fixture waits 15 minutes after first enabling bucket versioning before writing, following the [S3 propagation guidance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/manage-versioning-examples.html); invoke the Go test with a timeout of at least 45 minutes and include this wait plus cleanup in the approved window.

After approved live alarm exercise, `capture_alarm_evidence.py --profile <profile> --region <region> --account-id <account> --prefix <alarm-prefix> --installation <metric-dimension> --start <utc> --end <utc> --output <private-json>` captures alarm definitions, native state history and matching metric samples for all eight alarms, then validates their contracts. Preserve the printed SHA-256 and independently demonstrate that each metric came from the runtime; this reader does not generate safe breaches or prove metric provenance. The candidate fault procedure below needs an offline rehearsal and review before Gate D4 can be planned as runnable. The cut-age threshold alone requires over one hour of elapsed age; include the alarm evaluation/recovery periods and teardown buffer in the budget.

### Candidate fault exercise for the isolated installation

This is an operator procedure requiring a reviewed paid-run plan and explicit permission to enable faults. It is not yet live-validated. Set Terraform `enable_test_fault_injection=true` only for the disposable Cloud fleet; for the disposable Edge node set `enable_fault_injection=true` in its protected startup configuration before starting it. Confirm both profiles expose the node-scoped Admin fault API and that the release SHA matches the approved build. Direct each Admin call to the intended node through a pinned, verified tunnel; use a private 0600 token file and never put tokens on the command line. `dsgctl fault ls` and `dsgctl node ls` provide the exact node IDs. Use a separately provisioned synthetic gateway credential and disposable logical bucket for a random canary.

For an Edge test node without internet access, the opt-in `enable_edge_private_endpoint` adds a billable Roles Anywhere interface endpoint with private DNS and limits `CreateSession` to this installation's trust anchor. The test node still needs an approved security-group path to the endpoint, an SSM-only instance identity, its separate Roles Anywhere certificate, and a reproducible private provisioning procedure. The endpoint alone does not make the Edge/client deployment ready.

1. **Integrity / both profiles:** purge the target node cache with `dsgctl cache purge --scope node --node <node-id>` and wait for its job to finish. On that node, `dsgctl fault add --type corrupt_pack --scope node --node <node-id> --rate 1 --duration 180`; capture its returned fault ID. GET the synthetic canary directly through that node, expect an integrity failure, and verify that node's `miles_warm_read_integrity_failures_total` increments. Remove the rule with `dsgctl fault rm <fault-id>` and verify a successful GET. `corrupt_pack` changes the returned bytes in memory, not the S3 object. Repeat for Cloud and Edge.
2. **Throttling and observation age / both profiles:** on one target node at a time, install `dsgctl fault add --type ddb_throttle --scope node --node <node-id> --rate 1 --duration 180`. Issue a signed canary HEAD to exercise the SDK and expect an error; record the SDK throttle counter and continuing CloudWatch export. The observer's last good timestamp must age past 60 seconds. Let the rule expire automatically (Admin authorization itself may be throttled), verify fault list, healthy observation and counter recovery. This simulates an SDK throttle response; it is not evidence of AWS-side DynamoDB throttling. Repeat for Cloud and Edge.
3. **Recovery cut age / both profiles:** first record an actual sealed cut and its timestamp. With no workload writes, install `s3_5xx` at rate 1 on **every** Cloud and Edge node, retaining every rule ID. The opt-in wrapper rejects S3 calls before dispatch; it does not mutate S3. Monitor node identity, fault presence, observation freshness and the sealed-cut timestamp. Hold until the actual age exceeds 3600 seconds and both profile alarms breach, allowing their evaluation periods; if a node restarts, a new cut appears, the stop threshold is reached, or the teardown buffer is threatened, stop and mark the stage unverified. Remove all fault IDs, verify journal export/seal resumes and cut age returns below threshold. Use finite rule durations and account for a first cut, the >1h hold, alarm recovery and cleanup; do not assume the prior three-hour window suffices.
4. Capture the window using `capture_alarm_evidence.py` above. Integrity/throttle counters emit only nonzero deltas; recovery may occur through the alarm's configured `notBreaching` missing-data policy. Preserve continuously fresh runtime telemetry to show the exporter remained alive; never insert fake zero datapoints. Verify `OK → ALARM → OK`, correct dimensions, native history and metric timestamps for each profile, and distinguish injected SDK errors from AWS-side throttles. Disable test faults and confirm no rules remain on each surviving node before shutdown.

On any unexpected mutation, failed fault removal, unknown write outcome or insufficient cleanup time, stop new work and start Phase E; record incomplete evidence rather than asserting PASS. The procedure and exact IAM/SSM access still require replay and a reviewed resource/cost envelope before AWS.

Example benchmark arguments only (sensitive config and output directories are resolved privately and must be newly created per trial):

`warmbench --config <PRIVATE_CONFIG_0600> --manifest <PINNED_MANIFEST> --out <NEW_PRIVATE_RUN_DIR> --fail-fast --concurrency 1 --read-repeats 3 --drain-timeout 120s`

`--drain-timeout` bounds an **operator interruption**, not the initial request-failure latch. Any HTTP 500/503, mismatched checksum, clock rejection or lease failure is a hard stop for **new** work. Previously claimed requests must finish or be marked `indeterminate`; never retry or bulk-delete before durable reconciliation. Every trial records `planned = dispatched + skipped` and `completed = dispatched`, per-request errors, and run result status. Do not change production clock thresholds to force PASS.

**Actual Terraform shallow alarm contract** (`infra/terraform/cloud/modules/observability/main.tf`): metric `miles_recovery_cut_age_seconds` >3600 (missing=breaching); `miles_warm_read_integrity_failures_total` >0 (missing=notBreaching); `miles_ddb_throttles_total` >0 (missing=notBreaching); `miles_runtime_observation_age_seconds` >60 (missing=breaching). Evaluation periods=3, datapoints-to-alarm=1, period=60 seconds, dimensions `profile` + `installation`. Reserve enough time for both alarm and recovery evaluation before paid-window deadline. Use only safe isolated signals, not harmful live data tampering.

## 6. Phase E — mandatory teardown, including failed runs

**Current blocker:** live verifier requires a separately frozen Terraform state and hash, but coverage for all resource types and independently discovered runtime resources remains incomplete. The commands below describe the intended process and cannot yet establish State 0 for the full deployment. Recheck EBS and NAT while they are deleting; if either remains at the deadline, record the resource and escalate instead of claiming absence.

The offline inventory preparer can capture every managed state resource, including nested modules, without copying resource values other than IDs. In a private directory with `umask 077`, capture `terraform show -json` from active state and record its SHA-256 separately. Run `python3 scripts/freeze_terraform_inventory.py --state-json <private-state.json> --output <new-private-inventory.json> --run-id <run> --account-id <account> --region <region>` and record the printed SHA-256 separately. Both files are private; state may contain secrets. The candidate manifest generator `scripts/prepare_teardown_manifest.py` takes `--state-json`, `--state-sha256`, `--inventory-json`, `--run-id`, `--account-id`, `--region`, and `--output`, rejects unmapped resources and missing IDs, and prints the new manifest hash. It does not discover transient ASG instances/volumes or resources missing from Terraform state. The verifier takes the same frozen `--state-json` and independently recorded `--state-sha256`; state hash, manifest provenance and mappings must match. This is not sufficient for full-stack live proof until unsupported and transient resource inventory are resolved.

After read-only authorization and before provisioning, `capture_volume_baseline.py --profile <profile> --region <region> --account-id <account> --run-id <run> --output <private-baseline>` captures regional EBS IDs; record its printed SHA-256. Before destroy, `capture_runtime_inventory.py` requires the same identity and run ID, the owned `--vpc-id`, approved `--asg`/`--instance` values, and `--volume-baseline`/`--volume-baseline-sha256`. It records all instances found in that VPC, their attached disks, and every newly discovered regional volume since baseline. Review any additional volume ownership; this inventory never authorizes deleting an unrelated volume. The verifier takes `--runtime-json`, `--runtime-sha256`, and `--volume-baseline` and rechecks VPC instances and regional volume deltas after teardown, including replacements or detached volumes appearing after capture. Native discovery errors fail closed. The first audit remains `UNVERIFIED` until the temporary audit role is removed and the independent final audit succeeds.

Capture the v2 account baseline before creating the temporary audit role or attaching grants. Run `capture_account_inventory.py --profile <profile> --region <region> --account-id <account> --run-id <run> --iam-role <deploy-role-name> --output <private-account-baseline>` to record native resource IDs, the deploy role ID, trust policy hash, permissions boundary and policies, and all local managed policy versions. Bind the file and its SHA-256 with `--account-baseline` and `--account-baseline-sha256`. The verifier reports both newly discovered and missing baseline IDs as well as IAM drift. A discrepancy requires investigation. The audit identity needs IAM read access including `iam:GetRole` on audited persistent roles. For scheduled deletion, include one owner-reviewed `--approve-pending-deletion kms_key=<key-id>` or `--approve-pending-deletion secret=<secret-arn>` per approved resource. The verifier checks each ID against frozen Terraform state, the manifest and the pre-run baseline.

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

### Final independent cleanup audit

Freeze `--audit-role <temporary-read-only-role>` alongside `--deploy-role` in the pre-destroy manifest. The audit role must be absent from the pre-run account baseline and present for the first check. After workload teardown, restore deploy policies and remove the temporary role using the IAM operator. Re-run the verifier with all frozen artifacts and `--final-audit --profile <root-audit-profile>`. This read-only check requires the exact account root identity, proves `iam:GetRole` returns `NoSuchEntity` for the frozen audit role and repeats the two-way baseline comparison. Permission errors, a retained audit role, IAM drift or missing baseline IDs prevent a clean audit. Approved KMS/secret deletion schedules produce `PASS_WITH_PENDING`, with `all_absent=false`. Log out the root audit profile after exporting evidence.
