# Warm GC and compaction validation

The shared runtime uses explicit target generation job parameters
`pack_id` and `generation`. A system administrator submits
`gc` or `compaction` through the existing Jobs API. A GC dry run reports
observed roots, live bytes and canonical locations without physical dispatch.
An exact physical generation without a metadata row is reported as missing
metadata proof in a dry run.

## Protection and publication

- Mark observes live object heads of the current bucket incarnation,
  multipart part manifests and prepared Complete manifests, unresolved
  publisher packs, R1 pins and supported recovery generations. Published
  part bytes and open publisher counts must reconcile with the upload record.
  Refcounts count committed occurrences; live bytes count each canonical blob
  once. These measurements are never a delete condition.
- Compaction copies canonical blobs referenced by live heads or unfinished
  uploads from an exact Warm generation. Unreferenced winners stay on the
  source until a GC job retires them under the maintenance gate. It uses the immutable repack
  plan, verifies destination pack and sidecar, and swaps locations by revision
  under the job fence and current gate. Source generation remains readable
  for pinned readers. Physical reclamation is a separate operation.
- A real GC job reserves the shared maintenance gate only when its control
  lease is the sole active lease, then takes a fresh mark. It defers when the
  source has a root or retention protection. Under the gate it replaces dead
  canonical locations with revisioned tombstones and journals them atomically.
  A later PUT can publish the same identity with a higher revision. A Warm
  source first becomes Retiring, closing new admission and starting the
  recovery window. A later job requires 24 hours plus 10 minutes and a
  published cut after retirement, as well as a fresh proof of no roots.
  An Uploading orphan requires a released matching producer fence.
- Sweep binds exact version IDs to a durable intent and persists Deleting
  before a one-shot dispatch. Pack and sidecar versions are separate SDK
  operations with retries disabled. A partial or unknown result preserves
  Deleting and Maintenance. Only authoritative acknowledgement of all sends
  or a trusted terminal proof can settle that intent. The runtime currently
  has no live terminal-proof provider.

## Scope and reproduction

Badger supports the mark projection. Shared compaction and destructive jobs
use DynamoDB R1, a typed recovery baseline, and a versioned pack bucket.
Version deletion requires `s3:ListBucketVersions` on that bucket and
`s3:DeleteObjectVersion` on its pack and sidecar prefixes. Existing cloud
role policies and the VPC endpoint policy must grant those permissions before
physical sweep is exercised in AWS. Unversioned or ambiguous objects and
physical orphans without metadata proof are held.

Run the lifecycle and observation suites against Badger and pinned
DynamoDB Local, the S3 dispatch tests, the recovery retention tests and
the shared Cloud/Edge runtime test against DynamoDB Local and Moto.
Use `MILES_DDB_LOCAL_REQUIRED=1` and explicit loopback test endpoints.
The native lost-ACK fixture loses the sidecar acknowledgement after the
pack acknowledgement and checks that retry neither sends another delete
nor opens the gate. The compaction fixture holds a read pin and compares
source and destination data after the canonical swap. Local fixtures do
not establish live AWS timing or terminal request isolation.

## Native object flow regressions

`TestNativeWarmGCObjectFlows` uses real object services on separate gateway
instances with shared DynamoDB Local tables and the S3 pack adapter against a
versioned Moto bucket.

- PUT pauses at its canonical chunk lookup while holding a writer lease.
  GC defers, the exact pack and sidecar versions survive, and the resumed PUT
  reuses the winner and reads back byte for byte. A separate PUT after retirement
  writes a different generation with a higher canonical revision and reads back
  the original payload.
- GET pauses after preflight with the exact generation pinned. Same-domain
  CopyObject pauses after it binds its source head, at the destination lookup.
  A peer overwrites the source, GC defers, and compaction relocates the shared
  chunks while the captured generation remains available. Resumed GET and the
  copied destination match the original payload and SHA-256. A new GET reads the
  overwritten payload.
- After both requests release their leases, GC retires the old generation.
  The fixture advances wall time past retention, closes and seals the epoch,
  and publishes its checkpoint through the runtime executor. Sweep deletes the
  old pack and sidecar versions through the S3 adapter. Version inventory and
  GETs confirm that the current generation and copied destination survive.

The fixture uses channels for request ordering, a controlled wall-clock offset
and a test-only SDK transport wrapper for two pause points. It invokes GC and
compaction executor steps with real claimed jobs and R1 leases. The post-retirement
cut and checkpoint are produced against the emulators. The clock offset models
elapsed retention time.

The Compose fixtures pin DynamoDB Local 3.3.0
(`sha256:d89f8fcc6b1a39cb35976c248ed42a28c66ae00dc043099210f5571e42648ab4`)
and Moto 5.1.15
(`sha256:8860b18917e1ee4d067df19a406ae95dc26037e6965d5b499e1cffd33755b914`).
Use these commands on Linux with Go 1.27.2.

```bash
docker compose -f deploy/local/compose.yaml -p miles-gc-object up -d dynamodb s3
export MILES_DDB_LOCAL_ENDPOINT="http://$(docker compose -f deploy/local/compose.yaml -p miles-gc-object port dynamodb 8000)"
export MILES_RECOVERY_S3_ENDPOINT="http://$(docker compose -f deploy/local/compose.yaml -p miles-gc-object port s3 5000)"
export MILES_DDB_LOCAL_REQUIRED=1 MILES_RECOVERY_S3_REQUIRED=1
go vet ./internal/app
go test -race -v ./internal/app -run '^(TestNativeWarmGCObjectFlows|TestNativeSharedCloudEdgeRuntime)$' -count=2 -timeout=5m
docker compose -f deploy/local/compose.yaml -p miles-gc-object down
```

Local Linux results were PASS for both tests on both race runs. The app
package completed in 60.584 seconds and `go vet ./internal/app` passed.

The `dynamodb-contract` CI job runs the app package with `-race` and both
required-endpoint flags. With either corresponding flag enabled, a missing
endpoint fails fixture setup.
