# Checkpoint, rebuild and retained recovery cuts

Technical prerequisites are the authoritative multipart/event producers, sealed
recovery epochs, shared jobs/drain coordination and pack lifecycle projections.
The baseline includes these merged implementations.

## Implementation mapping

1. `recovery.Checkpointer` builds historical snapshots from an audited genesis
   or a previously published checkpoint plus every contiguous sealed epoch through
   the exact requested target. It never snapshots live tables. The existing
   membership/descriptor/attachment verification remains mandatory. Event hashes
   are not transaction order: authoritative effects merge by entity revision,
   equal-revision disagreement fails, and tombstones remain in historical state.
   Policy history revisions survive replacement of the current pointer.
2. `Checkpointer.Rebuild` produces a sorted concrete index installation plan:
   users, bucket incarnations/grants, disabled credentials/tokens, configuration
   history/current pointers, heads/tombstones, canonical chunk locations, pack
   state/protection/lifecycle facts, binary manifests/completion pages, and both
   multipart receipt projections. Receipt expiry is unchanged. Known incomplete
   uploads become Aborted; unjournaled old uploads/parts/leases are not installed
   and cannot resume. Used bytes, live namespace and chunk-reference counts are
   derived from verified current manifests. Reserved/staged/active-upload counters
   reset to zero. Missing quota/config/manifest/location prerequisites fail closed.
3. Support catalog publication uses immutable completion slots followed by native
   metadata CAS, bound to the recovery worker and current history/incarnation.
   Lost pointer acknowledgements reconcile from the same immutable slot; the slots
   also permit discovery without the source DB. Offline installation requires a
   dedicated empty table inventory and an injected `OfflineRecoveryGuard`; no
   default provider grants this authority. The target stays Closing while records
   stage with atomic progress/record acknowledgements. Only an exact record digest
   plus a repeated isolation/reconciliation proof opens the new incarnation.
4. `Recoverer` selects the newest supported cut at or before the requested time,
   inside the conservative 24-hour window. It tries a supplied PITR source first,
   compares its exact target, authoritative state and dependencies with reference
   replay, and rejects mixed/post-target images. An unavailable PITR source falls
   back to sealed replay. Every live canonical payload is verified before install:
   exact generation/range, CRC, codec decode and raw content hash. GLACIER_IR is
   readable; GLACIER requires an explicit completed restore before recovery can
   proceed. Restore acceptance alone is insufficient.

## Runtime and operator entry points

The shared runtime now installs `recovery_checkpoint`. Create it through the
existing system-admin jobs API with an explicit immutable target:

```json
{"type":"recovery_checkpoint","params":{"target_epoch":0}}
```

Unknown fields, null/missing targets and unsupported integer bounds are rejected.
The existing jobs API checks capabilities, admission, authorization and policy
revision. Dry run verifies the chain and publishes no support marker. The worker
uses a recovery lease; final artifact writes and catalog CAS recheck authority.
It never closes healthy producers merely to make a requested target available.
Unsealed/missing targets fail instead of being silently moved forward.

`ProvisionAuditedHistory` accepts an explicit initial seed before producer
admission. It cannot infer completeness from a running DB. Older opaque prototype
genesis bodies are deliberately rejected by checkpoint/rebuild; an audited typed
base is a prerequisite, not an automatic migration promise.

Offline tooling calls `Recoverer.Recover` with trusted UTC bounds, a new random
history/incarnation, a physical pack adapter and an `IndexInstaller`. A real
installation must supply a provider that proves persistent isolation of **all**
old workers and terminal reconciliation of destructive S3 actions, including
actions after the selected cut. Expiry, HEAD, or an operator boolean is not proof.
The default nil guard refuses installation. This change exposes no force switch
or HTTP restore endpoint that bypasses that requirement.

An installation writes a separate `normalized-genesis` with source-cut provenance;
the old historical snapshot keeps its original credential status. Canonical pack
class/restore metadata is reconciled with physical observations. Incarnation-bound
immutable catalog selectors coexist with the old selector; neither timestamp nor
mutable "latest" chooses history. Credentials must be explicitly reprovisioned
after the operator's security audit; recovered credentials never activate themselves.

