# Phase 5 — policy revision integration evidence

Status: implementation/API PROPOSAL for consumer review. Not Frozen, not a
production adapter or production runtime/simulator integration.

## Contract mapping

| Scope/path | Requested/current/history | Candidate / CAS | Evaluator / failure |
| --- | --- | --- | --- |
| Global bootstrap | Empty store only | Complete validated policy, candidate revision0, expected0; backend allocates1 | No defaults on historical read; auth/fence/event still required |
| Global admission | GetPolicy(0) returns authoritative current R | No write | Context pins exact validated R; no per-chunk current refresh |
| Global job/resume | GetPolicy(R), R>0 | No write | Missing/error/mismatch/incomplete document fails; no current/default fallback |
| Global update | Proposal based on R | Candidate revision0, expected R, caller credential/fence/event | Backend CAS; no automatic retry/rebase. No-op still checks CAS |
| Bucket | Independent bucket ID/revision B | No shared bucket cache-policy persistence/history port exists | Persistence BLOCKED; BindBucket only validates consumer handoff identity/revision |
| Read/write/protection | One immutable global snapshot R | Evaluators never persist | Empty policy input explicitly binds context; nonempty mismatched revision/content fails |

Sources: [Clock §3–7](../../../docs/contracts/clock-and-policy.md),
[Shared Storage](../../../docs/contracts/shared-storage.md),
[Engine](../../../docs/contracts/chunking-and-commit.md),
[R1](../../../docs/contracts/r1-and-cache.md),
[R2](../../../docs/contracts/r2-recovery.md).

## Architecture and provenance

`MetadataStore.GetPolicy/PutPolicy -> config validation -> immutable Context ->
shared policy.EvaluateRead/EvaluateWrite/EvaluateProtection`.

Load/create/commit helpers perform metadata I/O; the context's evaluation methods
and the underlying policy package do not. They retain no store reference. All
APIs are trusted INTERNAL calls: authorization must precede resource/revision
disclosure. Helpers do not implement HTTP/Admin authentication.

Context.Global is obtained through the supplied MetadataStore and strict config
decoder. It is not a cryptographic provenance token. Never use its revision as
R1 execution authority or R2 recovery epoch. BindBucket accepts a consumer-supplied
snapshot only; it cannot establish an authoritative bucket-policy read. Global
and bucket revisions need not be numerically equal and cannot replace each other.

Existing app dependencies wire startup/health and dev fixtures, not admitted
read/write/protection consumers. No actual simulator execution entrypoint was
found. Tests therefore exercise a harness with Real/Virtual Clock domains and
controlled UTC observations, not production integration or full simulator
determinism. Test caps/allowlists and fixture authority facts are not deployment
decisions or proof of actual authority.

## Evidence and limits

- Shared fake creates/reads global history, executes concurrent changing CAS,
  rejects stale no-op and losing event/history publication, and tests unchanged
  historical content and owned returned bytes.
- Shared fake's existing fail-before/fail-after hooks distinguish unavailable
  outcomes before and after publication. Helpers return the error unchanged and
  never automatically rebase. Post-publication response verification failure is
  classified as existing `storage.ErrUnavailable`, with diagnostic details and
  explicit reconciliation guidance; it is not exposed as an ordinary input
  validation rejection. This conservative category does not distinguish a
  committed mutation with a damaged response from other unknown outcomes. A
  richer taxonomy remains a consumer decision; no shared API is extended.
- INT-R01: CommitGlobal decodes the COMPLETE candidate with the validator passed
  to the commit call BEFORE PutPolicy. Validation by the proposal creator is not
  reused as proof of commit-validator readiness/limits. A temporary decode-only
  revision labels validation; the published candidate still has revision0 and
  only the backend allocates the authoritative revision. No candidate repair,
  defaults, retry or rebase is introduced.
- Permanent A–G regressions use the shared fake plus a delegating publication
  spy (call counting/response fault injection only, no new CAS/store behavior).
  Unconfigured/stricter validators make ZERO PutPolicy calls and leave current,
  history/outbox unchanged. Accepted candidates publish consistently; stale
  change/no-op proposals conflict through CAS. Before/after commit unavailable
  outcomes are not retried. Corrupt returned revision/version/document/content
  is reported as unavailable while actual committed history/outbox remain intact.
- Shared fake accepts incomplete JSON-object policy envelopes; LoadGlobal rejects
  those historical records via strict decoding. Malformed non-object JSON is
  tested directly at the existing decoder boundary, not injected via a new port.
- Bucket creation/read checks are LOCAL constructor/decode checks only. Durable
  bucket creation/read/CAS/history evidence is blocked until the shared owner
  supplies reviewed capability. No substitute fake/store is introduced.
- Pinned R remains stable after current advances. All three evaluation methods
  report R, reject contradictory supplied pins, and do not mutate policy current
  or history. Real/Virtual adapters use the same evaluators and equivalent UTC
  bounds/remaining budgets. Missing/untrusted UTC and binding mismatch fail closed.
- Concurrent protection proposals illustrate differing targets, not atomic
  persistence. Executor must use atomic max(current,Target) or CAS/retry.

## Consumer obligations / enablement gates

Production MetadataStore CAS/current/history/outbox atomicity; actual
authorization, R1 admission/lease/fence/closure and R2 registration; UTC evidence
trust/freshness/binding and restore expiry provenance; backend restore acceptance;
backend precision/lattice compatibility and protection acceptance; monotonic
protection persistence; durable retention-root preservation; unknown-outcome
reconciliation; actual runtime/simulator integration and distributed correctness
remain unproven by these fake/integration tests.

Do not drop roots because protection time passed. Do not restore worker authority
from a historical policy snapshot. Do not treat RestoreRequired as proof that a
backend restore has expired. Caller must use the pinned AutoRestore policy when
deciding whether/how to request restore and revalidate before side effects.
