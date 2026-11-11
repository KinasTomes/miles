# Lifecycle, recovery and Cold demonstration runbook

This runbook is for a Week 8 operator exercise. It describes the behavior
implemented in the shared Cloud/Edge runtime as of `origin/main` at
`ad5c798`. It is an operational guide, not a claim that every recovery tool is
exposed as an Admin command. In particular, full rebuild is an offline guarded
library workflow and must not be improvised from a running shell.

## 1. Access, scope and stop conditions

### Access

- Use a system-admin bearer token in a private `0600` token file for Admin
  actions. Prefer the approved SSH tunnel to the loopback Admin listener:

  ```sh
  export DSGCTL_ENDPOINT=http://127.0.0.1:19000
  export DSGCTL_TOKEN_FILE=/private/system-admin-token
  dsgctl job ls --limit 100
  ```

  Keep the token out of shell history, command arguments, logs and screenshots.
  `dsgctl` sends it only to the configured Admin endpoint. Use a separate
  operator identity for provisioning; do not use a provisioning role as a
  substitute for Admin authorization.
- Cloud runtime AWS access comes from its instance profile. Edge runtime AWS
  access comes from the configured `credential_process`. Routine lifecycle and
  checkpoint jobs use the runtime's service permissions and shared recovery
  lease; they do not require the operator to export AWS static keys.
- Bootstrap and offline index recovery require a separately approved,
  temporary provisioning/recovery role and its exact selected Region. Full
  rebuild also requires an `IndexInstaller`, trusted UTC bounds, verified
  recovery artifacts and a provider that proves isolation of every old worker.
  Current `dsgctl` has no rebuild command or HTTP endpoint. If those reviewed
  components are not supplied, stop and escalate; do not edit DynamoDB rows or
  call internal APIs from an ad-hoc program.

### Before any change

1. Confirm profile, selected Region, exact pack ID and generation, current
   policy revision, and the change ticket/owner. Never use a pack ID without
   its generation for lifecycle operations.
2. Capture `/healthz`, `/readyz`, `/metrics`, `node ls`, and the target
   `job show`/`pack show` before-state. Health and metrics are on the private
   Cloud health listener or Edge loopback listener; they are not public APIs.
3. Keep ordinary Cloud/Edge service admission open for normal tiering, restore
   and promote jobs. Do not stop healthy producers to force a recovery cut or a
   checkpoint target. For node replacement, use `node drain <node_id>` and
   wait for its durable result. For full offline rebuild only, close R1
   admission through the supported owner procedure, wait for the gate's active
   and resolving counts to reach zero, stop every gateway/worker, and preserve
   the journal and pack data. A local drain/readiness result alone is not an R1
   fence.
4. Stop and escalate on stale health observations, `/readyz` 503, incomplete
   drain, unresolved lifecycle `Unknown`, R1 busy/fenced errors, missing or
   corrupt recovery artifacts, untrusted UTC, or any discrepancy between
   metadata and the exact physical S3 generation. Do not retry a mutable S3
   operation merely because its ACK was lost.

## 2. Run lifecycle work

All commands below assume the access variables above are set. `job show` is the
durable progress view; poll it at a modest interval and also re-read the exact
pack generation when the job reports a lifecycle state change.

### Tiering

1. Inspect candidates and current policy:

   ```sh
   dsgctl pack ls --state Warm --limit 100
   dsgctl policy get
   ```

2. A normal `tiering` job scans bounded pages and queues stable exact-generation
   child jobs. Use an empty object for the scan request. First do a dry run:

   ```json
   {}
   ```

   ```sh
   dsgctl job run tiering --params-file tiering-scan.json --dry-run
   ```

   Review the returned job and counts. For an approved live run, use the same
   input without `--dry-run` and pin the reviewed policy revision with
   `--policy-revision`. Each child independently rechecks policy, retention,
   reader pins, roots, UTC proof and R1 maintenance authority before COPY. A
   candidate list or a successful dry run is not dispatch permission.

