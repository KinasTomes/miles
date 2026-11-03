# Checkpoint, rebuild and retained recovery cuts

W05-PHONG-03. Baseline `b3af419`, checked 2026-11-04. The supplied backlog
snapshot lists W04-HAI-02, W04-HUNG-04, W05-HUNG-02 and W05-PHONG-02 as Hard
Done. PR #61 is merged in this baseline. This change does not update task status.

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
fails closed. Binary manifests retain their existing FORMAT limits. Immutable
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
