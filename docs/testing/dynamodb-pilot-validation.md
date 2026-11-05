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
