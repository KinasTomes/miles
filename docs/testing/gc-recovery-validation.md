# GC and recovery fault validation

## Scope and acceptance status

Validated locally on 2026-11-11 with Linux Go 1.27.2, the pinned DynamoDB Local
3.3.0 and Moto 5.1.15 fixtures from `deploy/local/compose.yaml`. The fixtures use
dummy credentials, explicit loopback endpoints and a disposable Compose project.
No AWS resources or real archive restore timing were exercised.

| Area | Evidence | Status |
| --- | --- | --- |
| Reuse during GC admission, old GET/Copy views across overwrite and compaction | `TestNativeWarmGCObjectFlows`, existing R1 read and scrub suites | Local runtime validated |
| Source integrity and expired compaction writer | `TestLocalCompactionRejectsCorruptionAndExpiredWriter` | Native metadata with faulted fake physical reads validated |
| Delete/tombstone and bucket identity changes | `TestLocalGCNamespaceIdentityAndTombstone` | Native metadata validated |
| Exact generations across recovery cuts | `TestRecoveryRootsKeepExactGenerationsAcrossDeleteAndRecreate`, `TestRecoveryMissingHistoricalGenerationFailsWholeEnumeration` | Sealed projection/checkpoint and fake physical payload validated |
| Derived refcounts, serialized pages and transaction caps | `TestLocalGCRefcountIsDerivedNotDeletionAuthority`, exact boundary and no-dispatch tests | Native metadata and SDK serialization validated |
| Unknown COPY/DELETE, proof observation races and takeover | `TestLocalUnknownLifecycleCannotExpireIntoDispatchPermission`, `TestLocalTerminalEvidenceIsBoundToResolutionCAS` | Provider contract validated using an explicitly test-only oracle |
| Retention, multiple consumer pins, writer protection and R2 roots | Cold lifecycle matrix and seeded retention sequences | Local model/native safety validated |
| Unknown physical work resolved by a trusted runtime terminal provider | `TestNativeLifecycleTerminalProvider` through `sharedStore`/`composeShared`, real SDK and S3/Moto | Local composed-provider acceptance validated |
| Exact original step before takeover and authority cleanup | `TestLocalLifecycleTakeoverRequiresExactOriginalStep`, `TestLocalLifecycleCleanupKeepsMismatchedStepAuthority`, `TestLocalLifecycleSettledOutcomesResumeWithoutDispatch` | Native metadata and actual lifecycle resume validated |

After integrating PR #82, all five checklist areas have local acceptance evidence.
The provider-contract oracle remains explicitly test-only. Composed-provider
tests separately establish that the runtime obtains its proof through the real
S3 adapter. These local results do not establish live AWS acceptance.

## Regression found and fixed

`ChangeJob` previously reconciled a pending lifecycle step during takeover only
for tiering, restore and promotion. A GC or compaction worker could complete its
durable lifecycle transaction and lose the Jobs checkpoint acknowledgement,
leaving the replacement worker permanently rejected with unsettled work.

The native reconciliation path now includes GC and compaction. It still binds
the action kind and original fence into the claim transaction. Only durable
Done/Skipped/NotApplied outcomes, or a Waiting outcome for restore, permit
clearing the pending step. Unknown outcomes retain the barrier. Native tests
cover terminal COPY and DELETE reconciliation followed by takeover, and the
composed Cloud/Edge test now closes the original compaction authority, acquires
the next epoch, resumes the job and rejects the stale worker while old GET/Copy
views retain their independent pins.

## Review integration and exact original-step binding

PR #82 added immutable action V2 `StepID` and the trusted runtime terminal
provider. The merge retains the GC/repack takeover coverage and the V1/V2
decoder, schema validators and deployment upgrade gate. It preserves the
no-action tiering-discovery lease-closure path.

Adversarial native tests reproduced terminal Done/Skipped/NotApplied actions
being accepted with another valid StepID or an unbound legacy action before the
fix. Claim now requires matching action ID, job ID, kind, original fence and
nonempty exact original pending step for every allowed outcome. Waiting is
accepted only for restore. Unknown, Dispatched, missing/malformed actions and
step/identity/fence mismatches leave the pending job unchanged.
`TestLocalLifecycleTakeoverBindsActionObservationToClaimCAS` changes a valid
action's StepID immediately before native claim dispatch. The condition conflict
forces a reread, then the mismatched proof is rejected without mutating the job.

The authority-cleanup check enforces the same binding before reclaim/release,
so evidence rejected by claim cannot first release the old worker's authority.
Already-checkpointed work with no pending step can remain quiescent, but an
unbound legacy action cannot authorize cleanup. Terminal replay under a later
job step preserves the original action identity; a still-Waiting restore resumes
its original step. Tests exercise both and reject the stale worker.

## Invariants and fault boundaries

- Corrupt CRC, wrong raw hash with a matching stored CRC, short/oversized source
  responses, transient I/O and lease expiry during source read cannot register a
  destination, prepare a lifecycle intent or move a canonical location.
- Namespace tombstones and a recreated bucket identity remove current roots but
  only permit metadata retirement. The retained source is not physically deleted
  during that phase. Historical checkpoints independently protect their original
  exact generations after the current canonical location moves.
- Removing an intermediate historical pack makes physical root enumeration fail
  as a whole; it neither returns a partial proof nor withdraws the advertised cut.
- A recovered derived count of zero cannot delete a live or recovery-rooted
  winner. A stale positive count does not replace authoritative reachability.
- Aggregate item bytes are checked at 4 MiB minus one, exactly 4 MiB and plus one;
  action counts at 99/100/101. Maximum reference pages fit the native item envelope,
  and adding one reference is rejected. Invalid transactions make zero backend
  transaction calls and leave no rows visible across retry. Existing tests cover
  item envelopes, outcomes/receipts and actual SDK Binary/wire serialization.
