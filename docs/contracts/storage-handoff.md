# Storage, manifest and publication handoff

W03-HAI-03. This is the integration entry point for API/multipart and
lease/recovery consumers. Existing contracts remain the source for authority,
serialization and retention. No consumer may obtain publication authority from a
page, cached head, sidecar or this document alone.

## Capabilities available now

| Consumer need | Capability/helper | Preconditions and result |
|---|---|---|
| Physical pack/index publication | PackStore + PackLifecycleStore; write.StorePublisher | Retain writer/resource protection. Register Uploading, write/verify pack and sidecar, then Warm and canonical CAS. ReadSidecar is an optional bounded extension on both adapters. |
| Object page staging | MetadataStore.WriteManifestPages | Object kind 1, unique manifest owner; immutable/idempotent per page, partial batches possible. |
| Part page staging | storage.PartPageStore.WritePartPages/GetPartPage | Part kind 2, distinct namespace, owned copies; Badger verifies exact binary envelope on read. Writer fence is checked on write. No UploadPart head/state CAS is implied. |
| Streaming page construction | manifest.NewStreamWriter/Paginate | Explicit ObjectPage or PartPage; greedy split; offsets local to the stream, bounded to one page of refs. |
| Prepared PUT descriptor | write.PreparedObject.BindHead | Candidate revision zero, matching domain, non-delete; copies complete descriptor/index but does not assign ETag/revision or publish. |
| Persisted descriptor validation | ObjectHead.ValidateManifestDescriptor | Strict complete descriptor; no auto-derivation of omitted counts/index. MetadataStore commit retains its compatibility derivation path. |
| Head binary index | manifest.EncodePageIndex/DecodePageIndex | u64 little endian, <=1024 entries, shape/order checked before allocation; validate resulting head against size too. |
| Bounded object read | ManifestPageReader.GetManifestPage | Follow committed head's index; validate page domain/offsets/counts and canonical chunk metadata/CRC. GetManifest materializes all pages. |
| Logical quota | reserve/adjust/claim/release on MetadataStore | Bucket owner counters; exact ledger op/revision/writer; staging independent of overwrite credit; retry/release once. |
| Unknown commit | GetCommitOutcome, retained ledger/outbox/receipt | ErrNotFound alone is not no-commit proof. Claim/release perform serialized reconciliation. Current head cannot recover an overwritten operation's original result. |
| S3 error mapping | api/s3.MetadataWriteError | ErrBucketNotFound is a distinct wrapped ErrNotFound -> NoSuchBucket; generic missing internal records remain internal errors; unknown remains retryable. |

Part pages deliberately do not share object keys. An object and a part with the
same owner string cannot alias. The new `part-pages` Badger namespace is additive
to local schema 1; its value is exact kind-2 MILESPAG. There is no part head API,
append-seal, upload freeze or automatic page GC in PartPageStore. The multipart
publisher must bind its final part head CAS and enforce its own frozen descriptor,
protection and upload state; W4 integrates that boundary. It must not use the
staging helper as a way to append to a published part or mutate a Completing
upload. Consumers use fresh part_manifest_id for every replacement.

## Multipart integration order

1. Admit current bucket/upload/initiator binding and writer authority. Record the
   upload's immutable domain and use reservation/staging policy for body growth.
2. Build part pages with `manifest.NewStreamWriter(manifest.PartPage, id, domain,
   emit)`; emit calls WritePartPages. Reconcile unavailable writes by reading
   GetPartPage and comparing encoded bytes/digest. Finish and verify descriptor
   before publishing a part head at the expected upload/part revision.
3. Complete first uses multipart.Normalize and Assemble for the canonical input,
   selected frozen heads, response ETag and checksum. Their success is validation,
   not a database commit or storage completeness proof.
4. Read each frozen part's kind-2 pages in selected order. Validate dense page
   sequence, domain, part-local offsets/size and kind; retain frozen part roots.
   Rebase every ref offset onto a single object stream. Feed those refs to a new
   ObjectPage StreamWriter with a fresh manifest_id; never publish a head that
   points directly to part pages or simply concatenates non-greedy page bodies.