3. Follow the parent and child jobs with `job show <job_id>` and `job ls`;
   verify each affected generation with `pack show <pack_id>`. Confirm class,
   state revision, retention and protection. Existing bytes are retained; a
   class change is not proof of deletion or billing completion.

### Restore

1. Confirm the exact generation and class:

   ```sh
   dsgctl pack show <pack_id> --limit 100
   ```

   `GLACIER_IR` is directly readable. `GLACIER` requires restore; select the
   approved `Standard`, `Bulk` or `Expedited` tier and 1–30 days.
2. Submit the asynchronous request:

   ```sh
   dsgctl pack restore <pack_id> <generation> --days 1 --tier Bulk
   ```

   Record the returned job ID. `restore_accepted` means the provider accepted
   the request; it does not mean the object is readable. Continue polling
   `job show <job_id>` and `pack show <pack_id>`. The job is complete only when
   the runtime observes a positive restore state with sufficient expiry under
   trusted UTC. Verify a representative client read and checksum after
   readiness, before declaring the demo/use complete.
3. Cancellation is a safe-point request, not a claim that the provider
   cancelled an accepted restore. The accepted request may finish; a later
   explicit job can resume observation without issuing a duplicate RESTORE.
   Treat expiry as temporary and re-restore only through a new authorized job.

### Promotion

1. Confirm the source generation is readable. Restore `GLACIER` first and wait
   for verified readiness; promotion cannot read an archived source.
2. Submit and follow promotion:

   ```sh
   dsgctl pack promote <pack_id> <generation>
   dsgctl job show <job_id>
   ```

   The job writes and verifies a distinct immutable `STANDARD` generation,
   then performs canonical location CAS. Check the returned destination and
   verify the canonical payload. The original generation remains retained;
   promotion is not a rollback switch and is not permission to delete source
   data.

### Garbage collection

1. GC is destructive. Require the data owner and operator approval, a verified
   recovery/retention state, and the exact generation. Start with a dry run:

   ```json
   {"pack_id":"<pack_id>","generation":"<generation>"}
   ```

   ```sh
   dsgctl job run gc --params-file gc.json --dry-run
   dsgctl job show <dry_run_job_id>
   ```

   The dry-run result is observational. Review root reasons, canonical
   locations, referenced/unreferenced counts and reported byte/count summaries.
   It does **not** enumerate physical S3 version IDs or candidate versions, and
   it issues no physical deletion. Do not treat `job show` as evidence of the
   exact versions a live run would delete. Before destructive live GC, obtain
   exact physical-version evidence through a separately approved, read-only
   inventory/audit path and retain that evidence with the approval. The live
   path rechecks its own inventory; the dry-run is not a promise that a later
   live run sees the same objects or versions.
2. Only after a separate approval, run the same exact-generation job without
   `--dry-run`. The runtime re-marks roots, checks active pins and retention,
   reserves maintenance, binds exact S3 version IDs, and requires verified
   recovery-cut protection before deleting retired data. If any proof is
   unavailable, do not bypass it. Follow `job show`; verify the terminal result
   and remaining exact S3 versions through the approved operator audit path.
3. Deleting an exact S3 version is irreversible unless a separately verified
   backup/version is available. Do not promise rollback after GC dispatch.
   If the job API returns `job_type_unavailable` (HTTP 501), the selected
   runtime did not install this executor; stop and hand off instead of
   substituting direct S3 DELETE calls.

## 3. Checkpoint and rebuild

### Checkpoint

1. Confirm the requested target epoch is sealed and the recovery catalog/anchor
   chain is complete. `recovery_checkpoint` takes an immutable `target_epoch`;
   it never silently advances the requested target.
2. Validate without publishing support first:

   ```json
   {"target_epoch":0}
   ```

   ```sh
   dsgctl job run recovery_checkpoint --params-file checkpoint.json --dry-run
   dsgctl job show <job_id>
   ```

   The successful dry run reports a verified chain and `published:false`.
   Missing, unsealed or corrupt targets fail closed.
