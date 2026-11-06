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

The client benchmark reported 7 HTTP 500 errors occurring consecutively between `13:22:02Z` and `13:22:14Z`.

### Correlated Failure Window
Using the sanitized runner diagnostics (`cloud-c1-t1-failed-window.json`), 100% (7/7) of the failed requests were joined with node journal records:

| Timestamp (UTC) | Request ID | HTTP Status | Node ID | Stage | Failure Class |
|---|---|---|---|---|---|
| 13:22:02Z | `473F4F19E9B3DDE4` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| 13:22:04Z | `0BCE54AC02167D0C` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| 13:22:06Z | `CF7C27B8CEB51F9B` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| 13:22:08Z | `C549B804EB65E5C4` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| 13:22:10Z | `1FE99BC9CC20B440` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| 13:22:12Z | `B670E90DE6CE84DC` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |
| 13:22:14Z | `7C3B611D36B512AE` | 500 | `cloud-a` | `writer_admission` | `clock_uncertainty_exceeded` |

- **Upstream ALB 503:** 0 occurrences.
- **Uncollected / Missing Journal Records:** 0 occurrences.
- **Target Node:** All 7 failures occurred on `cloud-a` (`i-03bea44ea0f048125`). Node `cloud-b` handled its requests during this window without error.

---

## 3. Root Cause Investigation (Chrony Clock Guard)

### Observed Mechanism
1. The gateway enforces fail-closed data integrity (R1 monotonic lease and commit timestamp safety) via `utc.Bounds(ctx)` in `internal/app/shared.go`.
2. Under `internal/coordination/chrony.go:93–101`, the clock observation is marked untrusted when:
   $$\text{sample\_age} > \min(1024, 2 \times \text{poll\_interval} + 2)$$
   With `poll 4` (16-second poll interval against AWS Time Sync `169.254.169.123`), the threshold is $2 \times 16 + 2 = 34\,\text{s}$.
3. During the 12-second window from `13:22:02Z` to `13:22:14Z`, `chronyd` on `cloud-a` experienced a delay in receiving its expected NTP sample packet from `169.254.169.123`, causing the last sample age to climb from ~34.5s to ~46s.
4. Because the clock uncertainty exceeded the safety threshold, the gateway returned `storage.ErrClockUncertain`, rejecting writer admission to protect epoch ordering.
5. At `13:22:15Z`, the next NTP sample arrived. The sample age immediately dropped to ~0.5s, `utc.Bounds()` restored trusted status, and all 76 remaining PUT requests succeeded with 100% success rate.

This confirms the fail-closed clock guard is functioning exactly as specified to preserve R1 safety invariants under real network jitter.

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

- **`scripts/runner_diagnostics.py`:** Updated `sanitize_benchmark_failure` to accept root-level `samples` list emitted by `warmbench` in addition to dictionary `phases`.
- **`scripts/tests/test_runner_diagnostics.py`:** Added regression test `test_sanitize_benchmark_failure_toplevel_samples`. All 43 python tests pass.
- **Local Gates:** `make check`, `make test-local`, and `go test -race` for all packages pass 100%.
- **PR & Task Tracking:** PR #65 remains in **Draft**; Task **W05-HUNG-03** remains **Doing**.