5. Prepare/verify all object pages and the immutable R2 payload. Set complete
   descriptor, multipart ETag/checksum and HTTP metadata on the candidate. Final
   CommitRequest binds the same op/input digest, reservation revision and frozen
   Completing revision/fence; Completed state/receipt/counters publish atomically.
6. On an ambiguous result, retain prepared object/part roots and ledger. Resolve
   the original outcome/receipt before reopening, aborting or releasing quota.
   Retry must preserve receipt expiry and return the original response.

The dev storage.Completion model is the minimal final-CAS projection, not the
complete Upload schema. Upload creation/part replacement/freeze/abort orchestration
and production completion stream codecs (kinds 3/4) remain W4 work. Their existing
reference goldens must pass when implemented; PageKey support for those namespaces
does not mean a completion codec has been implemented.

## Lease and recovery integration

WarmGate is a local no-maintenance runtime boundary; restart loses its handles.
WriterFence is checked by Badger/fake but is not a durable distributed admission
or a client credential. Follow R1's shared closure/incarnation/attempt protocol;
wall-clock expiry alone cannot establish that a dispatched write did not commit.

Retain exact PackRef generations and logical manifest roots through publication,
unknown outcomes and all active readers/recovery cuts. Sidecar inventories prove
physical copies exist, not which copy won canonical CAS. GC/refcount work occurs
after the atomic head boundary and needs its own progress/retention conditions.
No helper here authorizes deleting orphan packs just because the current head
does not reference them. Do not use a test fixture's Bootstrap/Set methods as a
runtime mutation API.

Outbox envelopes currently validate Objects/global-policy and stable event IDs.
Producer registration, chunk child-event publication, full effect projections,
exporter claim/mark/seal/checkpoint operations need the shared R2 extension and
reviewed backend integration. Coordination/recovery prototype success is not a
proof that these effects share the Badger/DynamoDB transaction today.

## Native DynamoDB bounds

`internal/metadata/dynamodb` prepares SDK attributes without issuing AWS calls:

* ReferencePageItem emits exactly native keys + body B for kinds 1/2, preserving
  frozen page bytes; full-page tests use maximum-length owner/domain and require
  item <400000 bytes. No extra attributes or base64 JSON ref arrays are stored.
* HeadItem requires a complete published head and packed binary index (<=8192
  bytes). It rejects omitted descriptors and oversized HTTP metadata. Tombstones
  cannot retain the object payload fields.
* OutboxItem encodes only today's dev envelope. OutcomeItem prepares the result
  portion of an op row. The future builder must merge reservation/parent/attempt/
  authority bindings and validate the complete postimage. ReceiptAttributes is
  embedded in Uploads; validating it alone does not bound the whole upload item.
* ItemSizeUpperBound counts UTF-8 names/strings, raw Binary and map/list overhead.
  Integer N is conservatively bounded by decimal length+2 bytes; noncanonical or
  decimal/exponent N is unsupported until a reviewed codec exists. This can reject
  near-boundary numeric items early, never permits a known over-limit item.
* ValidateTransactionPlan checks exactly one action per target, <=100 distinct
  targets, <=400 KiB per complete image and <=4 MiB total. Provide postimages for
  Put/Update and observed complete images for Delete/ConditionCheck, bound to
  version/content predicates. Missing images cannot be substituted with keys.
  The guard cannot infer whether supplied images or expressions are truthful;
  backend reconciliation and expression/fault tests remain necessary.
* SDK wire serialization has a separate no-network seam test demonstrating Binary
  base64 encoding. Native item size is not HTTP request length. Transport/request
  budgeting must also account for wire/expression overhead in the actual W4
  builder; no JSON serialization of Go structs is used as item-size evidence.

No Store/MetadataStore implementation is exposed by this package. Schema and
transaction mapping are in [DynamoDB preparation](dynamodb-adapter.md). Do not
enable multiple gateways merely because these preparation tests pass.

## Validation and handoff owners

See [acceptance evidence](../testing/storage-handoff-validation.md). Hải owns
storage/schema fixes; Chiến consumes multipart/API helpers; Phong consumes indexed
reads, protected roots, outbox/outcome semantics and R1/R2 extensions; Hưng reviews
runtime inventory/session/IAM wiring. Consumer review and live S3 evidence remain
explicit enablement gates. This branch makes no AWS provision/apply, no task-board
Done mutation and no multi-node commit claim.
