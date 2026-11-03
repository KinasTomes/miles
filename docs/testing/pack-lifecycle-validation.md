# Pack lifecycle validation

W05-PHONG-02, baseline `765593e`. Backlog checked 2026-11-03: Doing/Ready;
W04-PHONG-02, W04-PHONG-03, W05-HUNG-01 and W05-HUNG-02 are Hard Done.

## Checklist implementation

1. System-admin GET `/admin/v1/packs` and `/packs/{id}` list exact generations,
   state/class/state revision, bytes, unique head-live stored bytes/ratio,
   protection, minimum retention and restore expiry. Filtering and signed,
   principal/resource/filter-bound cursors use the existing Admin infrastructure.
   This bounded non-atomic head-live projection is explicitly NOT GC/R2 proof.
2. Shared runtime installs explicit `tiering`, `pack_restore`, `pack_promote`
   executors into the existing Jobs runner. Restore/promote HTTP actions require
   an exact generation through a targeted metadata lookup, independent of global
   inventory availability or scan budgets; other gateways read the same durable job. Pinned global
   policy selects tiering through the existing pure evaluator. Automatic periodic
   cold movement is not enabled. GLACIER_IR remains readable while state=Cold.
3. Native job/node/gate/fence predicates accompany publication, with a fresh UTC
   bound after slow reads. Final SDK guards execute after HEAD/body buffering;
   mutable SDK calls have no hidden retries. Drain, cancellation, recovery closure,
   stale heartbeat, expiry and closed fences reject new dispatch/publication.
   Known pre-closure physical work can settle truthfully; unknowns retain protection.

## Durable states and safety

`TestPackActionsDoNotDependOnClusterInventory` covers restore/promote admission
with unavailable, over-budget and absent inventory collectors. Exact generations
still create jobs; missing generations remain 404 and create no job. Workers keep
their bounded head-live collection when lifecycle planning needs it.

Intent: Prepared -> Ready -> one-shot Dispatched -> Applied/Unknown -> Done.
The Ready registration and send claim are separate. A lost claim ACK never grants
another mutable send. Failure to persist Unknown cannot leave a reusable send
permission. Applied means a known synchronous SDK acknowledgement; for restore
it proves acceptance, not readiness. Restore waits until a positive backend expiry
exceeds the conservative UTC upper bound. A positive ongoing restore can settle
acceptance after an unknown response, but does not grant payload readability.
Persisted Restored state can remain after expiry; observational readable becomes
false when its deadline passes, and payload reads independently verify physical
state/UTC. Promotion requires a readable source; archive generations must be
restored first. IR restore is a no-op with `restore_not_required` progress. A cancelled accepted
restore can resume observation under a new explicit job without reissuing RESTORE.

Tiering reserves exclusive R1 maintenance only with the requesting control lease
as the sole active producer, no source pins, expired conservative protection and
minimum retention. Policy/live projection is evaluated under the reserved gate;
unknown/denied inputs do not dispatch. Transitioning is persisted before COPY;
success verifies class/size, installs a conservative 90-day minimum retention and
reopens only the owning gate revision. Old generation bytes/sidecar are retained.

Unknown mutable transitions cannot finish from HEAD/class, timeout or expiry.
`SharedAuthority.LifecycleTerminal` is an internal trusted reconciliation port:
the provider must observe exact original terminal outcome AND worker/request
isolation rows through Observe in the same CAS. Default runtime provider is nil,
so ambiguous COPY remains protected in Maintenance until that proof is supplied.
Positive applied proof finishes Cold; proven terminal no-effect restores prior
state and reports `transition_not_applied`, never successful movement. An expired
unknown owner cannot publish; full recovery/isolation is still required before
transferring it. There is no public force-complete/boolean-proof API.

