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
(Window: 05:05Z - 07:35Z) in `ap-southeast-1` using On-Demand throughput capacity
on all 20 DynamoDB tables, dual Cloud gateways (`cloud-a`, `cloud-b`), one Edge
gateway, and a dedicated pilot client:

- **PUT workload (100% PASS):** 90/90 `PutObject` requests succeeded (754,994,670 bytes,
  p50 latency 1,932 ms). All 90 objects were verified committed in `miles-dev-Objects`.
  The DynamoDB On-Demand capacity provisioning fully eliminated the PUT 500/503 errors
  observed in previous runs.
- **HEAD verification (100% PASS):** 90/90 `HeadObject` requests succeeded (p50: 9.2 ms).
- **GET verification (84/90 PASS, 6 FAIL):**
  - All 45 GET requests routed to `cloud-b` passed 100%.
  - On `cloud-a`, request #79 was truncated (4,901,763 / 8,388,608 bytes read), and 5
    subsequent requests (#81, #83, #85, #87, #89) returned HTTP 500 `InternalError`
    (`stage: object_get`, `failure_class: unknown`).
  - **Zero data corruption / Zero lost objects:** All 6 failed objects were subsequently
    downloaded directly via AWS CLI from the pilot client. All 6 downloaded successfully
    with 100% byte match and exact SHA256 match (`miles_warm_read_integrity_failures_total: 0`).
  - **Root Cause Confirmed (Clock Uncertainty Guard):** System chrony tracking logs on
    `cloud-a` revealed a 49.2-second gap between NTP packets from `169.254.169.123`
    (05:27:58Z to 05:28:47Z). Because `internal/coordination/chrony.go:94` enforces
    `maxAge = 2*interval + 2 = 34.6s`, at 05:28:32.6Z the clock bounds uncertainty
    threshold was exceeded. `Chrony.Bounds()` rejected lease issuance (fail-closed safety),
    causing the active stream to abort and subsequent requests on `cloud-a` to be rejected
    at `WarmGate.Acquire`. Node `cloud-a` recovered `miles_ready=1` immediately upon the
    next NTP packet at 05:28:47Z. See detailed RCA in [`cloud-c1-t1-live-run-20261106-rca.md`](./cloud-c1-t1-live-run-20261106-rca.md).
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
  - Total AWS spend remained under 1.00 USD (well below the 10 USD soft budget).

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
