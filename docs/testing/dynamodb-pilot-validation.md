# DynamoDB pilot validation

The pilot is prepared from the shared runtime with checkpoint/rebuild and Warm
GC/compaction merged. See [procedure and metric definitions](../../benchmarks/dynamodb-pilot.md).
This page records local proof. It does not report a new AWS measurement.

## Checks

| Check | Evidence |
| --- | --- |
| `make check` | Format, vet, Go/Python tests and release/CLI/harness builds. |
| `go test -race ./internal/observability ./internal/benchmark ./cmd/warmbench` | Native signed SDK capacity/throttle parsing; preserved frozen transaction parameters, HTTP consumed bytes/partial reads/empty-body framing, fixed series inventory, streaming response behavior, stale gauge removal and retained-prefix workload. |
| Native DynamoDB Local/Moto app + metadata race suites | Three real shared compositions (two Cloud, one Edge), producer commits/multipart, recovery cut/seal, checkpoint/GC, process-owned Warm metrics and complete physical statistics. Atomic event observation survives idempotent retry and disappears from pending lag after export. |
| Native recovery race suite | Export/seal and fault scenarios against pinned loopback fixtures. |
| `terraform validate`, `terraform test` |38 mock-provider plans, including four new alarm plans: opt-in default, exact publisher dimensions/thresholds, Cloud-only and pause removal. No AWS calls. |
| `python3 -B -m unittest discover -s scripts/tests -p test_dynamodb_pilot.py -v` | Report route/ratio separation and negative cases for mismatched workload/binary/encryption, failures/unhashed GET, missing routes, restart, stale gauges, missing nodes/incomplete statistics and invalid collection windows. Synthetic test inputs are not performance evidence. |

Fixtures use the pinned Docker images in `deploy/local/compose.yaml`, fresh
loopback-only ports, isolated table/bucket names and temporary local paths. Only
the owned compose project is removed, including on test failure. No persistent
runtime service or performance dataset is installed on the operator machine.

The sampled gate metric cannot provide a complete distribution of short gate
closures. Native capacity reports can omit failed/unknown consumption or a
read/write breakdown. These limits are carried into the report and runbook.
The added Outbox timestamp is telemetry beside the immutable event, not a new
recovery ordering rule; unresolved legacy events without it withhold observations.

## AWS acceptance still required

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

Alarms remain opt-in and no resources have been applied for this pilot. Prior AWS
results refer to their original releases and do not certify the added telemetry
or this pilot. Local success and a merged PR alone do not complete acceptance.
