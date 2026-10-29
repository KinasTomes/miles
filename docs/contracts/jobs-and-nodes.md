# Jobs / Nodes operational contract

W04-HUNG-02 implements shared operational records, worker state transitions,
node heartbeat/drain plumbing, metrics export and CLI transport. It does not run
tiering/GC/scrub, perform R1/R2 side effects or expose a production Admin listener.
W05-HUNG-01/02 composes those pieces with the real authorities and executors.

## Ownership and persistence

`internal/jobs` owns the model/state machine and `jobs.Store`. Badger and DynamoDB
implement the same contract; no job state lives only in the request node's RAM.
Badger is durable single-node development; DynamoDB uses existing `Jobs(job_id)`
and `Nodes(node_id)` tables, native attributes/consistent reads and conditional
transactions. No new table, GSI, TTL, scheduler or background cleanup service.

Each job stores immutable type, dry-run flag, bounded params and pinned global
policy revision; mutable state, logical revision, worker node/boot/fence,
pending step, checkpoint/progress and terminal result/error. Optional timestamps
are observations, never lease permission. Params/checkpoint each cap8KiB, record
cap128KiB, list page1..1000. HTTP uses `Job.View()`; internal worker fence,
pending step/checkpoint and params are not accidentally serialized to clients.

`Service.Create` validates and captures one policy snapshot. The adapter checks
that exact history record in the creation transaction; worker mutations check it
again. `ResumePolicy` loads that revision, never current on missing history.
Queued/running/cancelling/resumable jobs remain policy-history roots. Future
cleanup must establish stable roots under the existing recovery protocol; a
live, changing discovery scan is not a deletion proof. This task deletes no history.

## Worker authority, safe points and errors

`Store.ChangeJob` requires the expected job revision. Worker mutations check
the current R1 authority, node incarnation and job ownership in the same metadata
transaction as the write. The job lease is bound to `WriterFence.LeaseID=job_id`.
DynamoDB includes every authority observation in its conditional transaction;
closing the old lease between read and commit rejects that write. The supplied
production R1 provider must validate the correct job resource, service incarnation,
lease-use/UTC proof and shared closure protocol. Badger's local fixture fence
and the test fake lease are not distributed authority.

1. `claim`: queued → running. Same-job takeover needs a new, higher fence in the
   same service incarnation and a retained safe checkpoint. Cross-incarnation or
   pending/unsettled work does not automatically resume.
2. `begin_step`: persist a stable pending step ID **before** dispatch. R1/R2 intent,
   producer registration, pins and mutation reconciliation are still mandatory
   in the concrete executor; this marker cannot substitute for them.
3. `checkpoint`: require that exact pending step ID, store progress/checkpoint and
   clear the marker only after the trusted worker has settled/quiesced the work.
   A cancelling job becomes cancelled here. `finish` rejects a pending marker.
4. `cancel`: queued → cancelled, running → cancelling. It never rolls back
   published chunks/packs or rewrites a terminal job. Client receipt202 is not
   proof of cancellation completion.
5. `Service.Heartbeat` calls the injected R1 lease-renewal provider before updating
   the job observation. Missing/expired/closed authority fails; a timestamp never
   renews a lease. R1 retains the existing60s/15s/5s lease/heartbeat/grace contract;
   grace grants no worker rights. Draining nodes cannot claim or begin new steps.

Stale job revisions conflict; stale/closed worker fences fail. `ErrReconciliation`
retains unresolved work/checkpoint and withholds new ownership/success. The worker
must reconcile its durable R1/R2 outcome before a supported resume path; no blind
destructive replay. Lease expiry is not proof that a dispatched mutation failed.

Backend unavailable/unknown ACKs are not proof of no commit. Create retains its
generated job ID alongside the error for `GetJob`/list reconciliation; callers must
check the error and must not blindly create another job. Retry a mutation only after
checking the original record, revision, fence and operation/checkpoint identity.

## Nodes and runtime health