3. With an approved target, run the same job without `--dry-run`. Follow the
   job to completion and retain its checkpoint digest/target result. A
   checkpoint is historical state support, not an automatic restore,
   installation, retention release or GC permission. Checkpointing does not
   close healthy producers merely to satisfy a target. The shared runtime only
   installs this executor when the audited typed genesis/catalog loads. A 501
   or unavailable executor means stop; do not invoke internal code ad hoc.

### Full rebuild / PITR recovery

There is no supported online operator command for full rebuild. Use the
reviewed offline recovery implementation/procedure only when an approved
`Recoverer`, `IndexInstaller`, temporary provisioning role and terminal worker
isolation provider are available.

1. Obtain owner/incident approval for the exact target time/cut and selected
   Region. Validate the PITR source watermark across every table if used; the
   recovery implementation compares PITR output with sealed replay.
2. Close R1 admission, wait for active and resolving leases to drain, stop and
   verify isolation/fencing of all old workers, and keep the target installation
   closed during staging. Preserve source history, source catalog, genesis,
   seals, markers, event pages, attachments and S3 pack generations.
3. Recover only from a supported sealed cut in the retention window. Verify
   artifact chain, exact dependencies, canonical payload bytes, checksums and
   archived-object restore readiness. `HEAD`, timeout, a copied live table or
   an operator-supplied “complete” flag is not proof.
4. Install a new random history and incarnation with the reviewed provider.
   Recheck staging digest, isolation and readiness before final CAS/open. The
   prior history remains historical evidence; do not point old workers at the
   new incarnation or roll the pointer back by hand.
5. Run the post-install checks below. Credentials restored from history remain
   disabled. After security audit, disable/revoke old credentials and issue
   replacements through the normal token/credential workflow. Reopen admission
   only on the selected healthy incarnation.

If staging, verification or installation fails, keep admission closed and
preserve all source and staged artifacts for diagnosis. Resume the same
idempotent recovery when its frozen identity is valid, or obtain a new owner-
approved forward recovery. Never delete staged/source data to “reset” a failed
attempt. Full rollback to the old incarnation is allowed only while the old
history/incarnation is still selected and every old writer is proven fenced;
after the new incarnation is committed, recover forward instead.

### Data effects at a recovery cut

The selected sealed cut is the logical rollback point. Events after that cut
are not installed as current heads/configuration, although their immutable
source journal and pack payloads remain retained by recovery roots. Tombstones
and policy history are replayed as of the exact cut. Rebuild derives used bytes,
live namespace and chunk-reference counters from verified manifests; reserved,
staged and active-upload counters reset to zero. Known incomplete uploads are
installed as Aborted; unjournaled old uploads/parts/leases are not resumed.
Credentials and tokens in the recovered image stay disabled until security
review and reissue. These effects are intentional and must be listed in the
incident/change approval before choosing the target cut.

## 4. Unknown outcomes, fencing and cancellation

When a lifecycle step is `Unknown`, `job show` has a pending step, or the API
reports `fenced`/`r1_busy`:

1. Do not submit another COPY, RESTORE, PUT or DELETE for that action. Do not
   infer success/failure from a timeout, lease expiry, `HEAD` class alone, or
   absence of an ACK.
2. Preserve pack protection and roots. Capture job ID, exact pack generation,
   state revision, request ID, current job/node state and provider evidence.
3. Allow only the runtime terminal reconciler to settle the exact original
   action/step/fence using authoritative provider evidence and current worker
   isolation. Examples: action marker on the exact copied S3 version; exact
   versions for GC; positive restore status and expiry for restore. The runtime
   must reject a stale worker before side effects and prove terminal outcome;
   a human cannot fabricate this proof by editing rows.
