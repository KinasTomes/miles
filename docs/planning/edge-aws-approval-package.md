# Miles — AWS Edge follow-up approval package

**DRAFT — NOT AN APPROVAL — NO LIVE AWS AUTHORIZED**
**Prepared:** 2026-11-10. **Current baseline:** merged Gate F PR #74 (`d4c6ead`) and Gate R PR #75 (`633cb0e`).
**Execution runbook:** [Edge / recovery / alarms AWS run plan](edge-acceptance-aws-run-plan-draft.md).
**RCA evidence:** [Edge clock RCA](../testing/edge-clock-rca-followup.md).
**Historical run only:** [2026-11-08 AWS report](../testing/live-acceptance-20261108-report.md).

## 1. Decision requested from the owner

There are **two separate permissions**. They are not implied by this document or by earlier test approvals.

- **Decision A — read-only Gate C authorization:** permit a named non-root operator to verify the exact account/Region, read scoped service inventory, review current quota/cost assumptions and prepare fresh Terraform saved plans. No `apply`, state mutation, benchmark traffic, or new billable resources. The owner must set a UTC expiry for this authorization.
- **Decision B — paid live acceptance authorization:** after signing off on actual inventory, reviewed saved plans, pinned source/release/manifest hashes, IAM and costs, authorize the specifically scoped D0→D2→D3→D4→E sequence. The owner must explicitly approve a stop/deadline including teardown and designate fallback cleanup responsibility.

**Current decision:** neither A nor B has been granted for this next run. The historical `$10 / 3h` limit was for 2026-11-08 only and is **not** authorization to spend again.

## 2. Facts fixed by repo vs values not yet approved

| Item | Evidence-backed proposal / state | Must be decided |
|---|---|---|
| Repository baseline | `main` has PR #74 + #75 + #77; candidate latest reviewed runtime baseline `1dcc35298bd0432f434492d3a59f936738b61d27` | Confirm at Gate C; rebuild and pin linux/arm64 binary SHA-256 |
| Account / Region | Proposed account `499388132734` in `ap-southeast-1`; **not** the independent Sydney S3 Glacier demo | Authorize and revalidate via non-root scoped CLI after Decision A |
| Pending Edge RCA | 46/90 success; 44/90 fail; 44/44 exact gateway matches; underlying cause still unresolved | Implement/test or authorize diagnostic-only collector before D2 |
| Dataset | Proposed `backup30`, 90 objects, synthetic; previous manifest `04014dd767e799d9f70081536d01c988fa6a02c0b7653c34db11a48c9abc1556` is **reference only** | Regenerate/verify actual dataset and sign its hash |
| IAM | Proposed MFA-protected scoped `miles-dev-deploy`; no root, no shared credentials | Identify operator, permissions/boundary, session lifetime, roles for provision/teardown |
| Compute | Prior reference: up to four `t4g.small` ARM64 (2 Cloud, 1 Edge, 1 client), 20GB encrypted EBS each, 0 NAT gateway | Refresh quota/plan/resource ownership after A; explicit max count |
| Alarms | 8 shallow alarms, 4/profile (Cloud/Edge), `deep_observation=false` | Confirm test fixture/transition method and evaluation time |
| Gate E | Prior run proved 41/41 scoped absence in 2026-11-08; no entitlement to assume today's inventory absent | Revalidate list of owned resources and functioning native final verifier |
| Cost/window | Earlier `$10 / 3h` historical only; hourly estimates and billing lag are not a guaranteed cap | New amount, stop threshold, start/end UTC, teardown buffer and escalation |

## 3. Mandatory checks before Decision B (paid authorization)

