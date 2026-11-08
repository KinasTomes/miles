# AWS Live Acceptance Run Report (2026-11-08 Follow-Up)

- **Date / Window:** 2026-11-08 03:45 UTC to 05:17 UTC (Scope ID: `w05-hung03-live-20261108-0345`)
- **AWS Region / Account:** `ap-southeast-1` / `499388132734`
- **Session Duration:** ~1.5 hours (well within 3.0-hour maximum window)
- **Estimated Spend:** < $0.50 USD (soft cap: $10.00 USD; subject to standard AWS billing invoice lag)
- **Status:** **PHASE C & E COMPLETED (100% PASS, STATE 0 VERIFIED). PHASE D PARTIAL (CLOUD C1/C4 PASS; EDGE C1 FAIL WITH 44/44 CORRELATED EVIDENCE CAPTURED AT PHASE BOUNDARY; EDGE C4 NOT RUN; D3 R1/R2 RECOVERY & D4 ALARMS DEFERRED).**

---

## 1. Executive Summary

Following approved run plan [`docs/planning/aws-live-acceptance-run-plan.md`](../planning/aws-live-acceptance-run-plan.md) and [`docs/planning/aws-live-acceptance-follow-up.md`](../planning/aws-live-acceptance-follow-up.md), an end-to-end staged live acceptance run was executed on AWS Account `499388132734` in `ap-southeast-1`.

### Key Outcomes
1. **Preflight & Staging (Phase C & D0): PASS (100%)**
   - Non-root IAM deployer role `miles-dev-deploy` activated with scoped permissions.
   - Pinned arm64 release binary uploaded to S3.
   - 20 DynamoDB tables provisioned in `PAY_PER_REQUEST` billing mode with Point-in-Time Recovery (PITR) enabled.
   - Dual Cloud nodes deployed across 2 Availability Zones behind a private Application Load Balancer (`miles-dev`) with dynamic DNS resolution.
   - 1 Edge node deployed via AWS Roles Anywhere and 1 dedicated pilot client instance. All nodes were `t4g.small` ARM64 (2 vCPUs, 2 GiB RAM, burstable 20% baseline/vCPU, Standard CPU credits).
2. **Hard-Stop Gate D1 (Cloud C1 Trial 1): PASS (100%)**
   - **90/90 PUT succeeded (100% PASS)** without a single HTTP 500 or 503 error (elapsed: 158.00s, 4.56 MB/s, p50: 1,830.47 ms).
   - **90/90 HEAD succeeded (100% PASS)** (p50: 19.59 ms).
   - **360/360 GET full-file SHA-256 verifications passed (100% PASS)** across 4 consecutive read passes (1 verify-get + 3 repeat-get). Zero truncation, zero checksum mismatch, zero 500/503 errors.
   - **Dedup ratio: 1.94x**, **Compression ratio: 1.50x**, **Total reduction: 2.58x**.
   - *Verdict:* The clock uncertainty rejection bug observed on Cloud nodes during prior runs did NOT recur under live multi-AZ traffic through the ALB when Chrony bounds were synchronized. Gate D1 was cleanly passed.
3. **Comparative Runs (Phase D2):**
   - **Cloud C4 Trial 1: PASS (100%)** (90 PUT, 90 HEAD, 360 GET full-file SHA-256 match, 0 errors, 26.3 – 27.9 MB/s GET).
   - **Direct S3 Baselines: PASS (100%)** (C1: ~78–91 MB/s; C4: ~299–366 MB/s).
   - **Edge C1 Trial 1: STOP-THE-LINE TRIGGERED (Trial/Phase Boundary)**
     - Objects #1 – #46 succeeded (100% PASS).
     - Objects #47 – #90 failed with HTTP 503 `clock_uncertainty_exceeded`.
     - *Trial/Phase-Boundary Stop-the-Line:* In `internal/benchmark/run.go`, `filePhase` dispatches all 90 items in the configured phase before `finishPhase` evaluates phase status. Once the `put` phase completed with 44 errors, the runner halted at the phase/trial boundary, immediately cancelling subsequent phases (HEAD, GET) and subsequent trials (Edge C4, D4 Alarms).
     - *Automated Diagnostic Verification:* The Phase A diagnostic runner (`scripts/runner_diagnostics.py` and `scripts/runner_orchestrator.py`) automatically captured **100% (44/44) failures** with correlated request IDs, timestamps, and node status in `edge-c1-t1-failed-window.json` before halting.
