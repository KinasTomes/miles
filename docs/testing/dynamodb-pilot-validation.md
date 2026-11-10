# DynamoDB pilot validation

The pilot is prepared from the shared runtime with checkpoint/rebuild and Warm
GC/compaction merged. See [procedure and metric definitions](../../benchmarks/dynamodb-pilot.md).
This page records local proof and the incomplete live pilot described below.

## Checks

| Check | Evidence |
| --- | --- |
| `make check` | Format, vet, Go/Python tests and release/CLI/harness builds. |
| `go test -race ./internal/observability ./internal/benchmark ./cmd/warmbench` | Native signed SDK capacity/throttle parsing; preserved frozen transaction parameters, HTTP consumed bytes/partial reads/empty-body framing, fixed series inventory, streaming response behavior, stale gauge removal and retained-prefix workload. |
| Native DynamoDB Local/Moto app + metadata race suites | Three real shared compositions (two Cloud, one Edge), producer commits/multipart, recovery cut/seal, checkpoint/GC, process-owned Warm metrics and complete physical statistics. Atomic event observation survives idempotent retry and disappears from pending lag after export. |
| Native recovery race suite | Export/seal and fault scenarios against pinned loopback fixtures. |
| `terraform validate`, `terraform test` |38 mock-provider plans, including four new alarm plans: opt-in default, exact publisher dimensions/thresholds, Cloud-only and pause removal. No AWS calls. |
| `python3 -B -m unittest discover -s scripts/tests -p test_dynamodb_pilot.py -v` | Report route/ratio separation and negative cases for mismatched workload/binary/encryption, failures/unhashed GET, missing routes, restart, stale gauges, missing nodes/incomplete statistics and invalid collection windows. Synthetic test inputs are not performance evidence. |
| Native insert reconciliation regressions | Lost ACK and pre-send failure preserve the original journal outcome, retain roots, reject a delayed original transaction and allow a successor to seal the epoch. Joined writers can reconcile during shutdown. Unknown physical I/O continues to block release. |
| Native concurrent bucket-policy regression | A publication between the current-pointer read and history discovery returns `ErrConflict`. Immutable history is read before the pointer so an atomic concurrent commit cannot masquerade as inconsistent history. |

Fixtures use the pinned Docker images in `deploy/local/compose.yaml`, fresh
loopback-only ports, isolated table/bucket names and temporary local paths. Only
the owned compose project is removed, including on test failure. No persistent
runtime service or performance dataset is installed on the operator machine.

The sampled gate metric cannot provide a complete distribution of short gate
closures. Native capacity reports can omit failed/unknown consumption or a
read/write breakdown. These limits are carried into the report and runbook.
The added Outbox timestamp is telemetry beside the immutable event, not a new
recovery ordering rule; unresolved legacy events without it withhold observations.

## Incomplete live pilot

The Singapore pilot on 2026-11-05 used two private Cloud gateways, one AWS Edge
and one dedicated AWS client, all ARM64 `t4g.small` with Standard CPU credits.
The gateway release was `918ca58`; later Terraform and test-only corrections
preserved its production Go source. Runtime IAM, its permissions boundary and
the S3 gateway endpoint were corrected to allow data-bucket version inventory.

- One complete matched concurrency-1 set passed full-file GET verification and
  fresh physical statistics. Cloud and Edge PUT throughput were each about
  7.8% of direct S3 in this sample. Earlier failed trials remain part of the
  evidence. A successful retry does not establish failure-free operation.
- All 20 tables had PITR enabled. Native routing inventory showed zero NAT
  gateways, two gateway endpoints and nine interface endpoints across two AZs.
- Ten isolated probe alarms completed threshold-driven `OK` → `ALARM` → `OK`
  transitions with native alarm history. `SetAlarmState` was not used.
- Cloud concurrency-4 PUT failed twice with `ServiceUnavailable`. An earlier
  Edge concurrency-1 trial had one GET 503. The initiating causes remain open.
  A subsequent release logs a bounded, server-only 503 failure class
  (`r1_gate_busy`, `fenced`, `backend_unavailable` or `unknown`) with the response
  request ID and HTTP method, never raw backend messages, object keys or
  credentials. The Admin 503 handler similarly records its route pattern,
  request ID and fixed failure class, including cancellation and deadline.
  This makes a future recurrence easier to diagnose; it is **not** a fix or
  retrospective explanation for the old failures.