- The terminal fixture binds the complete observed action, source generation,
  fence, target and exact sweep versions. Wrong identity, nonterminal or
  nonisolated evidence cannot settle the action. An evidence change immediately
  before the native resolution CAS forces conflict/re-evaluation. The fixture
  models trusted evidence; it does not implement a method to obtain that evidence.
- Unknown work blocks another maintenance admission, does not become quiescent
  at lease expiry and never gains another mutable-send permission. Valid fixture
  evidence permits progress without another COPY/DELETE dispatch.
- Cold tests cover minimum retention before/equal/after its boundary, independent
  protection deadlines, existing reader pins, active writers, unavailable R2
  roots and unverified post-retirement cuts. Archive classes remain excluded
  from compaction.
- Retention properties use seeds 1, 7, 42 and 2026, each with 64 pin/prune actions
  across five cuts and three exact generations. They explicitly check the
  24-hour-plus-10-minute boundary using conservative lower UTC bounds, independent
  reader/rebuild pins, complete physical location verification, eventual support
  withdrawal after all consumers release, and immutable chain/anchor survival.
  Failures print the seed and ordered operation trace.

## Executed results

- Full `go test -p 4 -count=1 -timeout=12m ./...`: PASS with both local endpoint
  requirements enabled, including the complete DynamoDB and app packages.
- Targeted race command below: PASS for recovery, lifecycle, DynamoDB, app,
  readpath and jobs. The final composed-runtime compaction takeover test also
  passed independently with `-race` after its runtime-authority assertions were added.
- `go vet ./...`, `go build ./cmd/...`, formatting and whitespace checks: PASS.
- Linux Python suite: PASS, 115 tests. The documentation-name checker passed.

The original full Go and Linux Python outputs are kept locally in the ignored
cache as `gc-recovery-full-final.log` and `gc-recovery-python-final.log`.
Post-integration outputs use `pr81-full-final.log`, `pr81-race-final.log`,
`terminal-native-race.log` and `pr81-python-final.log`. These are local logs,
not versioned fixtures or evidence of live AWS acceptance.

## Reproduction

Start `deploy/local/compose.yaml` in an isolated project and set the resulting
explicit `http://127.0.0.1:<port>` endpoints as `MILES_DDB_LOCAL_ENDPOINT` and
`MILES_RECOVERY_S3_ENDPOINT`. Set both corresponding `_REQUIRED` variables to `1`
so missing fixture configuration fails the selected native suites.

```sh
go test -p 4 -count=1 -timeout=12m ./...
go test -race -count=1 -timeout=7m \
  ./internal/recovery ./internal/lifecycle ./internal/metadata/dynamodb \
  ./internal/app ./internal/readpath ./internal/jobs \
  -run 'Retention|RecoveryRoots|RecoveryMissingHistorical|Compaction|Lifecycle|GC|Scrub|WarmR1LeaseEquality'
go test -race -count=1 -timeout=6m ./internal/app -run '^TestNativeWarmGCObjectFlows$'
go test -race -count=1 -timeout=8m ./internal/app \
  -run 'TestNativeLifecycleTerminalProvider|TestNativeWarmGCObjectFlows|TestNativeColdCloudEdgeFlows'
go vet ./...
go build ./cmd/...
python3 -B -m unittest discover -s scripts/tests
python3 -B scripts/check_doc_names.py
```

Use Linux for the Unix-socket and POSIX-permission Python tests. Live AWS suites,
external deployment tests and scale tests keep their own explicit opt-in gates;
the required local endpoints do not enable those acceptance claims.

## Composed runtime provider evidence

`TestNativeLifecycleTerminalProvider` configures independent Cloud/Edge clients
through the production composition. A context-scoped transport seam pauses the
original request before backend dispatch or consumes a successful SDK response
and discards its ACK. Backend effects still execute on Moto. Reconciliation uses
`runtimeLifecycleTerminal`, native R1/job observations and the actual S3 version
inventory/metadata provider; no terminal proof row or boolean is injected.

| Scenario | Before request completion | After backend terminal evidence | Total SDK mutations |
| --- | --- | --- | --- |
| COPY lost ACK | Durable Unknown retains maintenance and blocks takeover/GC | Exact action-marked version permits progress | 1 COPY |
| COPY in flight | No completed marked version; reconciliation remains blocked | Joined request's marked version permits progress after lost ACK | 1 COPY |
| DELETE lost final ACK | Unknown cannot release protection on an inventory outage | Every exact pack/sidecar target version is gone | 2 DELETEs |
| DELETE partially complete, final request in flight | One target version remains; no retry or terminal inference | Final original request completes, then exact-version proof permits progress | 2 DELETEs |

Every scenario first proves that durable PUT protection, a real GET pin and a
concurrent writer prevent tiering. DELETE cases additionally publish a live R2
cut, delete the head, retain the cut with an independent consumer pin beyond
its time window, then release/prune it through fenced retention operations.
GC still respects the retirement grace and requires a newer cut before sending
physical deletes. No direct metadata edits bypass these protections.

While Unknown, a provider-inventory failure, job takeover and concurrent GC
admission all remain blocked. Authentic provider evidence reopens maintenance.
The test then simulates losing the Jobs checkpoint ACK, reacquires authority
through the runtime, resumes under a new epoch, checks no additional SDK mutation
and rejects the stale worker. COPY cases retain readable Edge payload; DELETE
cases verify the exact swept generation is absent from complete inventory.

Live AWS restore latency, provider behavior under real AWS failures and deployment
acceptance retain their separate operational approval and execution requirements.