4. If proof is unavailable, keep the action unresolved and escalate to the
   lifecycle/recovery owner. Do not clear pending state, release protection,
   force-finish a job or reopen maintenance manually.
5. Cancellation is observed at worker safe points after joined work and a
   durable checkpoint. A cancelled accepted archive restore can still complete
   at S3; continue observing it rather than resending it.

## 5. Health, readiness, drain, fence, clock and recovery signals

Scrape only the private health listener (`:9100` Cloud VPC address or Edge
loopback). `/healthz` is process liveness; `/readyz` is cached dependency/clock
readiness and independent local drain state. Neither endpoint is shared R1
authority. `/metrics` does not query AWS on scrape.

| Signal | Where to read | How to interpret |
| --- | --- | --- |
| Process health | `GET /healthz` | 200 means the HTTP process is alive only. It does not mean storage, clock, R1 or S3 is usable. |
| Readiness | `GET /readyz`; `miles_ready{mode="runtime"}` | Expect 200 / value 1. 503 / value 0 removes the node from routing readiness; investigate dependency probe, clock, and drain. The cached probe must be younger than 5s. |
| Drain | `miles_draining`; `miles_active_requests`; `dsgctl node ls` | `miles_draining=1` means local admission is closed. Wait for active requests to drain. Node view shows heartbeat, readiness, draining, `drain_applied`, `drain_state`, and active requests. `gated` is not Cloud shutdown completion; Cloud `complete` requires shutdown and ALB `unused` proof. Edge reports gate-only. |
| Probe freshness | `miles_dependency_probe_age_seconds`, `miles_dependency_probe_failures_total`, `miles_runtime_observation_age_seconds` | `-1` probe age means no observation yet; stale/unusable probes make the node unready. Runtime observation age must remain below 60s; shared runtime gauges are omitted when their observation is stale. An increasing failure counter is a prompt to inspect dependency and clock logs, not a direct cause label. |
| R1 gate / closure | `miles_r1_gate_closed_seconds`; owner-authorized `GetR1Gate` | Positive duration means the shared gate has not been Open continuously. Correlate with intentional `Closing`, `Maintenance` or `RecoveryCut`, `Active`/`Resolving` counts and the durable gate revision. This metric is not a lease/fence proof and has no universal alert threshold. |
| Fence | Admin/job result, `job show`, node/job logs; durable R1 lease/gate through the owner read path | There is no dedicated exported `miles_fence_*` metric. HTTP `fenced`/`r1_busy`, a stale job/node incarnation, or a nonterminal pending step means stop retries and reconcile. Job views deliberately omit raw writer fences. Never treat `miles_ready=1` as a fence. |
| Clock skew/uncertainty | `chronyc -n -c tracking` on the host; `/readyz`; dependency-failure counter | Check selected reference ID, stratum/leap status, update age, last offset, root delay/dispersion and skew. The adapter requires the configured reference, synchronized finite observations, absolute offset ≤2s, and projected total error ≤2s through its 45-minute horizon. No numeric skew gauge is exported; a failed probe keeps readiness false. Do not “fix” a failure by stepping the clock while the service is running. |
| Oldest unexported event | `miles_journal_export_lag_seconds` when deep observation is enabled and fresh | This is age in seconds of the oldest pending outbox export, not an item count. Zero means no pending event was observed. A rising value means export is behind; correlate with `miles_runtime_observation_age_seconds`, `miles_ddb_throttles_total`, retries and exporter job state. The gauge is absent when deep scans are disabled, incomplete or stale; absence is not zero. |
| Recovery cut age | `miles_recovery_cut_age_seconds`; recovery job/catalog | Age is based on the latest sealed cut (or the initial schedule before the first cut). The expected schedule is hourly; >3600s is an operational alarm threshold in the shared acceptance plan. A young cut is not proof of complete checkpoint, valid payloads or permission to delete data. |

Example read-only checks through the approved tunnel:

```sh
curl --fail "$HEALTH_ENDPOINT/healthz"
curl --fail "$HEALTH_ENDPOINT/readyz"
curl --fail "$HEALTH_ENDPOINT/metrics"
dsgctl node ls --limit 100
dsgctl job show <job_id>
dsgctl pack show <pack_id> --limit 100
```

Compare counters as deltas over a fixed interval; they are process counters,
not cluster-wide rates. Compare gauges only when their observation-age metric is
fresh. `node ls` and job detail are bounded snapshots, not an atomic cluster
inventory.

## 6. Cold demo plan (use existing state; no restore-time promise)

The retained evidence at
[`cold-glacier-live/result.json`](../testing/evidence/cold-glacier-live/result.json)
is an AWS CLI synthetic-object demo dated 2026-11-10 in `ap-southeast-2`: restore
was accepted, a GET during pending was rejected, later HEAD reported completion
and expiry, and the 54-byte payload checksum matched. It explicitly records
`production_miles_end_to_end=NOT_RUN`; do not present it as a Miles-on-AWS
end-to-end or as an AWS restore SLA. The observed ~88 seconds from acceptance to
first completed observation is one run, not a timing commitment.

### Preparation

- Prefer a pre-staged, owner-approved Cold pack generation already present in
  the target Miles environment. Confirm `pack ls --state Cold` and `pack show`
  before the session; record class, generation, protection, state revision and
  any existing restore expiry.
- If no suitable live Miles generation exists, use the retained synthetic S3
  evidence and/or deterministic Moto/virtual-clock tests. Label these as
  backend evidence or a fixture, not a live Miles pack demonstration. Do not
  create a new Glacier object, start a restore, or incur storage/restore charges
  solely to fill a demo slot without explicit owner approval.
- Prepare the Admin tunnel, system-admin token file, stable test object key,
  expected checksum, and a screen with `pack show`, `job show` and relevant
  health metrics. Keep credentials and private identifiers out of recording.

### Live Miles sequence (only if an approved pre-staged generation exists)

1. Show the pack's exact generation and current state; show `/readyz` and fresh
   observations. State that `GLACIER` payload is not readable yet. For
   `GLACIER_IR`, demonstrate direct readability instead of starting restore.
2. Submit `dsgctl pack restore <pack_id> <generation> --days 1 --tier Bulk` and
   display the returned job ID. Poll `job show`; explain `restore_accepted` as
   provider acceptance, not content readiness. Show the job waiting/progress if
   it is still asynchronous.
3. If the provider completes during the session, re-read `pack show`, verify
   positive restore status/expiry and perform a client read with checksum. If
   it is still pending, end the live demonstration with the job ID, pending
   state and the agreed follow-up owner/time. Do not hold the session open or
   promise completion within the meeting.
4. Explain follow-on promotion only after source readability; do not run
   promotion or GC during the demo unless separately planned and approved.
   Retain evidence, exact state and timestamps without tokens or credentials.

## 7. Post-operation closeout

- Confirm all jobs terminal or have a named owner for a durable pending/unknown
  reconciliation. Confirm no stale node incarnation can still dispatch.
- Re-check readiness, drain and gate state; reopen admission only through the
  owner-authorized revision/fence-aware procedure once the chosen runtime is
  healthy. For rebuild, confirm the new history/incarnation and credential
  audit before serving requests.
- Save job IDs, exact pack generations, before/after state, result/checksum,
  error/request IDs, and observation timestamps. Report unknown outcomes and
  unverified behavior explicitly; never mark the operation successful from a
  timeout or a single HEAD response.

## Checklist mapping

1. Tiering, restore, promote, GC, checkpoint and rebuild procedures, including
   prerequisites, stop-service boundaries, verification and unknown outcomes:
   §§1–4.
2. Health/readiness, drain, fence, clock skew, oldest-unexported and cut-age
   signals with interpretation: §5.
3. Cold demo using existing evidence or a pre-staged state, asynchronous
   progress, and no restore-time promise: §6.
