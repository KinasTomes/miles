# Jobs / Nodes / metrics backend validation

Task **W04-HUNG-02**, ngày29/10/2026. Hard dependencies W03-HUNG-03,
W03-HAI-03 và W03-PHONG-02 đã Done trên backlog hiện hành. Acceptance W4:
job contract kiểm thử bằng fake lease; job không chỉ tồn tại trong RAM request node.

| Checklist | Implementation / proof |
|---|---|
| Shared Jobs/Nodes, claim/checkpoint/cancel, heartbeat/drain | Same contract suite on durable Badger and native DynamoDB Local; conditional claim, exact policy history, safe-point cancellation, renewal port and sticky drain |
| Health/custom metrics and batch CloudWatch | Runtime worker lifecycle and cached heartbeat health; bounded enum labels, shared Prometheus snapshot, native SDK signed loopback batch and delta/error tests |
| Operational CLI and node/cluster schemas | Bearer-only job ls/show/run/cancel, node ls/drain; HTTP method/body/pagination/token tests; bounded per-node results with explicit failure states |

## Verification

`make check` passed: format, vet, all Go/Python tests, document names and binaries.
Targeted tests with `-race -count=1` cover jobs, both metadata backends, runtime,
observability, adminclient and dsgctl. Native tests use an explicitly selected
DynamoDB Local loopback endpoint and per-test tables with scoped cleanup.

Shared backend contract has eight cases on **each** backend:

1. Peer reads the same job; policy changes do not change its pinned revision;
   missing history fails before claim instead of selecting current.
2. Concurrent claims produce one winner, not two workers.
3. Cancel stays cancelling until the exact pending step reaches a checkpoint;
   terminal cancellation does not rewrite/rollback the result.
4. A safe checkpoint survives takeover; the old fence cannot finish the job.
5. Wrong/pending step, unsettled takeover and expired fake lease do not succeed.
6. Queued cancellation and sticky node drain reject stale/incarnation changes.
7. Job heartbeat calls the lease provider; failure does not advance the job;
   draining nodes cannot begin another step.
8. Bounded continuation pages preserve items; public Job.View omits worker capability
   and internal params/checkpoint fields.

Additional native DynamoDB test pauses at transaction dispatch, closes the old
lease through another adapter, then submits the old claim. Its conditional write
is rejected and the job remains queued at its original revision. Native attribute
round-trip tests cover empty optional JSON binary fields. Badger close/reopen
proves queued jobs survive process/store restart.

Node/runtime cases cover local admission closing before a failed metadata write,
remote marker versus actual gate closure, active-request preservation, monotonic
heartbeat staleness including a wall jump, persisted shutdown drain, and normal
parent cancellation/deadline. Production dependency readiness is still503.

Metrics cases prove scrapes do not probe/call AWS, labels cannot include IDs,
CloudWatch sends one bounded batch, counters export deltas, failed export retains
the delta and raw error secrets are not returned. Native SDK test uses a signed
request to loopback, including its gzip/CBOR wire protocol; it does not contact AWS.
Pinned CloudWatch SDK **v1.75.0** uses the existing AWS core/Smithy versions.

CLI tests cover methods/paths/bodies for run/show/cancel/drain, list limits/cursors,
private bearer file, invalid IDs/params/flags and unsupported routes. Existing
transport tests retain TLS/loopback, redirect, response cap and error redaction checks.

## Reproduce

```bash
make check
docker compose -f deploy/local/compose.yaml -p miles-jobs-contract up -d dynamodb
docker compose -f deploy/local/compose.yaml -p miles-jobs-contract port dynamodb 8000
# Set endpoint to the explicit 127.0.0.1 port printed above, never an AWS URL.
MILES_DDB_LOCAL_ENDPOINT=http://127.0.0.1:PORT MILES_DDB_LOCAL_REQUIRED=1 \
  go test -race -count=1 ./internal/metadata/dynamodb ./internal/metadata/badger \
  ./internal/jobs/... ./internal/app ./internal/observability ./internal/adminclient ./cmd/dsgctl
docker compose -f deploy/local/compose.yaml -p miles-jobs-contract down --volumes --remove-orphans
```

CI's existing DynamoDB job now includes the operational contract automatically;
the runtime race gate includes `internal/jobs/...`. No AWS credentials/budget are
needed. Private local logs are under `/home/ubuntu/.local/state/miles/jobs-nodes-metrics`.

## Downstream / remaining live integration

The [contract](../contracts/jobs-and-nodes.md) is the handoff to W04-PHONG-03/04
for real job lease/renewal and maintenance/exporter ownership; cache/pack consumers
use shared job/node records and per-node results. W04-CHIEN-01 owns identity and
Admin authorization; HTTP composition must use system-admin checks, Job.View and
MAC-bound public cursors, not expose raw store methods or internal page positions.

W05-HUNG-01/02 supplies production startup composition, job executors/scheduling,
immediate remote gate routing, real fan-out/membership and R1/R2 physical drain proof.
This foundation does not claim a GC/scrub/tiering job actually ran, a cluster purge
completed, or a pack/outbox flushed. Unknown pending work is held for reconciliation.
CloudWatch transport has local proof; AWS IAM/ingestion and real multi-node/Edge
availability remain live acceptance. No AWS apply, IAM mutation or charged publish.
