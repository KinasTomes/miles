# Storage statistics and layout accounting

W04-HAI-03. Statistics are admin-only observations, never quota or GC authority.

Logical bytes sum live committed heads in the current active bucket incarnation.
Disabled users still own stored data. Unique bytes count each live (domain, hash)
once: raw length and canonical stored range length respectively. Occurrences and
physical duplicate blobs are different measures. Tombstones/uncommitted uploads
do not contribute logical bytes. Denominator zero produces JSON null.

`bytes_stored` is retained LISTED durable bytes in the dedicated physical bucket,
including current/noncurrent versions, sidecars, journals, checkpoints and orphan
candidates. It excludes billed temporary restored copies, exposed separately as
unknown until a verified billing/expiry source is available. Local caches and
DynamoDB are infrastructure. Revival counters are null without a measured interval
source, never fabricated zeros. Incomplete physical coverage makes bytes_stored
and total_reduction null; listed breakdown remains explicitly partial.

Allocation version is returned with every report. Each live canonical payload
is divided equally among DISTINCT bucket incarnations referencing its identity.
Repeated occurrences do not bias allocation. Verified total pack payload permits
proportional structural overhead attribution. Without a verified full index, all
unknown overhead stays global/unallocated. Sidecar/recovery/noncurrent/orphan
bytes remain global/unallocated in the initial collector. Rational bucket totals
are floored only once; fractional residual remains unallocated. The invariant is
sum(bucket allocated bytes) + global unallocated bytes = global listed bytes.
Bucket unique totals overlap and must NEVER be summed to form global unique.

Global/bucket/layout views share a bounded collected report. Collection timestamps
and consistency=approximate are mandatory: consistent reads do not establish a
multi-table snapshot. A cached report is an observation, not a current authorization
or reachability proof. Auth is checked on every endpoint request before lookup.

Layout shared occurrences sum self(k) for identities with total(k)>self(k).
Repeated chunks inside one object alone are not shared with another object.
Layout exposes metadata placement, not proof that every stored blob was verified.
Corrupt/missing required metadata or inventory errors fail collection; no partial
report is published as a complete measurement. Collection is bounded by record,
reference, inventory and time budgets; no request triggers a full backend scan.

All version inventory uses paginated ListObjectVersions, including marker-only
pages, under the configured bucket. IAM list/version failures are unavailable,
never a fallback to zero or latest-only totals. The reader needs listing access;
PackStore serving paths remain independent of listing permission.

Local/benchmark collection defaults: 10,000 metadata records, 100,000 manifest
references, 10,000 physical versions, 20-second collection timeout, 30-second
refresh and 2-minute maximum report age. Exceeding budgets does not publish a
truncated complete report. Production constructors require explicit providers
and budgets. The service role needs s3:ListBucketVersions on the dedicated
physical bucket; no role/policy is deployed by this implementation.

Layout includes integrity=metadata-only/not-verified. Stored blob/footer CRC,
raw SHA-256 and generation checks remain on verified format/read paths. A layout
200 is never evidence that every physical blob was downloaded and validated.

Resource existence is checked independently of report coverage: a current bucket
or live object created after collection returns unavailable until measured,
not a false 404. Missing buckets/objects retain distinct not-found errors; a
changed head/incarnation conflicts with the cached layout binding.