Nodes record profile/private Admin address, process/boot and service incarnation,
heartbeat, last observed readiness, draining, active requests and applied policy
revision. Revision0 is create-only; CAS rejects stale updates and incarnation reuse.
Register a new node ID after process restart unless a separately reviewed takeover
protocol supplies a safe identity migration. Heartbeat cannot clear a drain marker.
Applied policy revision0 means unknown; only report a revision actually installed.

`Registry.Tick` and `Run` use the shared Clock, heartbeat every15s, and local
monotonic freshness <30s for runtime readiness. A failed write fails readiness;
wall jumps cannot renew that freshness. Remote liveness requires validated time
evidence against the reported heartbeat, not TTL or a persisted `ready=true` bit.

Local `RequestDrain` closes the admission gate **before** metadata I/O; an error
cannot reopen it. A remote request persists a marker, applied by the target's next
heartbeat. It does not falsely report that the remote gate is already closed.
W5 fan-out/routing must reach the target for the immediate drain API guarantee.
The local gate blocks new S3 admissions while retaining existing active requests.

`app.Operational` explicitly attaches a registry, metrics and optional CloudWatch
client to process lifecycle. Heartbeat health joins the existing readiness guard;
production runtime remains unready until real dependencies are composed. Shutdown
closes admission, persists draining, cancels/joins heartbeat/export workers and
only then closes dependencies. Persistence/join failure reports incomplete.
Physical pack/outbox flush, ALB deregistration and full drain-job execution remain
W5 integration; active_requests=0 alone is not a durability proof.

## Wire results and CLI

System-admin operations follow API §6; identity/token checks stay owned by the
Admin middleware. Do not expose `jobs.Store` directly over unauthenticated HTTP.
Internal page positions are not public cursors: wrap them with existing `admin.Keyring`
MAC/principal/resource/filter binding, expiry and limits in the Admin route layer.
Pagination is bounded discovery, not a cluster snapshot or epoch membership proof.

Per-node/cluster result schema is `jobs.Result`, with explicit unique membership:

```json
{
  "scope": "cluster",
  "nodes": [
    {"node_id":"cloud-a","state":"succeeded","result":{"removed":3}},
    {"node_id":"edge-a","state":"unavailable","error_code":"heartbeat_stale","result":null}
  ]
}
```

`Result.Validate` checks bounds/duplicates/states. The executor must separately
bind the node list to its immutable admitted membership and report partial failure;
schema validation does not prove that every target ran. No fabricated cache/cluster
sum, ratio or successful physical operation is supplied by this foundation.

CLI transport supports `job ls/show/run/cancel`, `node ls/drain`. Lists accept
`--limit`/`--cursor`; `job run <type>` accepts `--dry-run`, `--params-file` and optional
`--policy-revision` (omitted captures current). Global endpoint/token/timeout flags
precede the command. Token stays in a private0600 file, JSON request cap16KiB,
params cap8KiB, no redirects/proxy, bounded/redacted responses. Unsupported server
routes and auth/transport errors exit nonzero, including mutations.

## Metrics / CloudWatch

Health snapshots, active requests and `miles_job_updates_total{job_type,state}`
are cached/process-local. This counter counts acknowledged record updates, not
cluster job inventory or exact-once events. Labels are fixed enums; no job/node/
object/user IDs. Scrapes perform no metadata or CloudWatch I/O.

CloudWatch uses namespace `Miles/DSG`, profile=`cloud|edge`, one batch each60s,
standard60s resolution and at most128 validated series. The current inventory
is at most67 finite series, below the native1000-metric/1MiB limits. Counter deltas
use Sum; gauges use Average/Maximum as observations, not a cluster total. Failed
ACKs retain the unacknowledged delta; ambiguous acceptance can duplicate it. SDK
hidden retries are disabled for export. This best-effort telemetry is not billing
or an authoritative job ledger, and export errors do not make storage healthy.

AWS SDK CloudWatch v1.75.0 matches the existing core/Smithy pins; no core SDK bump.
The runtime supplies its refreshed credential provider/client; this task creates
no credentials, applies no IAM and enables no AWS publisher automatically.
[PutMetricData API limits](https://docs.aws.amazon.com/AmazonCloudWatch/latest/APIReference/API_PutMetricData.html).
