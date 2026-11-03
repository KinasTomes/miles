# Shared cluster operations

The Admin API uses shared Jobs/Nodes records. Receiving a request on another
gateway does not create a separate job namespace. System-admin authorization,
strict request parsing and caller/resource-bound cursors use the existing Admin
server. Job views exclude fences, internal parameters and checkpoints.

Job creation, cancellation and HTTP drain markers carry the admitted Admin actor.
Each backend checks the current token, active user and system scope inside the
same write transaction, together with the caller admission fence. Internal
scheduler/worker contexts retain their own authority.

## Runtime capabilities

| Job | Current execution |
| --- | --- |
| `cache_purge` | Local cache primitive, or a parent coordinating immutable target-specific child commands |
| `drain` | Acknowledged target admission gate; Edge reports gate-only, Cloud waits for native shutdown/ALB proof |
| `journal_flush` | Immutable journal export, scheduled once per minute under shared scheduler authority; also callable manually |
| `purge_uploads` | Fenced recovery closes expired Active or Aborting uploads and releases staging once |
| `recover_uploads` | Resumes stale Completing uploads from frozen input after lease expiry plus grace |
| Other declared types | HTTP 501 `job_type_unavailable` before enqueueing |

Declaring a type in `jobs.Types()` does not install an executor. GC/compaction,
tiering, scrub and checkpoint job adapters remain separate integration work.

Acceptance covers the shared dispatch/API, scheduler, policy checkpoints,
fencing, cancellation, node drain and the installed executors above. Consumer
components own their physical executor, its admission authority and business
integration tests. They register execution after those contracts are available,
using the explicit unavailable response during the handoff.

In particular, destructive maintenance requires authority compatible with its
R1 maintenance gate. Registering it as an ordinary control-lease executor would
make the job itself an active participant blocking maintenance. Do not bypass
that gate or claim GC support by registering an empty step.

## HTTP and CLI

- `POST /admin/v1/jobs` creates an installed job, returning 202 and `Location`.
- `GET /admin/v1/jobs` and `GET /admin/v1/jobs/{id}` work through any gateway.
- `DELETE /admin/v1/jobs/{id}` requests cancellation; running work stops only at
  a settled checkpoint. Repeating cancellation is safe.
- `GET /admin/v1/cluster/nodes` lists heartbeat/readiness/drain state.
- `POST /admin/v1/cluster/nodes/{id}/drain` returns 202 only after the target has
  closed admission and persisted `drain_applied`. A timeout does not undo a
  durable drain request. Inspect the job in `Location` when present.

The existing `dsgctl job ls/show/run/cancel` and `node ls/drain` commands use these
routes. Cache job parameters accept `{"scope":"node","node_id":"..."}` or
`{"scope":"cluster"}`. Node scope defaults to the receiving gateway when its
ID is omitted. Drain jobs require an explicit node ID. Journal export and both upload sweeps accept empty parameters. `dry_run` does not purge, close admission or export data.

## Worker and fan-out ownership

A worker uses the supplied R1 authority, exact policy history, stable step IDs
and CAS job transitions. Heartbeats renew soft expiry while an eight-minute
bounded step runs; they cannot extend the hard lease deadline. Heartbeat failure
cancels and joins the step before any result is published. Unknown outcomes retain
the pending marker. Physical jobs cannot take over unsettled work merely because
a heartbeat expired. Cache eviction, sticky drain and immutable journal export
can safely revisit their same step after an authority transfer. Upload sweeps
use the same bounded replay rule through transactional upload fences and receipts.

Upload sweeps have a type-specific periodic owner every 60 seconds. Each step
scans up to 32 native pages, retaining empty filtered-page continuation keys and
checkpointing progress. The 10,000-page run cap reports `scan_limit` with its
checkpoint. Eligibility uses the trusted UTC lower bound and is rechecked by
the operation transaction. Purge closes expired Active/Aborting uploads through
the recovery port, including open publishers, before releasing quota. A Completing
upload follows frozen-input recovery even after its upload expiry. Dry-run
reports examined/skipped counts and preserves data. Outcome errors retain the
pending step until durable upload state proves it settled.

Upload sweep dispatch holds recovery-kind authority for job progress. Physical
cleanup or completion acquires its own writer admission. A recovery cut can
close and seal while a sweep waits for that admission, then the same pending
step resumes in the next epoch. The progress lease cannot authorize physical
write dispatch.

Periodic creation holds a type-specific scheduler lease and checks shared active
records before enqueueing. Slot IDs reconcile duplicate creation and lost ACKs.
The hourly recovery-cut supervisor has separate shared ownership; it uses the
existing durable cut deadline, while the Jobs runner owns periodic export.

Cluster cache purge freezes node IDs and boot incarnations in the parent job.
Membership covers the current service incarnation and heartbeats within two
minutes of discovery: recently missing peers produce partial results, while
historical boots are excluded. It is a frozen discovery set, not an atomic
cluster-membership snapshot or a promise about nodes that join later.
Targets consume child commands from shared metadata, including Edge with its
loopback-only Admin listener. There is no peer bearer forwarding or new inbound
Admin port. A replacement incarnation cannot execute an old command. Membership
is limited to 64 targets and the existing 8-KiB parameters bound; larger requests
are rejected without truncation.

Partial results enumerate each target. Stale/unreachable nodes are unavailable;
a 60-second fan-out budget reports unresolved outcomes as timeouts, never as a
successful cluster purge. Cancellation propagates to children and waits for
active child safe points. Already requested drain remains sticky after cancel.

Workers poll runnable records every five seconds. Node drain observation polls
every second; the HTTP acknowledgement, rather than receipt of a marker alone,
is the gate boundary. Runnable discovery retains empty native Scan continuations
and bounds active records to 1,000 / Scan pages to 1,000. The base-table filter
still reads history: sustained large inventories need a work index or reviewed
history-retention policy before relying on this scan design for throughput/cost.
A scrape performs no backend work. Bounded job updates and dispatch-failure
counters join the existing 60-second CloudWatch exporter using runtime temporary
credentials and namespace `Miles/DSG`.

## Cloud completion proof

The gateway writes a private drain marker bound to its systemd invocation. The
existing privileged lifecycle controller validates its ownership and contents,
stops that invocation and deregisters its ALB target. If an operator drain
transitions into ASG termination, the controller begins hook heartbeats too.
It records `drain_state=complete` only after the invocation's clean-shutdown
proof and ALB `unused` state; native record-token CAS protects the Nodes update.
Lost ACKs are reconciled by a consistent read. An absent proof, failed exit or
timeout cannot publish completion. Older Nodes rows decode with no invented proof.

Deploy the updated controller together with the gateway. Fleet bootstrap passes
the reviewed table prefix into the controller configuration. Edge closes its gate
and reports `flush_complete=false`; it does not run the Cloud lifecycle controller.
No Admin or metrics listener is made public by this feature.