4. **Why Edge C4 and D4 Alarms Were Not Run:**
   - Under the approved **Stop-the-Line Policy**, when a trial encounters failures, the runner must halt immediately between trials to prevent running stress workloads on an unstable node, conserve the $10.00 soft budget cap, and preserve forensic evidence.
   - Edge C4 was not executed because Edge C1 failed.
   - Phase D4 (CloudWatch Alarms) was deferred to a dedicated follow-up run after Edge clock stability is resolved.
5. **DynamoDB Verification & Recovery Observations (Phase D3):**
   - **DDB Tables, PITR & Physical Stats: PASS (100%)**
     - All 20 tables verified ACTIVE with `pitr=ENABLED`.
     - Admin statistics snapshot captured: 226 objects, 660 chunks, 155 Warm packs, 1.90 GB logical, 692.5 MB stored. Overall dedup: **1.92x**, total reduction: **2.74x**.
   - **R1 Lease/Fence, R2 Outbox/Cut, & Checkpoint/GC: UNVERIFIED / DEFERRED**
     - Due to the stop-the-line halt at Edge C1, live recovery cut and slow-operation/GC measurements were deferred to a subsequent run.
6. **Teardown & Zero-Resource Verification (Phase E): PASS (100%)**
   - All 53 compute, ASG, ALB, CloudWatch alarms, and interface endpoints destroyed via `pause-reconciled.tfplan`.
   - Both S3 test buckets completely emptied (3,262 objects deleted) and removed.
   - All 20 DynamoDB tables, VPC, subnets, route tables, KMS key, and Roles Anywhere trust anchor destroyed.
   - Terraform workload state: **0 managed resources remaining**.
   - External artifacts destroyed: ACM certificate, SSM document `miles-dev-shell`, and Secrets Manager secret scheduled for deletion.
   - Both Service-Linked Roles (`AWSServiceRoleForRolesAnywhere`, `AWSServiceRoleForElasticLoadBalancing`) deleted and verified `SUCCEEDED`.
   - All 5 IAM managed policies restored to baseline (`v1`/`v2`/`v3`), temporary versions deleted, and inline policy removed.
   - `verify-final-aws-cleanup.py`: **41/41 absence checks PASSED**. Zero active synthetic resources remaining on AWS.

---

## 2. Trial Execution Breakdown

