# Warm GC and compaction validation

W05-HAI-01. Shared runtime uses explicit target generation job parameters
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
- Compaction copies canonical blobs from an exact Warm generation, including
  unreferenced winners that PUT may still reuse. It uses the immutable repack
  plan, verifies destination pack and sidecar, and swaps locations by revision
  under the job fence and current gate. Source generation remains readable
  for pinned readers. Physical reclamation is a separate operation.
- A real GC job reserves the shared maintenance gate only when its control
  lease is the sole active lease, then takes a fresh mark. It defers when the
  source has a root, canonical location or retention protection. A Warm
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