- The scheduled cut could not seal epoch zero. A failed PUT retained a closed
  writer with pending `insert/...`, which the existing commit-only reconciler
  could not settle. The subsequent slow-operation preparation received Admin
  503s on both front doors. Checkpoint/GC overhead and slow Complete/GET remain
  unverified on this release.
- Native metrics recorded the `Chunks` table reaching its 128-read-unit cap.
  The approved adjustment raised only that table's read cap to 512, retaining
  the write cap at 128. Available later samples had no further nonzero read-cap
  throttles. This does not establish the cause of the earlier PUT failures.
- Pinned ALB IP addresses became stale during the run. Measurements switched
  to the native ALB DNS name with a matching private-CA certificate SAN and
  strict TLS validation. Future trials must resolve the ALB hostname dynamically.

The pause apply completed at 04:10 UTC. The following plan had zero pending
changes and retained 83 foundation resources, including both S3 buckets, 20
tables, KMS and the runtime secret. ASG/ALB absence and Edge/client termination
were checked separately; Terraform removed the interface endpoints and alarms.
Full teardown subsequently completed at 08:54 UTC. All 4,542 retained S3 versions
were removed, both test buckets and all 20 tables were deleted, and the workload
Terraform state became empty. Independent API checks confirmed absence of the
four root disks, fleet, ALB, network/endpoints, certificate, provider and audited
session document. The runtime KMS key and secret were scheduled for deletion
with 30-day windows. Twenty automatic SYSTEM backups remain under AWS retention.
Bootstrap/state resources were preserved.

The renewed IAM grant was withdrawn after cleanup, all five managed-policy
defaults were restored, and the temporary policy versions and owned service-linked
roles were removed. Test private keys, bearer tokens and synthetic credentials
were removed from the operator files and the root CLI session was logged out.
Native plans and diagnostic metadata remain in private operator evidence.

The interrupted run was not restarted. Its only Admin token had expired, and
the operator selected evidence export followed by a fresh installation. Cleanup
finished before a replacement installation or a new VM was created.

The insert reconciliation and policy-read ordering fixes have local regression
proof. They require a new live release before they can certify the retained
history or complete the interrupted acceptance cases.

## Live pilot trial 2026-11-06 (Cloud C1 Trial 1)

Following approval for Workstream D, a live pilot trial was executed on 2026-11-06
(Window: 05:05Z - 07:35Z) in `ap-southeast-1` using `PAY_PER_REQUEST` billing mode
on all 20 DynamoDB tables, dual Cloud gateways (`cloud-a`, `cloud-b`), one Edge
gateway, and a dedicated pilot client:

- **PUT workload (100% PASS on this sample):** 90/90 `PutObject` requests succeeded
  (754,994,670 bytes, p50 latency 1,932 ms). All 90 objects were verified committed in
  `miles-dev-Objects`. However, both previous and current runs used identical `PAY_PER_REQUEST`
  capacity caps (19 tables 128/128, Chunks 512/128); this single run does not establish
  that prior PUT 500/503 root causes were resolved by capacity mode.