Promotion pins the source and copies the bounded current head-live canonical plan
into a distinct immutable STANDARD generation. Compressed stored bytes retain
their codec after full CRC/decode/SHA verification. Plans are immutable 32-chunk
pages (up to the format's 16,384 entries), root-digested, with a small native
header. Exact destination bytes and sidecar are verified before warm publication
and canonical location CAS. A concurrent canonical winner is skipped, never
rebased over; some output blobs can therefore be unreferenced. Source generations
are never deleted. Retry of an immutable PUT requires the same plan/body digest.

Jobs keep their unknown-step barrier. Only authoritative native Done/Skipped/
NotApplied or known Waiting restore outcomes permit pending-step reconciliation
before worker transfer; a generic caller cannot clear it. Expiry cleanup checks
this proof before closing/releasing the old physical job authority: missing intent
does not prove preflight quiescence. The runner may resume
this node's pending reconciliation while readiness is suppressed, including its
own Maintenance gate, but each new SDK send/CAS still checks current modes. Settled nonterminal
checkpoints yield their progress lease between polls, so waiting restores/deferred
tiering do not hold active control leases that block each other. Unknown steps
never yield.

## Journal handoff to checkpoint/rebuild

Additive event entity `PackLifecycle/entity_put` uses the unchanged v1 EventID
array/hash and retained-input digest contract. A native bundle atomically records
Packs state, `PackLifecycleFacts` (restore/minimum-retention times), action outcome
and ProducerScope. State facts follow pack StateRevision; action revisions may
skip internal coordination-only revisions and are not assumed contiguous.
The final promotion action also retains destination PackInfo/protection. Existing
PackProtection remains its independent monotonic revision domain.

W05-PHONG-03 MUST include these projection namespaces/retention facts in rebuild,
retain required action/outcome artifacts, and reconcile ambiguous physical work
before readiness. Immutable staged plan pages are coordination data, not new
recovery artifacts: active unknown control leases prevent sealing through an
unfinished promotion. No complete checkpoint/rebuild or deletion/GC proof is
claimed here. `ProtectPackStore` and ordinary destructive maintenance stay disabled;
this task never calls DELETE for lifecycle work.

## Tests and limits

- `TestPackAdminInventoryGenerationFiltersAndActions`: actual Badger/Admin HTTP,
  all generations, precise filters, zero live ratio, cursor binding, system-only
  permissions, unknown generation, strict bodies and shared job records.
- Operational token/user revocation after admission includes pack restore.
- `TestPhysicalGuardAfterSlowHeadAndNoHiddenCopyRetry` and
  `TestPhysicalGuardRestorePutAndDelete`: SDK guards after slow reads/buffering;
  no COPY/RESTORE/PUT/DELETE after permission loss; one mutable SDK attempt.
- Native DynamoDB lifecycle cases cover both cold classes, restore acceptance vs
  ready, IR no-op, promotion and source retention, exact terminal/isolation proof,
  no-effect outcome, immutable lost ACK, send-claim/unknown-marker lost ACK,
  retained generations, owned Maintenance readiness, stale heartbeat, closing,
  cancelled accepted restore resumption and proven terminal job takeover.
- `TestYieldLifecycleAuthorityOnlyAfterSettledCheckpoint`: known accepted waiting
  steps yield only after joined work and checkpoint; unknowns retain authority.
- `TestLocalLifecyclePagedPromotionPlan`: 80 zstd chunks, three immutable plan
  pages, all canonical rows moved and compressed codecs preserved.
- `TestNativeSharedCloudEdgeRuntime`: public pack list/show/promote, three shared
  Cloud/Edge instances, actual AWS SDK S3 adapter on Moto, cross-gateway job status
  and unchanged durable read bytes. Fake physical seams model ambiguous COPY and
  asynchronous archive timing; they are not AWS timing/consistency attestations.

Collector budgets: 10 seconds, 10,000 records and 32,768 manifest references in
shared runtime; exceeding limits returns unavailable, never truncated live data.
Pack format bounds and per-page native transactions remain in force. Linux
Go 1.27.2; pinned DynamoDB Local 3.3.0 and Moto 5.1.15, disposable project
`miles-lifecycle-contract`, explicit loopback endpoints. No AWS resources, billed
cold operations, live retention expiry or real restore tier latency were tested.
Week 6 still owns live Cold acceptance and a reviewed production terminal-proof
provider. Default nil reconciler never substitutes HEAD/time for terminal proof.