## Retention and downstream obligations

Each supported cut has an immutable exact-generation root inventory. Publication
re-derives the checkpoint projection from a trusted base; a checksum and matching
seal alone cannot advertise a checkpoint with missing effects. Existing support
survives a newer checkpoint. Internal fenced consumer pins prevent withdrawal;
`Prune` withdraws only unpinned targets strictly past 24 hours + 10 minutes using
the trusted lower bound. Equality at the boundary retains the cut. Requested
restore times outside the actual 24-hour support window are refused.

Pruning never deletes artifacts or payload. Base/chain/discovery anchors remain
immutable. New-history genesis retains the source support catalog and its roots,
including cuts after a rollback target: rollback cannot cancel a prior physical
retention obligation. Inherited roots have no automatic release mechanism; they
remain conservative until a separately audited consumer can discharge them.

GC/compaction must consume `Retention.ProtectedRoots` and the existing independent
R1/transient/reader roots before destructive admission. Neither an absent index
entry, a new checkpoint nor a prune result is a DELETE permission. The existing
runtime destructive guard remains in place; no lifecycle DELETE is enabled here.
Missing/corrupt remaining artifacts are integrity failures, never normal expiry.

## Bounds and evidence

Snapshot state: at most 256 pages, 128 rows per page and 256 KiB per artifact.
Dependency indexes use 64 entries/page, up to 256 pages. Replay holds at most
32,768 distinct rows and 262,144 distinct entity revisions per epoch; overflow
fails closed. Rebuild materializes at most 64 MiB of unique attachment pages and
262,144 manifest references; repeated identical pages do not consume that budget
again. Binary manifests retain their existing FORMAT limits. Immutable
support discovery follows at most 10,000 slots per invocation. Recovery ancestry
is bounded to 16 histories; unsupported depth requires an audited new anchor.
No collector silently truncates a supported cut or drops roots to fit a limit.

Regression coverage:

- Checkpoint contiguous chain, exact target, restart/lost writes, missing anchors,
  conflicting revisions, paged state and immutable configuration history.
- Rebuild namespace/manifests, original receipt expiry, disabled security,
  incomplete upload abort, recomputed quota/reference counts, Warm/IR/GLACIER
  payload verification and restore acceptance versus readiness.
- Supported-window boundaries, independent pins, retained anchors, DB-independent
  discovery, lost pointer ACK, fenced marker creation, PITR post-target rejection,
  missing physical payload and checksummed-but-incomplete checkpoint rejection.
- Native DynamoDB: partial installation stays closed, replay-safe record staging,
  final proof failure, exact record-digest checks, canonical manifest/location and
  both receipt projections, stale catalog worker, and producer -> seal -> checkpoint
  job -> source DB loss -> new-history install with disabled old credentials.

Targeted Linux race suites use Go 1.27.2 and pinned DynamoDB Local 3.3.0.
The full gate also uses pinned Moto 5.1.15 for existing shared runtime/S3 tests.
Only disposable local fixtures are used. Modeled isolation/UTC/PITR seams do not
attest live AWS PITR equivalence, fleet isolation, terminal S3 outcomes, archive
restore latency or KMS availability. Those real-environment drills remain
downstream acceptance; no billable AWS operation is performed by this validation.

Recorded results:

- `17b90cf`: clean clone `make check` passed with both local endpoints mandatory;
  full affected race suites passed. Native DynamoDB race package took 297 seconds.
- `a55b2b8` / `0dbbc8f`: final readiness/identity regressions and targeted native
  recovery/storage race suites passed; `go vet ./...` and `go build ./cmd/...` passed.
- `9095f16`: recovery race suite and recovery vet passed after the attachment
  materialization bound. New native install tests use modeled persistent isolation
  and an S3 service seam; separate existing runtime tests use the real SDK on Moto.

