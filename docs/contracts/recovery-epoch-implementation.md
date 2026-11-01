# Recovery exporter, epoch closure và seal

Extends [R2](r2-recovery.md), [shared R1](../testing/shared-r1-validation.md)
and the existing producer/outcome registry. Scope is export, exact epoch closure
and seal. Checkpoint replay, rebuild and24h physical retention remain separate
implementations. No new table, GSI, dependency or public HTTP API.

## Entry points and authority

`recovery.ProvisionHistory` is explicit provisioning: the operator supplies an
audited immutable genesis and `Catalog`. It verifies genesis and history catalog,
conditional-creates the initial active-history slot, then initializes metadata.
It cannot rotate an existing selected history. It does not derive a genesis from
Scan/live heads or assert that an arbitrary supplied base is a complete checkpoint.

`recovery.Worker` receives the existing native DynamoDB store as both metadata
and retained payload source, an `ImmutableStore`, the selected catalog, a shared
Clock and an already acquired R1 `kind=recovery` fence. `Run` supervises:

- export immediately and each60s after the preceding attempt;
- renewal every15s, using the independent validated-UTC R1 provider;
- recovery cut on the persisted hourly schedule, or resume an interrupted shared cut;
- cancellation/join before the caller releases its job lease.

The runtime/operational executor owns job creation, claim, cancellation and final
job status. Native `journal_flush` records accept recovery-kind fences; GC and
other mutating job types do not. `Flush` and `CloseAndSeal` are bounded operations
available to that executor. `Observe` reports finite result codes including
`close_timeout`; timestamps/metrics cannot grant authority or promise RPO60s.

Recovery leases cannot publish object/control/location changes, acquire pack
pins or begin physical R1 actions. They may be admitted while `RecoveryCut` is
active, as may readers. New writer/control leases receive `ErrR1Busy`; existing
admitted producer generations can finish in their bound epoch. Other global
Closing/Maintenance gates keep their original behavior.

## Native records and exact closure

Existing table/key inventory is unchanged. New records use native attributes:

| Record | Purpose |
|---|---|
| `SystemConfig/coordination/r1/current` | `RecoveryVersion=1`, enabled flag and exact recovery close ID, in addition to the existing history/incarnation/epoch/gate |
| `Leases(recovery/producers/H/E, hex(lease_id)/fence)` | Retained admission for each writer/control generation; terminal closure revision written with the shared authority closure |
| `Leases(recovery/mutations/H/E, event_id)` | Immutable membership attachment of the existing producer mutation registry; full matching event, written in the **same** transaction as outcome/outbox/business effects |
| `RecoveryEpochs(H/E)` | Closing/proof cursor, producer and event digest chains/counts, Closed/cut_at, owner fence/revision, Sealed pointer |
| `SystemConfig/recovery/state/H` | Genesis binding, contiguous `NextToSeal`, last published seal pointer/revision, and `NextCutEpoch`/`NextCutAt` schedule |

Producer slot reuse cannot erase old membership. A terminal producer receipt is
immutable; Pending actions prevent terminal certification. Closing freezes new
producer admission before consistent base Query of the retained cohort. Each
page verifies native terminal receipts and advances cursor/digest in one CAS.
No `Active==0`, expiry, node heartbeat, timeout or absent-outcome observation is
used as a quiescence certificate.

After the entire cohort is terminal, mutation membership is fixed. The next
paged phase checks **every** registered event against consistent base Outbox,
compares the complete source envelope, and accumulates the ordered event digest.
At most24 members are consumed per transaction (at most52 actions including the
Outbox checks, epoch, gate, schedule and worker authority). Existing wire/item/transaction
budget checks remain in force. The final bounded transaction consumes the
persisted generation-bound progress, records E Closed/cut_at and opens E+1.
It does not loop over all producers in one transaction.

`storage.RecoveryDigest` is SHA-256 over a domain tag, previous hex digest, a
separator and the typed JSON source. Closed retains the exact expected event
chain derived from atomic mutation attachments. Seal independently enumerates
all consistent **base Outbox** pages, including empty filtered pages, and must
reproduce that exact chain/count. A skipped member, altered source, extra member
or missing retained payload blocks completion. Digests summarize already verified
leaves; caller-supplied counts/digests alone cannot create a closure certificate.

## Durable hourly cadence

