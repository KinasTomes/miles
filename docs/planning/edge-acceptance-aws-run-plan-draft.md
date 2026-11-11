# Miles — Edge & recovery/alarms AWS run plan (DRAFT — NOT APPROVED)

**Status:** `DRAFT / DO NOT EXECUTE` — created after PR #73 merged, 2026-11-08.
**Companion:** [edge-acceptance-followup-plan.md](edge-acceptance-followup-plan.md)
**Reference only:** [previous executed AWS plan](aws-live-acceptance-run-plan.md) / [live run report](../testing/live-acceptance-20261108-report.md). **Previous approval, plans, AMI/credentials and budget window cannot be reused.**

## 1. Owner approval required before any AWS API activity

| Field | To fill/approve |
|---|---|
| Account / Region | `499388132734` / `ap-southeast-1` — confirm by scoped caller identity |
| Fresh run ID, owner, approval timestamp | **TBD / NOT APPROVED** |
| Source commit SHA & tested arm64 release SHA-256 | **TBD**, pin from reviewed merged code |
| Dataset / manifest hash | Suggest `backup30`, 90 × ~8 MiB; **revalidate hash** |
| Deployer non-root role & permission boundary | **TBD**, do not use root |
| Maximum EC2 count/type, DDB/endpoint/ALB, S3 buckets | **TBD**, explicitly scoped |
| Network ingress, allowed client CIDR /32 | **TBD**, private/SSM-only admin |
| Max duration including teardown + start/deadline | **TBD** |
| Soft budget in USD, stop threshold, teardown threshold | **TBD** — previous $10 soft cap is historical, not approval |
| Teardown ownership, KMS/Secrets pending deletion & bootstrap exceptions | **TBD** |
| Data classification | Synthetic only; no customer data |
| Gate authority / escalation contact | **TBD** |

**Hard stop:** Không dùng quyền/phê duyệt phiên cũ; không gọi AWS để chạy preflight trước approval cho giai đoạn này. CloudWatch billing/cost reports có độ trễ; budget alarms không bảo đảm hard cap. Chỉ dùng scoped IAM session với expiry/time window được owner xác nhận.

## 2. Readiness & run steps (sẽ áp dụng sau approval)

| Gate | Procedure | Acceptance / evidence | On failure |
|---|---|---|---|
| C: Preflight | Validate IAM/caller, read-only baseline 41-check inventory, quota, fresh Terraform plans, CI, release/manifest checksums, stop conditions | New owner approval+paid window; reviewed plan hashes, clean baseline within owned scope | Do not apply |
| D0: Provision | Fresh Cloud/Edge stack only as approved; chronyc host wrapper, metrics, readiness, ALB, DDB/PITR, alarms enabled | Host UTC provenance & runtime-equivalence verified, cloud and edge readiness, safe synthetic fixture | Capture & teardown |
| D2-A: Edge C1 | `warmbench --fail-fast`, C1, `backup30`, SHA256 GET, 4 read passes | 90/90 PUT, 90/90 HEAD, 360/360 GET hashes, zero 500/503 and no new clock rejections | Stop dispatch, wait in-flight, persist failed-window, teardown |
| D2-B: Edge C4 | Only if Edge C1 PASS, same release, dataset and policy | Same completeness and zero-error gate; record transport + host UTC + credits/scheduling | Stop, capture, teardown |
| D3: Native recovery | R1 lease/fence/closed writer, R2 outbox export/seal/cut, checkpoint/GC, DDB PITR/capacity | Versioned, timestamped native proof for each item; failure injection isolated and controlled | Stop, capture, teardown |
| D4: Shallow alarms | `deep_observation=false`, 4 metrics/profile × Cloud and Edge; threshold-driven real signals | 8 native histories `OK → ALARM → OK` + dimensions & timestamps, no `SetAlarmState` | Mark FAIL/DEFERRED, follow stop policy |
| E: Cleanup | Export sanitized evidence, drain, destroy OWNED resources, restore policy/SLR if created, direct absence checks | Workload state 0, new 41-check proof (scope stated), exceptions/pending deletion explicit, billing estimated now/invoice later | Escalate; do not conceal leaks |

**D4 shallow alarm identities (actual Terraform contract):** `recovery_cut_age` (`miles_recovery_cut_age_seconds` > 3600, missing=breaching); `integrity` (`miles_warm_read_integrity_failures_total` > 0, missing=notBreaching); `throttling` (`miles_ddb_throttles_total` > 0, missing=notBreaching); `telemetry_age` (`miles_runtime_observation_age_seconds` > 60, missing=breaching). Dimensions `profile` and `installation`; period 60s, evaluation periods 3, datapoints-to-alarm 1. The 8 alarms are exactly the 4 above × Cloud and Edge; test with safe isolated native metric sources, **not** forced state changes. Require sufficient time for state transitions and clear recovery before teardown.

## 3. Evidence to capture (private / sanitized)

- Private: signed approval, expected launch manifest and hashes, Terraform state/plans, AWS IDs/credentials if necessary, raw requests/logs and mapping, stored with least privilege and 0600 where applicable. Never commit.
- Git-publishable: pseudonymous request join IDs, versioned summary tables, per-gate verdict, host Chrony timing/provenance, CPU-credit correlation with source/resolution/confidence, redacted alarm history, owned-resource absence counts, cost estimate with later billed reconciliation.
- Each failure: error onset request/timestamp, admitted vs dispatched vs in-flight vs skipped, gateway stage and HTTP status, genuine host-chronyc elapsed, NTP source/poll/age, node CPU/credits and timestamp resolution. If a signal is missing, label **UNVERIFIED**, never guess.
- Protect unknown-commit: do not retry PUT merely because client timeout; reconcile durable state first. Preserve owned prefix when stop triggers, then include it in cleanup inventory.

## 4. Explicit decision gates

- **NO-GO** if prior P0 telemetry corrections/`--fail-fast` are unmerged or their offline tests fail; missing owner approval; unbounded cost/teardown; missing scoped IAM; unknown artifact provenance; or clock source not verified at startup.
- **STOP-THE-LINE** if any 500/503/checksum/lease rejection/clock-uncertainty breach. Fail-fast only prevents new dispatch; wait/reconcile all in-flight and capture evidence. Do not escalate C1→C4 while failing.
- **TEARDOWN ALWAYS** even on D2/D3/D4 FAIL; no implicit overnight retention. A new AWS run requires its own explicit owner approval, source release pin and billing window.

**Execution status: NOT STARTED.** Editing this file is planning only; it does not authorize AWS activity.
