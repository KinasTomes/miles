# Cloud C1 Trial 1 Live Run Report (2026-11-06 12:55 UTC Rerun)

- **Date / Window:** 2026-11-06 12:55 UTC to 15:25 UTC (Scope ID: `w05-hung03-live-20261106-1255`)
- **AWS Region / Account:** `ap-southeast-1` / `499388132734`
- **Soft Budget:** $10 (actual billing pending lag; 0 compute/workload resources retained)
- **Status:** **STOP-THE-LINE TRIGGERED; TEARDOWN COMPLETED (STATE 0 VERIFIED). PR #65 REMAINS DRAFT; TASK W05-HUNG-03 REMAINS DOING.**

---

## 1. Executive Summary

A live diagnostic rerun of Cloud C1 Trial 1 (`backup30`, concurrency=1) was initiated under user approval to observe gateway runtime diagnostics under real AWS conditions.
- **Trial Outcome:** Out of 90 PUT requests, **83 PUT succeeded (HTTP 200)** and **7 PUT failed (HTTP 500 InternalError)**.
- **Stop-The-Line:** In strict accordance with the remediation plan rules, upon detecting the first failed trial, all further trial executions (Cloud C4, Edge C1, Edge C4) were immediately halted.
- **Evidence Collection:** Gateway journal logs were captured and correlated against client request IDs.
- **Teardown & Absence:** Full Teardown D4 was executed. All 4 EC2 instances, ASG, ALB, endpoints, 20 DynamoDB tables, KMS key, and S3 data/direct buckets were removed. All IAM policies were rolled back to their baseline versions. Native AWS absence verification verified **41/41 absence checks PASS** (Workload state: 0).

---

## 2. Failure Analysis & Correlation

The client benchmark executed 1 preflight `ListObjectsV2` request followed by 90 sequential PUT requests (each 8 MiB) routed via the ALB.