| Phase / Trial | Target | Concurrency | Outcome | Key Metrics |
|---|---|---|---|---|
| **D1: Cloud C1 T1** | `miles-warm` (Cloud) | 1 | **PASS (100%)** | 90 PUT (4.56 MB/s), 90 HEAD (p50: 19.6 ms), 360 GET hash verified (14.2 MB/s, 0 500/503). Dedup: 1.94x, Reduction: 2.58x. |
| **D2: Cloud C4 T1** | `miles-warm` (Cloud) | 4 | **PASS (100%)** | 90 PUT (9.00 MB/s), 90 HEAD (p50: 21.6 ms), 360 GET hash verified (27.1 MB/s, 0 500/503). |
| **D2: Direct S3 C1** | Direct S3 | 1 | **PASS (100%)** | PUT 78.22 MB/s, GET 86.83 – 90.86 MB/s (p50: 85.9 ms). |
| **D2: Direct S3 C4** | Direct S3 | 4 | **PASS (100%)** | PUT 298.97 MB/s, GET 361.97 – 365.88 MB/s (p50: 85.8 ms). |
| **D2: Edge C1 T1** | `miles-warm` (Edge) | 1 | **STOP-THE-LINE** | PUT #1–#46 PASS (4.53 MB/s); PUT #47–#90 FAIL (HTTP 503 `clock_uncertainty_exceeded`). 44/44 captured at phase boundary. |
| **D2: Edge C4 T1** | `miles-warm` (Edge) | 4 | **NOT RUN** | Halted per stop-the-line policy due to Edge C1 failure. |
| **D3: DDB / PITR / Stats** | AWS DynamoDB | N/A | **PASS (100%)** | 20 tables verified with PITR; Final stats snapshot captured (Dedup 1.92x, Reduction 2.74x). |
| **D3: R1/R2 Recovery & GC** | AWS Coordination | N/A | **UNVERIFIED / DEFERRED** | Deferred due to trial-boundary halt after Edge C1; no live recovery cut performed. |
| **D4: Alarms** | CloudWatch | N/A | **DEFERRED** | Deferred to isolated follow-up run; cluster drained and torn down. |
| **Phase E: Teardown** | AWS ap-southeast-1 | N/A | **PASS (100%)** | 41/41 absence verified, state = 0, SLRs deleted, IAM restored, spend < $0.50 USD. |

---

## 3. Edge C1 Failure Analysis & Diagnostics Correlation

During Edge C1 Trial 1, requests PUT #1 through #46 were committed successfully to DynamoDB and S3. Beginning at request PUT #47, the gateway rejected requests with HTTP 503 `ServiceUnavailable`.

### 100% Correlated Failure Sample (Extracted from `edge-c1-t1-failed-window.json`)

> [!NOTE]
> **Sanitization & Correlation Identity:** In accordance with PR #71 review requirements, raw 16-hex request IDs have been replaced with stable pseudonymous join IDs (`REQ-EDGE-047`...`REQ-EDGE-090`). The complete 1-to-1 mapping to raw AWS request IDs is maintained exclusively inside private operator evidence (`edge-c1-t1-failed-window.json` and raw trace under restricted permissions `0o600`) and is never committed to source control.

