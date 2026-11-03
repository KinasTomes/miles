# Shared runtime validation

## Local evidence

The native test `TestNativeSharedCloudEdgeRuntime` composes two Cloud nodes and
one Edge node with distinct local caches against the same ephemeral 20-table
DynamoDB Local inventory and Moto S3 bucket. It uses the production shared
authority, producer, recovery catalog, engine, credential sealer and policy wiring.
Only UTC and AWS credentials/endpoints are test substitutes; these constructors
are not reachable from production startup configuration.

The test checks shared credential decryption, bucket/cache-policy provisioning,
alternating writes and reads from all three nodes, Unicode/percent/plus/slash keys,
multipart create/abort and cross-node UploadPart → Complete → Range read-back.
It requires zero retained active/resolving leases after the operations and native
producer events. Each node's close also verifies final journal export. The CI
native metadata job supplies both emulators and requires this test to run.

Run the fixture-scoped race suite with explicit loopback addresses:

```bash
MILES_DDB_LOCAL_ENDPOINT=http://127.0.0.1:8000 \
MILES_RECOVERY_S3_ENDPOINT=http://127.0.0.1:5000 \
MILES_DDB_LOCAL_REQUIRED=1 MILES_RECOVERY_S3_REQUIRED=1 \
go test -race -count=1 -timeout=10m ./internal/app ./internal/metadata/dynamodb \
  ./internal/object ./internal/coordination ./internal/readpath ./internal/recovery
```

Additional checks cover invalid/stale/unapproved/unsynchronized chrony tracking,
clock jumps during observation, native credential cache refresh, helper revocation,
static-key/fallback rejection, private listener validation and tunnel failover.
Terraform mock tests cover the separate certificate-bound Edge role/profile,
private ASG bootstrap and the existing pause/resume/security contracts. They do not
validate a real certificate, IAM grant, host, AMI or network route.

## Live evidence

The [native run record](cloud-edge-live-validation.md) records the earlier
unknown-commit blocker, its correction and the passing AWS-only functional rerun.
It includes ALB path/Host preservation, a separate AWS Edge, Internet HTTPS,
same-process Roles Anywhere refresh, owner SSH failover, physical ASG replacement,
in-flight drain and the durable hourly seal. Its cleanup section is the authority
for teardown status and retained resources. It also records an unresolved stalled
upload observation; the two-GiB fleet does not establish sizing or performance.
Use the [live acceptance plan](../../deploy/shared-acceptance-plan.md) for future
runs with fresh resource, IAM and cost approval. Emulator results above do not
substitute for that separate native evidence.
The existing native TLS proxy test covers transport/SigV4 with synthetic handlers;
it is complementary to native storage tests and is not combined live evidence.

The runtime genesis base has a separate versioned installation schema. Rebuild
consumers must load it before applying exported events. Checkpoint replay,
physical retention/GC, cluster cache/job fan-out and full performance/cost acceptance
remain independent consumers; this implementation does not claim those results.