### Sequence Breakdown of 90 PUT Requests
1. **Initial Phase (PUT #1 $\rightarrow$ #34):** 34 requests succeeded (HTTP 200), balanced across `cloud-a` and `cloud-b`.
2. **Instability Window (PUT #35 $\rightarrow$ #47, spanning 12.5 seconds from 13:22:02Z to 13:22:14Z):**
   - The ALB distributed requests round-robin between the two active nodes.
   - **7 requests routed to `cloud-a`:** All 7 failed with HTTP 500 (`clock_uncertainty_exceeded`).
   - **6 requests routed to `cloud-b`:** All 6 succeeded normally (HTTP 200, 8 MiB payload).
3. **Recovery Phase (PUT #48 $\rightarrow$ #90):** Exactly **43 requests succeeded** following the last failure (both `cloud-a` post-recovery and `cloud-b`).
4. **Totals:** $34 + 6 + 43 = 83$ successful PUTs; $7$ failed PUTs ($83 + 7 = 90$).

### Correlated Failure Window (Verified against `cloud-c1-t1-failed-window.json`)
100% (7/7) of the failed requests match the exact gateway journal logs on `cloud-a` (`i-03bea44ea0f048125`):

| Trial PUT # | Timestamp (UTC) | Client Request ID | HTTP Status | Target Node | Stage | Failure Class |
|---|---|---|---|---|---|---|
| PUT #35 | 13:22:02.387Z | `6D3C1BC6DDAC8A78` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| PUT #37 | 13:22:05.182Z | `B48EC5D2B70EC26A` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| PUT #39 | 13:22:07.264Z | `224444512613BA71` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| PUT #41 | 13:22:08.708Z | `8112EEEAE1D562B1` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| PUT #43 | 13:22:11.182Z | `A18AFBF8A7E7D7B9` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| PUT #45 | 13:22:13.237Z | `4C909826697B375B` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| PUT #47 | 13:22:14.841Z | `A96DBCD93956F1CE` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |

- **Upstream ALB 503:** 0 occurrences.
- **Uncollected / Missing Journal Records:** 0 occurrences.
- **Node Isolation:** Node `cloud-b` handled alternating PUTs #36, #38, #40, #42, #44, #46 without any error, isolating the issue strictly to `cloud-a` during this window.

---

## 3. Root Cause Analysis: Chrony Clock Guard & Verification Status

### A. Observed Facts (Bằng chứng thực nghiệm đã xác minh)
1. Gateway journal logs on `cloud-a` recorded exact structured log entries: `msg="s3 server failure"`, `stage="writer_admission"`, `failure_class="clock_uncertainty_exceeded"`.
2. Fail-closed rejection is triggered in `internal/app/shared.go` when `utc.Bounds(ctx)` returns `storage.ErrClockUncertain`.
3. Node `cloud-b` remained healthy and in-sync throughout the exact same window.
4. After `13:22:14.841Z`, all 43 subsequent PUT requests sent to the cluster succeeded, indicating `cloud-a` returned to healthy admission.

### B. Analytical Model & Invariants (Quy tắc thiết kế và mô hình tính toán)
- In `internal/coordination/chrony.go:93–101`, observation age is checked against:
  $$\text{sample\_age} \le \min(1024, 2 \times \text{poll\_interval} + 2)$$
- With `poll 4` (16s nominal polling against AWS Time Sync `169.254.169.123`), the threshold is $2 \times 16 + 2 = 34\,\text{s}$.
- If a single NTP response packet is dropped or delayed beyond 34 seconds from the previous sample, the guard immediately marks the clock untrusted, rejecting write admissions to protect R1 monotonic lease invariants.

### C. Analytical Hypothesis vs. Missing Evidence (Phân định giả thuyết và bằng chứng thiếu)
- **What is verified:** The gateway code rejected requests due to `clock_uncertainty_exceeded` at writer admission.
- **What is a hypothesis (not yet verified with raw packet traces):**
  - The exact gap duration (e.g. 34.5s – 46s) and exact recovery instant (13:22:15Z) were deduced from the failure window timestamps and the code formula, NOT from continuous wire packet captures or sub-second `chronyc tracking` polling during those 12 seconds.
  - The `chronyc tracking` snapshot in `cloud-c1-t1-full-diagnostics.json` was captured at `13:35:27Z` (12 minutes after trial termination), reflecting healthy steady state, not the transient failure event itself.
- **Conclusion:** While consistent with all available evidence and code constraints, the NTP packet-drop hypothesis must be treated as an **analytical model**, not a 100% empirically proven wire trace. To achieve complete proof in future runs, the diagnostics collector must sample continuous node telemetry and chrony state during execution.

---

## 4. Teardown D4 & AWS Absence Verification

Following the stop-the-line procedure, full teardown was executed:

1. **Active Fleet:** Applied `pause.tfplan`. ASG `miles-dev-gateway`, ALB `miles-dev`, interface VPC endpoints, test alarms, and all 4 EC2 instances were terminated.
2. **S3 Version Clearance:** Data bucket `miles-dev-data-499388132734-ap-southeast-1` was emptied of all 1,021 versions/markers flushed during instance draining.
3. **Terraform State 0:** Remaining 83 resources were destroyed via Terraform. `terraform state list` verified empty (0 resources).
4. **Prerequisites Teardown:**
   - ACM Certificate `arn:aws:acm:ap-southeast-1:499388132734:certificate/ccfb9ca8-112c-4bf3-b44c-329945e13a49` deleted.
   - SSM Document `miles-dev-shell` deleted.
   - Secrets Manager secret `miles-dev/runtime-secrets-pilot-20261106-1255` deletion scheduled (PendingDeletion).
   - Service-Linked Roles `AWSServiceRoleForRolesAnywhere` and `AWSServiceRoleForElasticLoadBalancing` deleted (`SUCCEEDED`).
5. **IAM Rollback:**
   - Restored 5 managed IAM policies to baseline default versions (`MilesDevDeployRuntimeRoles` v1, `MilesDevRuntimeBoundary` v2, `MilesDevDeployData` v1, `MilesDevDeployCompute` v1, `MilesDevDeployServices` v3).
   - Deleted 5 temporary IAM policy versions.
   - Removed inline policy `MilesPilotAcceptance` from role `miles-dev-deploy`.
6. **Native Absence Verification:**
   - Verified via `verify-final-aws-cleanup.py`: **41/41 checks passed**. Proof stored in `native-final-cleanup-proof.json`.
   - Retained buckets: strictly `miles-dev-tfstate-499388132734-ap-southeast-1`.

---

## 5. Code & Pipeline Updates

- **`scripts/runner_diagnostics.py`:**
  - Updated `sanitize_benchmark_failure` to accept root-level `samples` list emitted by `warmbench` in addition to dictionary `phases`.
  - Added `parse_prometheus_metrics` to safely parse Prometheus exposition metrics from `/metrics` endpoints.
  - Added `build_failed_window_payload` to standardize diagnostic assembly with explicit before/after telemetry snapshots and node status.
  - Fixed trailing whitespace at line 150 (`git diff --check` passes cleanly).
- **`scripts/tests/test_runner_diagnostics.py`:**
  - Added unit test `test_sanitize_benchmark_failure_toplevel_samples`.
  - Added unit test `test_parse_prometheus_metrics`.
  - Added unit test `test_build_failed_window_payload_with_telemetry`.
  - All 45 python tests pass cleanly.
- **`.gitignore`:** Added `__pycache__/` and `*.py[cod]` rules to keep the worktree clean of untracked Python bytecode.
- **Local Gates:** `make check`, `make test-local`, and `go test -race` for all packages pass 100%.
- **PR & Task Tracking:** PR #65 remains in **Draft**; Task **W05-HUNG-03** remains **Doing**.