All 44 failed PUT requests (#47 $\rightarrow$ #90) were correlated against gateway structured logs on `edge-pilot`:

| Ordinal | Pseudonymous Join ID | HTTP Status | Target Node | Stage | Failure Class | Diagnostic Status |
|---|---|---|---|---|---|---|
| PUT #47 | `REQ-EDGE-047` | 503 | `edge-pilot` | `prepare` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #48 | `REQ-EDGE-048` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #49 | `REQ-EDGE-049` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #50 | `REQ-EDGE-050` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #51 | `REQ-EDGE-051` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #52 | `REQ-EDGE-052` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #53 | `REQ-EDGE-053` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #54 | `REQ-EDGE-054` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #55 | `REQ-EDGE-055` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #56 | `REQ-EDGE-056` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #57 | `REQ-EDGE-057` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #58 | `REQ-EDGE-058` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #59 | `REQ-EDGE-059` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #60 | `REQ-EDGE-060` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| ... | *(44 total)* | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |
| PUT #90 | `REQ-EDGE-090` | 503 | `edge-pilot` | `writer_admission` | `clock_uncertainty_exceeded` | `correlated_gateway_log` |

- **Upstream ALB Errors:** 0 (traffic was directly connected to Edge via Roles Anywhere).
- **Uncollected / Missing Journal Records:** 0 (100% of 44 failures were correlated).

### Root Cause Analysis & Working Hypothesis
- **Direct Mechanism:** Under [`internal/coordination/chrony.go:94`](../../internal/coordination/chrony.go#L94), `Chrony.Bounds()` enforces `age <= min(1024, 2*interval + 2)`. When this limit is exceeded, the node treats its local clock as uncertain and fails closed to prevent silent data corruption or invalid lease acquisition.
- **Working Hypothesis:** The Edge instance is a burstable `t4g.small` (2 vCPUs, 2 GiB RAM, 20% baseline CPU per vCPU). During intensive sustained network I/O and disk write streaming across 46 consecutive 8-MiB chunks, CPU scheduling contention is hypothesized to have delayed the NTP poll execution beyond the sample freshness threshold.
- **Verification Requirement:** Confirming this hypothesis requires cross-referencing high-resolution per-second CPU utilization, CloudWatch CPU credit usage, and raw Chrony tracking logs with millisecond timestamps.

### Stop-the-Line Behavior & Clarification
- In `internal/benchmark/run.go:172-190,304-327`, `filePhase` dispatches all 90 items in the configured workload phase. When the `put` phase completed with 44 errors, `finishPhase` returned a non-zero error, causing the orchestrator to halt at the **trial/phase boundary**.
- The stop-the-line policy was enforced by halting between phases and trials (aborting subsequent HEAD, GET, Edge C4, and D4 Alarms). It did not terminate individual requests in-flight mid-phase. Per-request fail-fast cancellation within `warmbench run` is identified as an optional runner enhancement for future live work.

---

## 4. Teardown & AWS Zero-State Verification (Phase E)

Teardown was executed in accordance with Phase E of the run plan:
1. `pause-reconciled.tfplan` destroyed all 53 active compute, load balancing, alarm, and network endpoint resources.
2. `empty-owned-buckets.py` purged all 3,262 objects from test buckets `miles-dev-data-499388132734-ap-southeast-1` and `miles-dev-pilot-direct-499388132734-ap-southeast-1`.
3. `destroy-final.tfplan` destroyed all 20 DynamoDB tables, VPC networking, KMS alias/key, and Roles Anywhere trust anchor.
4. `delete-owned-service-roles.py` deleted `AWSServiceRoleForRolesAnywhere` and `AWSServiceRoleForElasticLoadBalancing`, verified with status `SUCCEEDED` (`iam/slr-deletion-status.json`).
5. `restore-temporary-iam-final.py` reverted all 5 IAM managed policy defaults to `old_default`, deleted temporary versions, and removed inline policy `MilesPilotAcceptance` (`iam/final-restoration-proof.json`).
6. `verify-final-aws-cleanup.py` performed root read-only queries against AWS APIs:
   - **41/41 direct absence checks PASSED (100%)** (`native-final-cleanup-proof.json`).
   - Terraform workload state: **0 managed resources**.
   - Preserved resources: Only bootstrap state bucket `miles-dev-tfstate-499388132734-ap-southeast-1`.
   - KMS key and Secrets Manager secret scheduled for deletion under standard 30-day AWS retention.

---

## 5. Scope Partitioning & Next Steps

1. **Cloud Workload Acceptance (Cloud C1/C4):**
   - Cloud multi-AZ multi-node setup behind ALB passed with 100% data integrity and zero regressions. Repeating Cloud C1/C4 is not required unless future changes affect Cloud gateway logic.
2. **Dedicated Follow-Up Scope:**
   - **Edge Clock Stability & Tuning:** Investigate chrony configuration (e.g. `minpoll`/`maxpoll`, burst sampling) and CPU credit contention on low-vCPU Edge nodes under sustained network load.
   - **Edge C1 & C4 Retest:** Re-execute Edge benchmark trials once clock freshness tuning is validated on offline fixtures.
   - **Phase D3 Native Recovery & GC Verification:** Validate R1 writer lease/fencing, R2 outbox export/seal, cut age, and checkpoint/GC overhead under a live AWS environment.
   - **Phase D4 Real-Threshold Alarms:** Execute the `OK → ALARM → OK` transition validation using real metric thresholds under isolated conditions.
   - **Runner Fail-Fast Enhancement:** Optional per-request fail-fast cancellation within `warmbench run` phases to halt immediately on first error instead of completing the phase.
