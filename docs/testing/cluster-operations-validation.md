# Cluster operations validation

## Local proof

The focused regression suite covers:

- Admin role/schema enforcement, shared job views, cancellation through another
  HTTP server, cursor handoff/binding and rejection of missing executors.
- Badger CAS/fence worker tests: exclusive periodic creation, takeover at a safe
  checkpoint, retention of unsettled physical work, cancellation at a settled
  checkpoint, heartbeat-loss cancellation and joining of an active step.
- DynamoDB codec compatibility for older Nodes rows, empty optional per-node
  results, and runnable Scan pages whose filter returns no items but still has
  a native continuation key.
- Shared runtime integration on DynamoDB Local + S3/Moto: three independent
  Cloud/Cloud/Edge compositions; purge and per-node partial results; cancellation
  from another gateway; target-gate acknowledgement; Edge gate-only completion;
  Cloud incomplete drain never reporting a completed physical flush.
- Upload discovery with empty native pages, purge expiry boundary and open
  publisher closure, repeated cleanup without duplicate quota release, frozen
  completion preserved for recovery, and composed Cloud/Edge dry-run/real purge
  jobs on DynamoDB Local and S3/Moto.
- Lifecycle-controller process/ALB proof, manual drain adopting ASG heartbeats,
  private invocation-bound markers, timeout/failure withholding completion and
  node-result reconciliation after an UpdateItem acknowledgement is lost.

Run focused checks with:

```bash
go test -race ./internal/jobs ./internal/api/admin ./internal/metadata/badger ./internal/observability ./internal/app
python3 -B -m unittest discover -s scripts/tests -p test_deployment.py -v
make check
```

Native adapter coverage requires disposable loopback DynamoDB Local and S3/Moto
fixtures. Configure `MILES_DDB_LOCAL_ENDPOINT`, `MILES_DDB_LOCAL_REQUIRED=1`,
`MILES_RECOVERY_S3_ENDPOINT` and `MILES_RECOVERY_S3_REQUIRED=1` as in the native
contract CI job, using test credentials only:

```bash
go test -race -count=1 -timeout=10m ./internal/metadata/dynamodb ./internal/metadata/badger ./internal/storage/contracttest ./internal/readpath ./internal/app
```

## Native AWS acceptance

These tests do not prove live CloudWatch ingestion, EC2 invocation ownership,
private endpoint reachability or actual ASG replacement. The approved
AWS-only run is recorded in [live validation](cluster-operations-live-validation.md).
Its release/hash and native evidence cover:

1. Jobs list/create/get/cancel through either Cloud node and Edge, with exact
   policy revision, owner transfer, stable checkpoints and cancellation proof.
2. Cache purge membership and per-node results, including unavailable/replaced
   nodes and cancellation while a child is active.
3. Manual Cloud drain overlapping a real request and ASG termination: handler
   admission closed even if ALB fails open, readyz503, timely hook heartbeats,
   successful bytes/read-back, invocation-bound flush and ALB-unused proof.
4. Edge gate-only behavior and actual `Miles/DSG` metric ingestion using its
   credential provider, including dispatch failure and exporter failure cases.
5. Evidence export and cleanup of that run's synthetic data, temporary grants
   and resources within the separately approved time/cost envelope.

The orchestration foundation and its installed executors form the accepted
scope. Future GC/tiering/checkpoint/scrub implementations supply their executors
and maintenance-compatible authority, then prove their business operations.
An uninstalled executor returns HTTP501 before enqueue. Native physical success
for those future operations is owned by their integration suites.