- **HEAD verification (100% PASS):** 90/90 `HeadObject` requests succeeded (p50: 9.2 ms).
- **GET verification (84/90 PASS, 6 FAIL):**
  - All 45 GET requests routed to `cloud-b` passed 100%.
  - On `cloud-a`, request #79 was truncated (4,901,763 / 8,388,608 bytes read), and 5
    subsequent requests (#81, #83, #85, #87, #89) returned HTTP 500 `InternalError`
    (`stage: object_get`, `failure_class: unknown`).
  - Warmbench reported `miles_warm_read_integrity_failures_total: 0`, and all 90 objects
    had committed DynamoDB heads, but separate offline download checksum artifacts for the 6
    failing GETs were not retained in the test archive.
  - **Chrony clock hypothesis:** Retained post-failure metrics showed `cloud-a` with
    `miles_ready=0` while `cloud-b` remained ready. Under `internal/coordination/chrony.go:94`,
    the clock boundary requires `age <= min(1024, 2*interval+2)` (~34s for 16s poll interval).
    A temporary NTP sample gap beyond ~34s causes `Chrony.Bounds()` to fail-closed, closing
    the read gate and rejecting lease acquisition. While consistent with the observed outage,
    raw independent chrony tracking logs with exact packet timestamps were not archived;
    Chrony failure remains an unverified hypothesis rather than an independently proven fact.
    See technical analysis in [`cloud-c1-t1-live-run-20261106-rca.md`](./cloud-c1-t1-live-run-20261106-rca.md).
- **Stop-the-line & Full D4 Teardown:**
  - Upon encountering the GET errors, further testing (C4, Edge) was immediately halted to
    preserve evidence and minimize AWS spend.
  - All 3,286 DynamoDB records across 20 tables were exported to retained evidence.
  - All 1,228 S3 versions/delete markers were purged and both buckets deleted.
  - Terraform workload and foundation resources (136 total) were completely destroyed
    (`terraform state: 0 resources`).
  - ACM certificate, audited SSM session document `miles-dev-shell`, and both service-linked
    roles (`AWSServiceRoleForRolesAnywhere`, `AWSServiceRoleForElasticLoadBalancing`) were deleted.
  - All 5 IAM managed policies were restored to baseline versions, temporary versions purged,
    and inline policy removed.
  - Native AWS absence verification (`verify-final-aws-cleanup.py`) verified 41/41 absence
    checks passed (`native-final-cleanup-proof.json`).
  - Estimated session spend remained under 1.00 USD (within the 10 USD soft budget); final
    billed cost awaits AWS invoice reporting lag.

## Diagnostic and observability updates (Workstreams A–C)

Offline analysis and negative fault-injection tests (PR #65 continuation) verified:
- Analysis of the 90 ordered PUTs from Cloud C1 identified that the 2 failures
  (#71 HTTP 503 and #73 HTTP 500) occurred at distinct pipeline stages:
  - Request #71 had a writer lease and quota reservation granted, which were
    safely transitioned to `Released` without publishing an object head or leaking quota.
  - Request #73 failed prior to writer admission during credential/authorization lookup.
- The failure of the diagnostic collection path in earlier runs was traced to
  SSM Session Manager `Standard_Stream` failure combined with unstructured slog text
  output. `app.Run` now initializes `slog.SetDefault(logger)` to ensure all runtime
  components output structured JSON lines.
- `cmd/miles/s3.go` now captures admission gate rejections with node and stage context,
  and `internal/api/s3/object.go` logs stages for `GetObject`, `HeadObject`, and
  `DeleteObject`.
- Full regression suites (`make check`, `make test-local`, `go test -race`, and Python
  runner unit tests) pass locally. Local PASS does not constitute AWS acceptance.

## Live pilot acceptance run 2026-11-08 (Follow-Up)

Following approved run plan [`docs/planning/aws-live-acceptance-run-plan.md`](../planning/aws-live-acceptance-run-plan.md) and [`docs/planning/aws-live-acceptance-follow-up.md`](../planning/aws-live-acceptance-follow-up.md), a complete staged live acceptance run was executed on 2026-11-08 (Window: 03:45Z – 05:17Z) on AWS Account `499388132734` in `ap-southeast-1`. See detailed execution report in [`live-acceptance-20261108-report.md`](./live-acceptance-20261108-report.md).

### Topology and environment
- **Release binary:** Built from source for PR #71 (`miles-linux-arm64.tar.gz`), staged via S3 release upload.
- **Compute nodes:** 2 Cloud gateways (`cloud-a`, `cloud-b`) across two Availability Zones behind a private Application Load Balancer (`miles-dev`) with dynamic DNS resolution, 1 Edge gateway bootstrapped via AWS Roles Anywhere, and 1 dedicated client instance. All nodes are `t4g.small` ARM64 (2 vCPUs, 2 GiB RAM, burstable with 20% baseline per vCPU, Standard CPU credits).
- **Storage and networking:** 20 DynamoDB tables in `PAY_PER_REQUEST` billing mode with Point-in-Time Recovery (PITR) enabled; 9 VPC interface endpoints; 2 S3 gateway endpoints; 0 NAT gateways.
- **Security & IAM:** Non-root deployer `arn:aws:sts::499388132734:assumed-role/miles-dev-deploy/hung-deploy` (profile `miles-dev`). Root (`miles-iam-bootstrap`) strictly reserved for preflight IAM activation, temporary SLR management, and final zero-resource verification.

### Phase D1 — Hard-stop gate (Cloud C1 Trial 1): PASS (100%)
- **Dataset:** `backup30` (90 objects, 754,994,670 logical bytes).
- **PUT workload:** **90/90 PUT succeeded (100% PASS, 0 failed, 0 HTTP 500, 0 HTTP 503)**. Elapsed: 158.00s, throughput: 4.56 MB/s, latency p50: 1,830.47 ms, p95: 2,089.50 ms.
- **HEAD verification:** **90/90 HEAD succeeded (100% PASS)**, latency p50: 19.59 ms, p95: 24.19 ms.
- **GET full-file SHA-256 verification (4 repeats):**
  - Verify-GET: 90/90 PASS (13.96 MB/s, p50: 547.33 ms).
  - Repeat-GET 1: 90/90 PASS (13.52 MB/s, p50: 593.41 ms).
  - Repeat-GET 2: 90/90 PASS (14.40 MB/s, p50: 538.73 ms).
  - Repeat-GET 3: 90/90 PASS (14.91 MB/s, p50: 542.56 ms).
  - **Total GET verifications:** **360/360 full-file SHA-256 PASS (0 HTTP 500, 0 HTTP 503, 0 truncation, 0 checksum mismatch)**.
- **Physical statistics (D1):**
  - Logical bytes: 754,994,670 B; Stored bytes: 292,817,237 B.
  - **Dedup ratio: 1.94x**, **Compression ratio: 1.50x**, **Total reduction: 2.58x**.
  - Physical allocation complete (`cloud-c1-t1-window.json`).
- **Gate D1 Result:** Zero clock rejection errors, zero truncation, and zero HTTP 500/503 errors. The clock boundary issue observed on Cloud nodes in previous runs did not recur when chrony NTP synchronization was actively tracked. Hard-stop Gate D1 was cleanly passed.

### Phase D2 — Comparative runs

#### 1. Cloud C4 Trial 1: PASS (100%)
- **Concurrency:** 4 workers on Cloud nodes through ALB.
- **PUT workload:** 90/90 PUT succeeded (100% PASS, 0 failures, 0 HTTP 500/503). Elapsed: 79.97s, throughput: 9.00 MB/s, latency p50: 3,255.78 ms, p95: 5,568.51 ms.
- **HEAD verification:** 90/90 HEAD succeeded (100% PASS), latency p50: 21.61 ms, p95: 89.30 ms.
- **GET verification (4 repeats):** **360/360 full-file SHA-256 PASS**. Throughput: 26.28 – 27.91 MB/s, latency p50: 1,033.70 – 1,121.33 ms (`cloud-c4-t1-result.json`).

#### 2. Direct S3 Baselines (C1 & C4): PASS (100%)
- **Direct C1:** PUT throughput: 78.22 MB/s (p50: 101.38 ms); GET throughput: 86.83 – 90.86 MB/s (p50: 85.37 – 86.12 ms).
- **Direct C4:** PUT throughput: 298.97 MB/s (p50: 101.65 ms); GET throughput: 361.97 – 365.88 MB/s (p50: 85.56 – 86.11 ms).

#### 3. Edge C1 Trial 1: Stop-the-Line Triggered (Trial/Phase Boundary)
- **Workload:** Concurrency=1 on Edge node via AWS Roles Anywhere.
- **PUT workload:** Objects #1 – #46 succeeded (100% PASS). Objects #47 – #90 failed with HTTP 503 `clock_uncertainty_exceeded`.
- **Stop-the-Line Behavior:** In `internal/benchmark/run.go`, `filePhase` dispatches all 90 items in the configured phase. When the `put` phase completed with 44 errors, `finishPhase` returned an error, causing the orchestrator to halt at the phase/trial boundary, immediately cancelling subsequent phases (HEAD, GET) and subsequent trials (Edge C4, D4 Alarms).
- **Automated Evidence Capture:** The Phase A diagnostic runner (`scripts/runner_diagnostics.py` and `scripts/runner_orchestrator.py`) captured **100% (44/44) failures** with correlated request IDs, precise timestamps, and node status in `edge-c1-t1-failed-window.json` and raw trace files before halting.
- **Root Cause Analysis (Edge):** The observed failure mechanism was `clock_uncertainty_exceeded` (HTTP 503) beginning at request #47, where `internal/coordination/chrony.go:94` enforces `age <= min(1024, 2*interval + 2)`, causing `Chrony.Bounds()` to fail-closed and reject writer admission with `ErrClockUncertain`. While concurrent network/disk I/O and CPU scheduling contention on the burstable `t4g.small` instance (2 vCPUs, 2 GiB RAM, 20% baseline per vCPU) is the primary working hypothesis for why the NTP sample interval momentarily fell behind, verifying this causation requires correlation against detailed CPU utilization, CloudWatch CPU credit metrics, and raw chrony tracking timestamps. Crucially, unlike previous runs where diagnostics were lost, 100% (44/44) of telemetry, request IDs, and node state were captured and verified in `edge-c1-t1-failed-window.json`.

### Phase D3 — DynamoDB & recovery observations
- **DynamoDB tables & PITR (PASS 100%):** All 20 tables verified ACTIVE with `pitr=ENABLED`.
- **Administrative physical statistics snapshot (PASS 100%):**
  - Logical bytes: 1,895,875,283 B; Stored bytes: 692,545,991 B; Unique stored: 654,355,195 B.
  - Objects: 226, Chunks: 660, Warm packs: 155.
  - **Overall Dedup ratio: 1.92x**, **Compression ratio: 1.51x**, **Total reduction: 2.74x** (`final-stats.json`).
- **R1 Lease/Fence, R2 Outbox/Cut & Checkpoint/GC (UNVERIFIED / DEFERRED):** Live recovery cut and slow-operation/GC measurements were deferred to a subsequent run due to the stop-the-line halt at Edge C1.

### Phase E — Teardown & final AWS zero-resource verification
- **Compute and endpoints pause:** 53 resources destroyed via `pause-reconciled.tfplan` (EC2 client/edge instances, ASG gateway fleet, ALB, CloudWatch metric alarms, and VPC interface endpoints).
- **Data buckets purged:** 3,082 objects deleted from `miles-dev-data-499388132734-ap-southeast-1` and 180 objects from `miles-dev-pilot-direct-499388132734-ap-southeast-1` (`empty-owned-buckets.py`).
- **Remaining workload destroyed:** All 20 DynamoDB tables, VPC, subnets, route tables, KMS key, and Roles Anywhere trust anchor destroyed via `destroy-final.tfplan`.
- **Terraform state:** 0 managed resources remaining (`cloud/environments/dev`).
- **External artifacts deleted:**
  - ACM certificate `665f49f1-2750-44e7-bc9b-c9fb9e0f3721` deleted.
  - SSM session document `miles-dev-shell` deleted.
  - Secrets Manager secret `miles-dev/runtime-secrets-pilot-20261108-0345-CG930e` scheduled for deletion (`2026-11-08T05:06:35Z`).
  - KMS key `5ad99779-8ca4-4d79-b79c-2f913e673e48` scheduled for deletion (`2026-12-08T05:05:04Z`).
- **Service-Linked Roles cleaned up:** Both `AWSServiceRoleForRolesAnywhere` and `AWSServiceRoleForElasticLoadBalancing` deleted via root `miles-iam-bootstrap` and verified `SUCCEEDED` (`iam/slr-deletion-status.json`).
- **IAM restoration to baseline:** All 5 managed policies reverted to `old_default` versions, temporary versions deleted, and inline role policy `MilesPilotAcceptance` removed from role `miles-dev-deploy` (`iam/final-restoration-proof.json`).
- **Native AWS zero-state audit:** `verify-final-aws-cleanup.py` executed via root `miles-iam-bootstrap`:
  - **41/41 absence checks PASSED (100% verified zero resources)**.
  - 0 EC2 instances, 0 ASG, 0 ALB, 0 DynamoDB tables, 0 interface endpoints, 0 EBS volumes, 0 ACM certificates, 0 SSM documents, 0 Roles Anywhere trust anchors.
  - Only bootstrap state bucket `miles-dev-tfstate-499388132734-ap-southeast-1` retained (`native-final-cleanup-proof.json`).
  - *Note on retention:* The 41/41 check confirms 100% absence of all active synthetic run resources within audit scope, noting that the KMS key and Secrets Manager secret remain in their required AWS standard 30-day pending-deletion retention windows.
- **Budget and spend:** Soft budget cap was $10.00 USD; total estimated AWS spend for the live acceptance session remained under $0.50 USD (operational estimate subject to AWS invoice reporting latency).

### Live Acceptance Scope & Handoff Summary

| Phase / Trial | Acceptance Status | Notes |
| :--- | :---: | :--- |
| **Phase C (Preflight & Preparation)** | **PASS** | Scoped IAM, PKI, 20 DDB tables, endpoints, ALB, ASG. |
| **Cloud C1 (Trial 1 — Gate D1)** | **PASS** | 90/90 PUT, 90/90 HEAD, 360/360 GET hash match, 0 HTTP 500/503. Dedup 1.94x, Reduction 2.58x. |
| **Cloud C4 (Trial 1)** | **PASS** | 90/90 PUT, 90/90 HEAD, 360/360 GET hash match, 0 HTTP 500/503. |
| **Direct S3 (C1 & C4 Baselines)** | **PASS** | C1 ~78–91 MB/s, C4 ~299–366 MB/s. |
| **Edge C1 (Trial 1)** | **FAIL** | 46 PASS / 44 FAIL (`clock_uncertainty_exceeded`). Stop-the-line invoked at phase boundary; 44/44 failures captured with full telemetry. |
| **Edge C4 (Trial 1)** | **NOT RUN** | Halted per stop-the-line policy due to Edge C1 failure. |
| **D3: DDB / PITR / Stats** | **PASS** | 20 tables verified with PITR; Final stats snapshot captured (Dedup 1.92x, Reduction 2.74x). |
| **D3: R1/R2 Recovery & GC** | **UNVERIFIED / DEFERRED** | Live recovery cut and GC deferred due to trial-boundary halt after Edge C1. |
| **Phase D4 (CloudWatch Alarms)** | **DEFERRED** | Deferred to dedicated alarm follow-up run after Edge investigation. |
| **Phase E (Teardown & Cleanup)** | **PASS** | State = 0, 41/41 absence verified, SLRs deleted, IAM restored. Spend < $0.50 USD. |

**Follow-Up Partitioning:** Because Cloud C1 and Cloud C4 passed with 100% data integrity and zero regressions, subsequent investigation and fixes for Edge clock stability, D3 native recovery cut/GC measurements, D4 threshold-driven alarms, and optional runner per-request fail-fast cancellation are partitioned into a dedicated follow-up task. Repeating Cloud C1/C4 is not required unless future modifications affect the Cloud code path.

## AWS acceptance remaining

- Build hashes and configuration on two Cloud nodes plus one AWS Edge, with the
  same dedicated client/manifest/toolchain/encryption for direct S3 and gateways.
- Matched Warm trials including full GET hashes, post-workload physical statistics,
  per-node counter windows and explicitly separated HTTP/network routes.
- Native DDB capacity/throttling/transaction metrics, PITR status/storage observations,
  actual recovery cut/export observations and controlled GC/checkpoint overhead.
- CloudWatch ingestion and threshold-driven ALARM→OK transitions for cut age,
  integrity, throttling and stuck Completing, with telemetry freshness verified.
- Complete endpoint/NAT/ALB/EC2/EBS/S3/DDB/PITR/backups/KMS/secret/metrics/alarms
  inventory, dated regional prices, cost window and verified synthetic cleanup.

Alarms remain opt-in. Evidence belongs to its exact release and observation
window. Local success and a merged PR alone do not complete acceptance.
