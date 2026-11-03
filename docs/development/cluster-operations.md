# Shared cluster operations

The Admin API uses shared Jobs/Nodes records. Receiving a request on another
gateway does not create a separate job namespace. System-admin authorization,
strict request parsing and caller/resource-bound cursors use the existing Admin
server. Job views exclude fences, internal parameters and checkpoints.

## Runtime capabilities

| Job | Current execution |
| --- | --- |
| `cache_purge` | Local cache primitive, or a parent coordinating immutable target-specific child commands |
| `drain` | Acknowledged target admission gate; Edge reports gate-only, Cloud waits for native shutdown/ALB proof |
| `journal_flush` | Immutable journal export, scheduled once per minute under shared scheduler authority; also callable manually |
| Other declared types | HTTP 501 `job_type_unavailable` before enqueueing |

Declaring a type in `jobs.Types()` does not install an executor. GC/compaction,
tiering, scrub, checkpoint, upload purge and upload-recovery job adapters remain
separate integration work. Existing object upload-recovery methods are not a
complete background-job adapter or a bounded periodic discovery process.

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
ID is omitted. Drain jobs require an explicit node ID. Journal export accepts
empty parameters. `dry_run` does not purge, close admission or export data.

## Worker and fan-out ownership

A worker uses the supplied R1 authority, exact policy history, stable step IDs
and CAS job transitions. Heartbeats renew soft expiry while an eight-minute
bounded step runs; they cannot extend the hard lease deadline. Heartbeat failure
cancels and joins the step before any result is published. Unknown outcomes retain
the pending marker. Physical jobs cannot take over unsettled work merely because
a heartbeat expired. Cache eviction, sticky drain and immutable journal export
can safely revisit their same step after an authority transfer.

Periodic creation holds a type-specific scheduler lease and checks shared active
records before enqueueing. Slot IDs reconcile duplicate creation and lost ACKs.
The hourly recovery-cut supervisor has separate shared ownership; it uses the
existing durable cut deadline, while the Jobs runner owns periodic export.

Cluster cache purge freezes node IDs and boot incarnations in the parent job.
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
