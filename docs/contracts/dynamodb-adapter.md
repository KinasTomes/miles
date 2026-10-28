# DynamoDB adapter preparation

W03-HAI-03, **lead-approved W04 design baseline**, recorded in
[PR #35 review and final acceptance gate](https://github.com/KinasTomes/miles/pull/35#issuecomment-6436239260).
The design decisions below are settled; consumer sign-off, implementation and
fault/authority proofs remain separate gates. Implemented here:
`internal/metadata/dynamodb` supplies SDK key constructors and the narrow API
seam. It does **not** implement MetadataStore, deploy tables, execute transaction
recipes, or establish distributed R1/R2 publication authority.

Sources: [architecture §10](../dedup-storage-gateway-architecture.md),
[shared port](shared-storage.md), [commit](chunking-and-commit.md),
[R2](r2-recovery.md), [physical inventory](../../infra/terraform/cloud/metadata-tables.md).
`TestInventoryMatchesTerraform` prevents drift between the 20 table keys and Go.
Environment names come from explicit runtime inventory. No region/account/table
prefix is inferred from an S3 bucket or a logical user credential.

## Physical schema and native attributes

| Table | PK / SK | Encoding and authoritative access |
|---|---|---|
| Users | user_id S | active/status revision; owner limit/used/reserved/staged/active_uploads; Get consistent |
| Buckets | bucket S | bucket_id, owner_id, domain_id immutable per incarnation; active/state/revision |
| BucketGrants | bucket_id S / user_id S | read/write and revision, no inherited grants across incarnations |
| AdminTokens | token_id S | hash, user, scope, state, expiry; no bearer plaintext |
| Credentials | access_key_id S | user, active, flags, encrypted secret; no IAM credentials |
| SystemConfig | config_id S | current/history policy plus tagged adapter namespaces below |
| Objects | bucket S / key S | bucket_id, domain_id, revision, manifest descriptor, HTTP metadata, ETag/checksum, op_id/deleted |
| ManifestPages | manifest_id S / page_no N | exactly keys and body B; kind 1 |
| Chunks | chunk_id S | domain#hash; separate domain_id/hash; canonical PackRef/range/codec/CRC, raw size, location_revision |
| Packs | pack_id S / generation S | lifecycle/state revision/protection/class/access/retention and writer |
| Uploads | upload_id S | binding/domain/initiator, CAS state/fence, input/snapshot pointers, prepared head, Completed receipt |
| UploadParts | upload_id S / part_number N | revision/size/ETag/checksum and immutable part manifest descriptor |
| UploadPartPages | part_manifest_id S / page_no N | exactly keys and body B; kind 2 |
| CompletionPages | completion_id S / page_no N | exactly keys and body B; kind 3 or 4, different owner IDs per stream |
| Outbox | event_id S | history/epoch/entity/action/revision/op/payload digest and pointer, export state/refcount progress |
| RecoveryEpochs | history_epoch_key S | history#epoch, closure/membership/seal/checkpoint descriptors |
| QuotaReservations | reservation_id S | owner/object/upload/op binding, counters, base snapshot, CAS state/deadline/writer |
| Leases | resource_id S / lease_id S | authority/incarnation/revision and lease proof, not a UTC timestamp alone |
| Jobs | job_id S | type/state/progress, worker authority, cancellation/result |
| Nodes | node_id S | heartbeat/drain/runtime identity; TTL is not liveness proof |

Chunk keys use `storage.ChunkKey.PartitionKey`; a hash-only or domain-only key is
invalid. Pack generation remains String. Object keys are exact UTF-8 bytes,
without path cleaning; bucket incarnation is an item attribute/condition, not a
new sort-key prefix. Page bodies remain frozen MILESPAG v1, not JSON chunk lists.
Page index is packed u64 little-endian Binary, at most 8*1024 bytes. Integer
fields use decimal N without float64 conversions. Times use the contract's UTC
representation; a process-local monotonic instant is never serialized as authority.

## Adapter records missing from the original table inventory

Keep the 20 physical tables. Reserve disjoint `SystemConfig.config_id` namespaces
instead of adding attributes to frozen page items or relying on eventual GSIs.
These are internal records, not additional global-policy entities. This placement
is approved; metadata/R2 and infrastructure consumers must confirm the integration
bindings before the W4 writer is enabled.

| Key | record_kind / contents | Badger equivalent |
|---|---|---|
| global-policy | policy_current_v1; revision + current document | policy-current and current policy |
| global-policy/revision/{20 digit revision} | policy_history_v1; immutable revision/document | policies |
| storage/op/{32hex op_id} | storage_operation_v1; op/logical-parent/history/incarnation/attempt binding, reservation_id if any, input digest, outcome and original committed result/event ID | reservation-ops + event-ops + outcomes |
| storage/manifest/{1 or 2}/{owner} | manifest_owner_v1; page kind/domain/writer, revision, Building/Closing/Verified/Published, descriptor/digests and publishing op_id | manifest-seals plus bounded preparation state |

`Reserve` creates the op binding and ledger together. Final commit updates that
same op row, not a second competing Put; child chunk mutations have their own
mutation IDs. Policy publication creates a terminal op record without an object
reservation. Original outcome/head is retained after overwrite/delete. Reads by
op_id use this exact base key, consistent, never a GSI or current head inference.
Missing outcome alone is not no-commit proof. No TTL removes active or protected
terminal rows. Retention/cleanup follows R2 and the request/receipt windows.

The operation result, owner-seal and ledger effects belong to the owning R2
bundle/projection. They cannot be exported as `SystemConfig/global-policy`
mutations merely because they share its physical table. Policy queries use exact
keys/record_kind; recovery inventories must distinguish these records. Historical
restore retains original outcomes only as evidence and starts new authority;
it must not reopen old attempts in the new history. The current dev OutboxEvent
is an envelope and does not yet implement this full projection/registry protocol.

## Index decision and read API

The approved W04 baseline keeps **exactly 20 base tables, no GSI/LSI and no TTL**.
The Terraform inventory already has this shape; no resource change or apply is
needed. TestInventoryMatchesTerraform fixes the count/key map;
TestTerraformBaselineHasNoIndexesOrTTL checks all data-module `.tf` files,
including dynamic index/TTL blocks, and the resource's exact local.tables binding.
It is a lexical regression guard, not a replacement for Terraform validation.
Any later table/index/TTL change requires reviewed migration and guard updates.

* Auth, head, outcome, ledger, chunk/location and policy: exact GetItem with
  ConsistentRead=true; use TransactGetItems when a bounded coherent bundle is
  needed for reconciliation. Separate consistent reads are not a snapshot.
* LIST objects: consistent Query of Objects by bucket, begins_with key prefix;
  skip tombstones and wrong-incarnation rows; preserve the last evaluated key,
  delimiter state/options/principal/incarnation in the internal cursor. Query
  until the logical page is full or exhausted, including empty filtered pages.
* Accessible buckets, pack-state candidates, outbox export candidates: bounded
  paginated base Scan and explicit authoritative authorization/state checks.
  These scans are not cross-page snapshots. Epoch completeness requires closed,
  stable membership and complete consistent base enumeration under R2.
* Future PacksByState or Outbox export GSIs are candidate optimizations requiring
  a reviewed Terraform/index migration and pagination contract. GSI results can
  never authorize publication or prove an epoch complete.

BatchGetItem: deduplicate keys, split at 100, keep ConsistentRead per table,
merge unordered results by complete key, retry UnprocessedKeys with bounded
backoff/deadline. Only a key omitted from a **processed** response is absent.
Exhausted throttle/transport retries return ErrUnavailable; never report those
keys as missing. Response/page limits require pagination even below 100 keys.
Immutable pages use conditional transactional puts, not blind BatchWriteItem
overwrites. All request contexts and retry budgets come from the admitted caller.

## Manifest validation cannot be copied from Badger

Badger validates pages/chunks in its publication snapshot. A DynamoDB transaction
cannot read every chunk of an arbitrarily large manifest. The W4 preparation
protocol must provide equivalent prerequisites before the bounded head commit:

1. Register unique owner/kind/domain and active writer/protection as Building.
2. Every new page transaction condition-checks that owner is still Building,
   its writer/revision and shared publication authority. Write absent page only;
   an identical retry reads/verifies bytes, including after Closing/Published.
3. CAS Building -> Closing serializes with every page transaction, freezes page
   membership and prevents late appends. Reconcile ambiguous page writes before
   claiming completeness. Enumerate all pages, including unexpected trailing
   pages, and validate codec/domain/offset/greedy split/chunk raw sizes/index.
4. Persist Verified descriptor/digest/revision under retained logical roots and
   resource protection. Chunks/pages cannot be GC'd during validation or between
   verification and publication. Missing protection/closure proof fails closed.
5. Final head commit atomically consumes this exact Verified owner revision and
   marks Published with its op_id. Reusing owner for a different head conflicts.
   Empty manifests follow the same owner lifecycle with zero pages.

This is an integration obligation, not a claim that R1/manifest owner protocol
has been implemented by the key helpers. Verification outside a transaction
without frozen membership and retained dependencies is insufficient.

## Transaction recipes

Build one TransactWriteItems request and validate unique table+key targets,
item/action limits and retained dependencies before dispatch. SDK seam is API in
`internal/metadata/dynamodb/api.go`. Table resolution is explicit. Never split
head/quota/ledger/outbox publication into multiple visible transactions. The same
atomic boundary includes the durable operation outcome and manifest-owner
Verified->Published transition; Complete also includes upload state/receipt.
All effects share the admitted authority/conditions. Coalesce predicates for the
same target into its single action. If item/action/aggregate or wire budgets do
not fit, fail closed or rework preparation; do not publish a subset first.

| Operation | Atomic effects and conditions |
|---|---|
| Reserve | Check bucket incarnation/owner, head base snapshot, writer authority; create reservation and unique op binding; update owner reserved/staged with current counter predicates |
| Adjust | Check Active ledger op/revision/writer/deadline, replace reservation counters and owner totals atomically; exact retry does not reapply delta |
| Claim/release | Reconcile coherent op/ledger/event/receipt first, condition exact ledger revision and closed old authority; committed or unknown cannot release |
| Canonical insert/CAS | Conditional chunk create or expected location_revision update; Warm destination pack; writer/history/attempt authority; matching child outbox and op outcome |
| Commit/Delete | Head/tombstone, owner counter delta, terminal reservation, matching outbox, operation outcome, Verified->Published seal for non-delete; current auth/bucket/grant and authority predicates |
| Complete | Commit bundle plus Uploads Completing revision/fence -> Completed and receipt; consume active-upload count once, preserve original receipt expiry |
| PutPolicy | CAS current + immutable history + op/outbox, admin credential/user and authority; action=entity_put; exact no-op does not create history/outbox |

For Commit/Delete, include credential active, user active, exact active bucket ID
and domain, and current grant when caller is not owner. If caller=owner, fold user
active into the same Users counter Update. Do not also ConditionCheck that item.
Merge any repeated authority/registry target predicates into its single action.

Read current head at expected revision: H=old size (0 for absence/tombstone),
D=new size-H. Read owner counters and compute new Used/Reserved/Staged with checked
integer arithmetic, including other reservations. DynamoDB condition expressions
do **not** accept arbitrary `used+D+(reserved-r)` arithmetic: compare the observed
counters or a dedicated counter revision in the same Update, then SET checked new
values. Conflict requires reconciliation then a fresh attempt/snapshot; never
reuse a token with changed expressions/values.

Head condition for expected>0 includes bucket_id and revision. For expected=0,
logical absence means no row **or** an old-incarnation row. The replacement may
condition `(attribute_not_exists(bucket) OR bucket_id <> :admitted_bucket_id)`
only together with an exact active Buckets condition and old-root retention;
the old incarnation's revision/size is not charged to the new owner. A malformed
row lacking bucket_id must fail validation, not be treated as an old incarnation.

Unknown: retry identical token/input/attempt; reconcile stable op, ledger, event
and optional receipt before error classification or cleanup. Token format is
mutation32hex+attempt4hex (36 characters). The 10-minute SDK idempotency window
does not replace durable op records; later retry still returns the original
committed result after another overwrite. Failed permission/bucket conditions
retain their specific error (ErrBucketNotFound -> NoSuchBucket), while missing
internal records remain internal faults. Missing outcome under unclosed authority
cannot prove no-commit.

R1/R2 add gate/history/producer/attempt/root conditions and record effects beyond
the old ten-action example. Count the actual request including those rows. Stop
on >100 distinct actions, >4 MiB item total or oversized item; include a separate
wire-payload check for encoded request overhead. No multi-node readiness follows
from a passing request-shape or fake transaction test.

References: [transaction API](https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_TransactWriteItems.html),
[batch read](https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_BatchGetItem.html),
[read consistency](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/HowItWorks.ReadConsistency.html),
[item sizes](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/CapacityUnitCalculations.html).