## Historical full-prefix measurement (before anchored verification)

At 720 sealed epochs (at least 720 events; quota updates plus the initial live
manifest/receipt fixture), the opt-in test measures the full Build/Load/Publish
path with 1 ms injected per immutable-object read. Windows, Go 1.27.2 local run:

- Incremental build: 10,832 reads, 17.544 seconds.
- Loading the result: 3,611 additional reads.
- Publication, including independent re-derivation: 23,104 reads.
- Whole measured sequence, including a genesis-based candidate for initial
  publication: 49,810 reads, 24,797,519 returned bytes, 80.519 seconds. PASS under
  the 480-second step budget.

Reproduce with `MILES_CHECKPOINT_SCALE=1 go test ./internal/recovery -run
'^TestCheckpointLongHistoryReadBudget$' -count=1 -timeout=10m -v`. Ordinary CI
skips this latency experiment. This is an artifact-store latency model, not an
AWS benchmark: most epochs have one small event without binary attachments.
The measurement exposes the remaining linear full-prefix read cost. At 10 ms
per sequential read the measured whole sequence alone would exceed 480 seconds;
this result does not establish a universal long-history/S3 step-time guarantee.
Larger histories, attachment-heavy workloads and higher-latency deployments
require separate measurement or checkpoint-anchored validation before rollout.


## Published-checkpoint anchors and review regressions

Normal `CheckpointWorker.Step` obtains its base from the history/incarnation-bound
support catalog. It installs an internal anchor only from that published,
checksum-bound checkpoint; HTTP/job parameters cannot supply anchors. Publication
of the base already verified its complete state by independent sealed replay.
Loading an anchored base still checks snapshot pages and required dependencies.
Successors verify every epoch from anchor + 1 through the requested target, with
exact previous-seal binding, marker/history/epoch checks, membership/event digest
checks and attachment validation. New candidate state is still independently
re-derived at publication. Thus integrity follows from the trusted base plus the
contiguous verified suffix; a matching new seal alone remains insufficient.

The standalone `VerifyCut` and unanchored offline audits retain complete-prefix
verification. A missing old marker is still detected by full audit; it is not
withdrawal or physical GC permission. Inherited provenance/dependency validation
and snapshot size still contribute work, so this is not a universal S3 latency SLA.

`TestCheckpointWorkerAnchoredStepReadBudget` exercises the actual job Step after
publishing epoch 718 in a 720-epoch history. With 20 ms injected on each immutable
object read, epoch 719 required 128 reads / 145,216 returned bytes, 2.56 seconds
of modeled I/O and 2.82 seconds observed Step time on Linux. Its ordinary regression
cap is 2,000 reads and the Step context has the real eight-minute timeout.
`TestCheckpointTrustedAnchorChecksEveryNewLink` checks wrong predecessor rejection
and the separate full-audit behavior. These results supersede the full-prefix
baseline above for the anchored runtime path.

Standard first-install provisioning now creates a typed audited genesis from its
exact offline bootstrap seed. The Cloud/Edge DynamoDB Local/Moto runtime test now
seals and publishes a normal checkpoint using that standard seed. Existing opaque
prototype bases do not register the checkpoint executor, returning the existing
capability-unavailable response until an operator supplies an audited compatible
base through offline recovery/provisioning. No running-table scan manufactures
that compatibility.

Rebuild installs only verified live canonical winners. Historical unused chunk
rows stay in the original snapshot/source provenance for audit and retention,
but are omitted from both the dedup index and normalized new-history baseline,
so a subsequent insert starts a fresh revision sequence. The tombstone/missing-pack
regression verifies both omissions.

Installation retry first rediscovers the immutable incarnation selector, validates
its source checkpoint and original requested target, rechecks current payload
readiness, then reuses that exact genesis/record digest. It does not regenerate
restore-expiry facts or select a newer support cut. The interrupted-install test
extends archive restoration between attempts and asserts unchanged selector and
installation digest before successful resumption.