- [ ] Gate C read-only authorization A issued, signed, with expiry; correct assumed-role ARN, account, Region and boundaries verified. Keep identity evidence private.
- [ ] Fresh owned-resource baseline and exceptions reviewed, including residual KMS/Secrets pending deletion and IAM/SLR state; all tagged resources have a teardown owner.
- [ ] New saved Terraform plans **and destruction path** reviewed, with frozen plan hashes. No reliance on saved plans from the last run.
- [ ] Offline test matrix PASS on pinned source: `make check`, `make test-local`, `go test -race ./internal/benchmark ./cmd/warmbench`, documentation naming and `git diff --check`.
- [ ] linux/arm64 binaries built from verified source and SHA-256 pinned; actual `backup30` manifest hash/size verified, no customer data.
- [ ] BLOCKED — pending reviewer hotfix verification: Host Chrony/OS diagnostic sampling implemented and offline-tested (PR #77 baseline + PR #76 hotfix: CWD-independent imports, preflight checks, fail-closed empty-window propagation, strict ring-buffer hard-cap bounding; `scripts/edge_host_telemetry.py` & tests); local timestamp provenance, command duration, scheduler/CPU steal, ring-buffer limits and graceful export verified offline.
- [ ] Cost envelope calculated from current plan: EC2/EBS, ALB, endpoints/data transfer, S3 operations/versions, DynamoDB/PITR, CloudWatch logs/alarms, KMS, retention/deletion effects; values and billing-delay assumptions reviewed.
- [ ] Run operator, independent stop/teardown owner and fallback contact assigned; latest workload start, final teardown UTC and response to quota/clock/permission failure documented.
- [ ] BLOCKED — pending reviewer hotfix verification: Native service checks and teardown/absence verifier implemented and offline-tested (PR #77 baseline + PR #76 hotfix: comprehensive resource inventory [subnets, SGs, VPC endpoints, instance profiles, trust anchors, profiles, instances, volumes, target groups, ASGs, SSM parameters], empty inventory fail-closed, CloudWatch describe-alarms empty list fix, 0600 ledger permissions; `scripts/verify_teardown_absence.py` & tests); the earlier historical `41/41` is not treated as current proof.
- [ ] Private approvals B and paid window recorded **after** checks complete. If any box remains unchecked: **NO-GO**.

## 4. Prepared cost/window worksheet (fill only after authorized preflight)

| Component | Quantity / mode | Unit pricing source and assumptions | Estimated USD |
|---|---|---|---|
| EC2 & EBS | Proposed up to 4 × `t4g.small` ARM64 and encrypted block devices | Current Region prices, reserved wall time, CPU credit mode | **TBD** |
| ALB / transfer / endpoints | 1 approved ALB footprint, private connectivity, no NAT planned | ALB hours/LCUs, gateway vs interface endpoints, cross-AZ bytes | **TBD** |
| DynamoDB / PITR | Planned tables, RCU/WCU, storage and PITR duration | Selected billing mode and approved trial duration | **TBD** |
| S3 | Test objects, read repeats, version cleanup, API calls | Storage class, PUT/GET/HEAD/LIST, delete markers | **TBD** |
| CloudWatch | 8 alarms, logs, custom metrics/scrapes | Alarm-hours, metric and log ingestion/retention | **TBD** |
| Other | KMS/Secrets, SSM/transfer, buffer and post-run retained items | Verify applicable charges | **TBD** |
| **Estimated total + contingency** | Cap to be approved | Sum of above × worst-case duration + guardband | **TBD** |

**Decision B must state:** approved amount in USD, earlier stop-new-work trigger, latest teardown trigger, maximum payable duration, UTC start/stop and uncertainty margin. Billing console alerts are not hard spending limits. A responsible operator must monitor actual resources and exit early.

## 5. Exact acceptance / failure handoff

| Gate | Pass condition | Evidence artifact (private first; sanitized summary only in Git) |
|---|---|---|
| **C** | Correct scoped identity + Region, current inventory, IAM, pinned release & dataset, reviewed saved plans and signed budget/window | `gate-c-preflight-summary.json` + signed private approvals + plan checksum manifest |
| **D0** | Clock source verified on host, diagnostic recorder sampling **before** load, healthy designated nodes, private connectivity, fixtures, PITR/alarms | `gate-d0-readiness.json` and timestamped host sample contract |
| **D2 Edge C1** | 90/90 PUT, 90/90 HEAD, 360/360 GET hashes, 0 500/503, no clock uncertainty, no indeterminate writes | `edge-c1-result.json` + gate decision and host/gateway timeline |
| **D2 Edge C4** | Same complete matrix, only after Edge C1 PASS | `edge-c4-result.json` and native clock/CPU correlation |
| **D3 native R1/R2** | Lease/fencing/closed writer; outbox seal/cut/checkpoint/GC; native DDB/PITR and safe fixture proof | `gate-d3-native-evidence-summary.json` + private raw proofs |
| **D4 alarms** | 8 histories `OK → ALARM → OK` from real metrics & thresholds, not `SetAlarmState` | `gate-d4-alarm-evidence-summary.json` plus native alarm history |
| **E teardown** | New native absence checks for **owned** resources, no untracked leaks, pending deletions disclosed | `gate-e-absence-summary.json`; Terraform destroy proof, cost reconciliation ledger |

*Artifact names are planned evidence outputs, not claims that those files/scripts already exist.* All stored raw data (request IDs, AWS object names, Terraform state, logs, role information) stays outside Git under restricted permissions; redaction must be tested. Any clock uncertainty / HTTP 503 / integrity mismatch → `--fail-fast` stop new dispatch, safe drain, reconcile unknown writes, preserve evidence, then teardown. Never silently bypass D2 to run D3/D4 under the same failed gate.

## 6. Private owner-authorization template (NOT TO COMMIT AS AN ACTUAL SIGNATURE)

Copy the following fields into a *private, owner-controlled* run-specific record. Keep `authorized=false` until the owner grants the named action. Do not paste signed authorization, secret-bearing plan or sensitive credentials back into the repository.

```json
{
  "run_id": "TBD",
  "authorization_phase": "gate-c-read-only",
  "authorized": false,
  "owner_identifier": "TBD_PRIVATE",
  "operator_identifier": "TBD_PRIVATE",
  "approved_at_utc": null,
  "expires_at_utc": null,
  "account_id": "499388132734",
  "region": "ap-southeast-1",
  "scope": "read-only-inventory-and-plan-only",
  "source_sha": "1dcc35298bd0432f434492d3a59f936738b61d27",
  "arm64_release_sha256": null,
  "dataset_manifest_sha256": null,
  "terraform_saved_plan_sha256": null,
  "max_spend_usd": null,
  "stop_new_work_usd": null,
  "absolute_teardown_deadline_utc": null,
  "cleanup_owner": "TBD_PRIVATE",
  "run_mode": "TBD"
}
```

For Decision B, create a **new approval** with `authorization_phase="paid-live-acceptance"`, explicit permission for reviewed plans, window and cleanup, and an actual approving owner signature. Approval A alone does not allow `apply` or a paid workload.

## 7. Operational handoff

1. Implement and validate safe host sampler and teardown verifier **offline**, then review.
2. Obtain explicit owner Decision A; only then perform scoped preflight and compute the current dollar envelope.
3. Update this package with redacted preflight findings and private SHA references; request Decision B.
4. Execute [the staged run plan](edge-acceptance-aws-run-plan-draft.md) only under the signed envelope; preserve Gate F/R fail-closed guarantees, do Phase E in success **or** failure.