The schedule belongs to the history/epoch, independently of any worker lease.
Provisioning sets `NextCutEpoch=0` and `NextCutAt` one hour after validated UTC's
upper bound; an identical provisioning retry preserves that timestamp.
`RecoveryCutDue` returns the scheduled epoch only when fresh validated UTC's
lower bound reaches the deadline. A local wall-clock change cannot trigger it.
The returned epoch stays bound to the observation if another worker advances
the gate before dispatch.

The same transaction that confirms Closed and opens E+1 advances the schedule
to that next epoch, one hour after `cut_at`. Abort, unavailable/unknown outcomes
and worker restart do not postpone an unclosed epoch. An overdue successor
therefore attempts the existing cut immediately, rather than waiting another
hour. Recovery leases retain their original hard deadlines: the runtime must
join and replace an expired lease before restarting the worker, and cannot
extend publication rights from the schedule itself.

## Budget, crash and takeover

Close uses a120s process-local monotonic budget; the native adapter also checks
the persisted generation's120s budget against validated UTC on each proof step.
Equality expires the budget. A healthy producer is neither killed nor reclaimed
to force closure. Before Closed, abort CAS opens the same epoch and invalidates
old proof generation; terminal authorities and unknown resource roots stay closed.
Closed cannot be undone. A lost final ACK is reconciled from the durable epoch
and gate; it cannot reopen an already advanced epoch.

A successor retains the close ID and proof cursor. It cannot replace a live
owner. An expired recovery owner is closed only when validated L>=expiry+5s;
its old publication conditions serialize with that closure. With no physical
rights/pins/Pending work, its R1 protection is released, then a separate CAS
transfers epoch ownership. A reused lease slot must already have passed the
existing Released/generation protocol. All new side-effect dispatch rechecks
worker authority; native progress/pointer transactions include the same fence.
A late old immutable S3 write can only match the verified original bytes.

## Immutable wire records

New canonical v1 schemas live in `internal/recovery`; their byte example is
[epoch-seal.json](../../tests/fixtures/recovery/epoch-seal.json). Existing
256KiB metadata, safe integers, canonical strings and strict decoding remain.
No pack/page binary kind changes or claim of full RFC8785/format freeze.

| Path | Body |
|---|---|
| `journal/H/E/event/pages/N.json`, `descriptor.json`, dependencies | Existing exporter envelope/pages and verified binary attachments, always Standard |
| `journal/epochs/H/E/membership/{sha}.json` | History/epoch, level/count, up to128 ordered members or child references; bounded incremental tree, max level8; explicit empty leaf for an empty epoch |
| `journal/epochs/H/E/seal-{sha}.json` | History/epoch/cut/close ID, producer closure proof hash/count, expected event hash/count, membership root, predecessor hash; epoch0 binds genesis hash |
| `journal/epochs/H/E/complete.json` | Fixed immutable discovery slot binding the exact seal key, length and SHA-256 |

Seal and marker exclude worker attempts/fences/processing times, so takeover
produces identical bytes. Every object uses conditional-create and exact readback;
conflicting bytes are never overwritten. Only after all event pages/dependencies,
membership, seal and marker verify does metadata CAS publish Sealed and advance
the contiguous pointer. Export progress is separate and excludes attempt ownership.
A completion marker may survive a lost DB pointer ACK; identical retry can finish
publication. Open export progress, Closed state and an orphan seal without marker
are insufficient.

`VerifyCut` uses only immutable S3 artifacts: verifies genesis/catalog, every
contiguous marker/predecessor, membership count/order/hash, descriptors, event
pages and attachment contents. It cannot skip a missing/corrupt link or mix
histories. It proves journal completeness, **not** a complete physical recovery
installation or a GC permission.

## Downstream and activation

The GC/compaction implementation must keep physical roots until its independent
GC/retention conjunction passes. Checkpoint/rebuild integration adds replay, full
physical coverage/retention and recovery security/readiness. No membership, payload, epoch or root is deleted
here; Sealed alone does not grant cleanup or remove existing retention guards.

`cmd/miles` runtime still lacks the production DynamoDB/time/job composition;
this change provides the real worker/backend and executable local integration,
without replacing that runtime with test authority. Production composition can
call `Worker.Run` with the existing shared store, verified clock and selected
history. The local export/close/seal acceptance is independent of that deployment.

Activation requires a fresh R1 history with RecoveryVersion1 and strict producers,
plus audited genesis/catalog provisioning. Legacy R1 gates without retained
cohorts are refused; there is no online backfill or auto-upgrade that could invent
missing history. Use an audited offline transition/new history for existing
installations. Badger/fake retain their previous exporter behavior and do not
claim distributed epoch closure. No AWS apply, IAM changes or paid resources
are part of this implementation.
