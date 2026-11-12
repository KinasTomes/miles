# Follow-up plan: clock freshness, journal shutdown and PUT conflicts

Updated 12 November 2026. This is the follow-up handoff for
[PR #76](https://github.com/KinasTomes/miles/pull/76), merged as
`a5255a69d7762b2c104ecfbecf506df653b87f3f`. The follow-up branch is
`fix/clock-journal-reliability`, initialized from that merged main. This plan
does not authorize another AWS run.

## Implementation status and execution checklist

Status: **offline fixes implemented; focused AWS shutdown acceptance failed**. Renewal
causes survive read cancellation, Outbox retries retain successful scan pages,
and export-receipt CAS retries retain their worker fence. The four-attempt
Chrony profile completed one C1/C4 cycle with valid clock evidence. The full
two-cycle criterion was reduced at owner steering; do not claim that criterion
passed. The stressed Edge shutdown still exhausted its eight-minute budget. See the
[local proof and remaining limits](../testing/clock-journal-reliability-validation.md).
Keep the PR Draft until its live scope has evidence for review.

Existing [live evidence](../testing/aws-clock-recovery-validation.md) and the
[verified AWS runbook](../testing/edge-aws-runbook.md) are already on the base.
Reuse them; historical passing trials do not validate a new release/profile.

- [x] Create a dedicated branch from merged main and carry forward this plan,
  evidence and runbook.
- [x] Preserve and classify read cancellation causes (implementation step 1).
- [x] Reproduce later-page throttle discovery loss and concurrent export-receipt
  fence loss; fix both without weakening fencing or closure (step 2). Native
  capacity/time-budget acceptance remains a live gate.
- [x] Replay the recorded clock freshness failure and fresh-reference recovery
  without changing guards or measurement filters (step 3). The packaged
  four-attempt candidate completed one live C1/C4 cycle; the original two-cycle
  criterion was not completed.
- [x] Commit implemented parts separately and record focused/race/native local
  proof. Retain unresolved findings rather than marking them fixed.
- [x] Prepare and obtain approval for the concrete IAM/plan/cost/cleanup package
  for the 12 November focused run; earlier grants were not reused.
- [x] Run the approved comparison and native throttling experiment; record the
  failed shutdown and reduced clock scope in the
  [focused live report](../testing/focused-clock-shutdown-aws-validation.md).
- [x] Finish cleanup: 117 managed resources destroyed, independent audit accepted
  88 checks with only the two approved pending deletions; estimate 3.89 USD
  before tax, not final billing.
- [ ] Correct sustained-capacity shutdown progress and obtain fresh acceptance.
  PUT 409 remains unclassified because it did not recur.

Scope exclusions: no unrelated features, replacement time provider, blind
mutation retries, wider IAM grants, capacity/timeout increases or changes to
clock/lease safety contracts merely to obtain a passing result. Any material
contract or paid-run scope change requires a separate decision.

Goal: make journal shutdown complete under bounded backend capacity, retain the
cause of canceled requests, validate clock freshness under consecutive rejected
measurements, and identify the historical PUT 409. A successful replay alone does
not establish the cause or close a finding.

## Focused AWS run

Use the verified two-Cloud, one-Edge, one-client topology and deployment sequence.
Do not redesign infrastructure to reduce this run's test scope. All runtime
processes and load generators run on AWS; the operator host only coordinates.

1. Freeze one release and enable native Chrony/host telemetry, request/AWS-ID
   transaction diagnostics and bounded shutdown-stage logs before load.
2. Run the clock baseline and candidate comparison with the same runtime,
   installation and table caps. C1 means one concurrent request; C4 means four.
   The final profile still needs two complete C1-to-C4 cycles. Stop on an
   unexplained failure and retain its write outcome before another attempt.
3. After the clock phase, quiesce client mutations. Retain historical/current
   epochs with multiple physical and logical Outbox pages and concurrent normal
   exporters. Compare shutdown at the unchanged Outbox read cap of 128. Capture
   native throttling, scan/retry counts, export receipts, the exact systemd
   invocation and independent S3 closure/seal proof where applicable. Do not
   substitute an SDK-injected error for native capacity evidence. If native
   throttling does not occur, report that this live condition was not exercised.
4. Keep 409 diagnostics throughout both phases. Correlate each occurrence with
   backend reasons and authoritative write outcomes; absence does not close the
   historical finding.
5. Export evidence and clean up using the existing strict runbook. Skip unrelated
   full recovery, GC, alarms and node-replacement acceptance suites. Closures,
   seals and takeover directly affected by these fixes remain regression gates.

This combines infrastructure use, not simultaneous fault experiments. Clock
availability and shutdown capacity must have separate measured windows. The
owner approved this technical scope; refresh the concrete IAM, cost, deadline
and deletion package before paid activation. No previous grant/window is reused.

## Prior diagnostic merge boundary and evidence

PR #76 delivered runtime fixes already covered by focused tests,
safe AWS diagnostics, packaged clock profiles, live evidence and a verified
deployment/cleanup runbook. CreateBucket's busy-admission 500 mapping is fixed;
busy admission still returns retryable 503 and preserves fencing.

Merging this change must **not** be recorded as final clock/shutdown acceptance,
production readiness, or resolution of PUT 409. The two-attempt profile failed
under consecutive rejected samples. The four-attempt candidate has not run on
AWS. The final Edge invocation failed its eight-minute journal flush and lacked
`shutdown complete`. Earlier successful recovery/GC/alarm evidence remains bound
to its reported source; it does not close these findings on a later release.

The paid run is closed: 117 managed resources destroyed, all four VM/root disks
absent, IAM restored and CLI sessions logged out. Independent audit accepted 88
checks with exactly two approved key/secret pending deletions and no unapproved
residuals. The conservative estimate was 4.74 USD before tax, not final billing.

## Implementation order in subsequent PRs

Use the smallest independently reviewable changes below. Complete the offline
gates before preparing a new paid run; follow the detailed AWS sequence later
in this document. No need to rebuild unrelated deployment or acceptance tooling.

### 1. Retain the original request cancellation cause

- Extend the existing lease `keepAlive` owner in `internal/coordination/lease.go`:
  it currently discards `renew()` errors and calls plain `cancel()`. Trace its
  callers through read/stream handling before changing cause propagation.
- Preserve the first meaningful renewal failure through cancellation and safe
  request diagnostics. Distinguish clock uncertainty, fencing, backend failure,
  caller cancellation and an ordinary explicit stop. Keep the public S3 contract
  and existing redaction rules; do not expose raw backend errors or entity data.
- Prove with deterministic tests that each failure cancels the request and
  retains its classified cause, normal stop releases the goroutine, and parent
  cancellation remains distinguishable. Run affected coordination/read-path
  race tests. This improves evidence; it does not itself fix clock freshness.

### 2. Reproduce and correct shutdown progress under Outbox throttling

- Reuse DynamoDB Local/S3 fixtures and the existing fault-injection layer. Seed
  historical and current epochs, more than one logical/physical page, concurrent
  exporters and a throttle on a later scan page. Quiesce request writers first.
- Measure discovery scans, repeated reads, successful export/ack progress,
  authority renewal/close and time spent sealing. Preserve the eight-minute
  deadline and read-cap comparison at 128. Native AWS recorded 518 read-limit
  throttle events during the failed flush; that establishes a capacity problem,
  not that throttling alone explains the whole timeout.
- `ListOutbox` currently scans the full table for each logical page, and a failed
  page can cause a fresh outer flush to restart discovery. Select the smallest
  bounded enumeration/retry correction at the owning storage/recovery layer
  after reproducing this path. Preserve event ordering, exact closure, immutable
  receipts, takeover/fencing and crash-safe progress. Do not infer absence from
  a partial scan or declare success after silently skipping work.
- PASS: transient throttles recover and drain the pending work within the
  existing budget without repeated full rediscovery preventing progress;
  independent S3 verification succeeds where the fixture closes/seals a cut.
  Persistent failure must remain bounded, retain unresolved work and withhold
  `shutdown complete`. Include lost-ACK and takeover regressions.
- The retained snapshot has epoch 1 Closed and 177 events without recorded
  export progress. Reconcile receipts/object versions; do not label those events
  lost solely from missing metadata progress.

### 3. Validate the clock correction and finish conflict diagnosis

- Replay recorded tracking/measurement timelines offline, including consecutive
  rejections and the strict observation-gap case. Real stale evidence occurred
  at reference age 34.252 seconds against a 34.2-second bound; the overlapping
  GET only logged `canceled`, so its exact cause remains unproven.
- Verify the packaged `minpoll 2 maxpoll 4 burst` candidate allows up to four
  attempts at poll 4; retain filters and runtime bounds. This is a candidate,
  not a guarantee against an NTP outage. The physical source of delay variation
  remains unknown; do not assert CPU-credit exhaustion or an AWS NTP defect.
- After steps 1 and 2 pass, freeze a final release and obtain a fresh reviewed
  IAM/plan/cost/cleanup approval. Run the baseline/comparison below and require
  two complete C1 (concurrency 1) to C4 (concurrency 4) cycles on the final
  source/profile, complete clock telemetry and exact-invocation clean shutdown.
- Keep PUT 409 open until a request's backend cause and write outcome are
  established. Correlate server request ID, stage, AWS request ID and cancellation
  reason; expected internally retried CAS warnings are not proof of HTTP 409.
  If it does not recur, report that result and the remaining uncertainty without
  inventing a fix or adding blind mutation retries.

### Follow-up completion evidence

Record the reproducer, fix commit, focused/race test results and remaining limits
for each work item. The final paid report must include release/profile hashes,
two complete cycles, classified request failures, native clock evidence, journal
shutdown proof, strict teardown audit and cost. Update the runbook only with
steps actually verified. The initialization-only boundary applied to the original documentation handoff.
The follow-up now includes the separately authorized implementation and focused
AWS run; that run does not authorize another window.

## Status after the live comparison

Read the [live report](../testing/aws-clock-recovery-validation.md) before another
run. Native delay-filter rejections are now observed. The first burst candidate
recovered isolated bad measurements but exhausted its two attempts on consecutive
rejections; a real stale span and failed GET followed. The revised packaged
candidate permits four attempts and is **not live-validated**. Do not reuse the
three earlier passing trials as final-profile acceptance.

A CreateBucket 500 during the scheduled recovery cut was fixed using the existing
metadata error mapper; the busy admission remains fenced and returns retryable
503. Native bucket outcome was absent. The historical PUT 409 remains unresolved,
although real-AWS request/AWS-ID/cancellation-reason logging is now verified.

Before spending on another run, reproduce/classify the eight-minute shutdown
journal flush timeout with concurrent native shared-runtime supervisors/jobs and
a quiesced request workload. Preserve the original deadline, worker authority,
export cursor and failure behavior. Native CloudWatch records 518 Outbox
on-demand read-limit throttle events in the minute bins overlapping the failed
flush; Outbox remains capped at 128. Reproduce historical-table scans and a
throttled later scan page with concurrent exporters. A failed page currently
causes the outer flush retry to restart discovery; verify progress and authority
before choosing a bounded enumeration/retry correction. Do not raise the cap
without a separate comparison decision. Identify whether time is spent acquiring
recovery authority, discovering/exporting a page, publishing a seal or closing
the worker; use bounded stage/result diagnostics through the existing worker
observer if needed, without entity keys or raw backend errors. A released-lease
snapshot alone cannot prove that journal export completed. Require the exact
invocation's clean-stop proof before any subsequent profile rollout.

Only after that gate and focused regression tests pass, prepare a fresh reviewed
AWS package for two complete C1→C4 cycles on one final source/profile. The
previous paid window does not authorize a new run or a deadline extension.

## Offline prerequisites

- Merge current main and verify the affected Go/race/native shared-runtime and
  packaged telemetry tests. Build a clean ARM64 release and freeze its source,
  archive hash and manifest. Keep the same source/config within each comparison.
- The S3 router supplies its server-generated request ID to native DynamoDB
  cancellation logs. Log only request IDs, table, action index and allowlisted
  cancellation reasons. No keys, values, tokens or raw backend messages. Neither
  retry policy, transaction predicates nor HTTP error semantics change.
- Package the baseline and candidate in `deploy/chrony/`. Both pin the AWS source
  and retain delay filters, clock-step restrictions and runtime uncertainty/age
  guards. Add read-only `ntpdata` to the recorder. Native measurement logs must be
  enabled before bootstrap, not after the first failure.
- Reuse the verified deployment and teardown sequence in the [runbook](../testing/edge-aws-runbook.md).
  Fresh run ID, PKI, login codes, baseline, plans and grants are mandatory. Validate
  all private helper filenames, exact current auditor ARN and rollback documents
  before applying. No workloads on Oracle.

## Read-only review and paid authorization

Refresh deploy/root identities, account and EBS baselines, live Terraform plans,
IAM simulations/validation, regional prices and cleanup resource scope. Root is
limited to IAM and audit; deploy runs workload. Present the concrete package and
hashes for a new paid-run decision before grants or paid resources.

Reference envelope: four hours including cleanup, 10 USD before tax, two Cloud
nodes plus Edge and client, at most five disposable ARM64 VM/root disks, shared
S3 and 20 tables, private endpoints and internal ALB. Actual plans/prices must be
refreshed; this document is not approval or a hard billing cap. Preserve bootstrap
and account baseline, remove only synthetic data/root disks without backup, and
schedule only the new key/secret for 30-day deletion.

After active deployment, the proposed comparison holds Chunks read cap at 512,
all writes and other table caps at 128, including Outbox. Approve this explicitly,
read back native caps, and keep them identical across clock profiles. Do not let
later Terraform applies silently revert the comparison; reconcile the cleanup
copy against the actual native caps.

## Baseline, diagnosis and correction

1. Install verified artifacts with `deploy/chrony/aws.conf` on Edge. Save Chrony
   version, config/hash, bootstrap time, instance/ENA/CPU settings and log start.
   Validate configuration with `chronyd -p -f <file>` before restart. Start the
   native logs and recorder, require growing samples and successful `ntpdata`,
   then dispatch. Preserve the running Cloud source and shared-backend identity.
2. Use a fresh synthetic user/domain/bucket for C1. If it passes, run C4 on the
   same release/config. This baseline is diagnostic: on error stop dispatch,
   drain requests, export evidence and reconcile write outcomes before retrying
   or changing host configuration. Do not repeat blindly to obtain a PASS.
3. Correlate last response, gateway request/stage, tracking reference age, native
   measurement test flags, TX/RX/valid/good-RX deltas, command latency, CPU credits,
   PSI and ENA allowance counters. `Update interval` is retrospective, and valid
   packet receipt does not guarantee an accepted measurement. See [Chrony
   reports](https://chrony-project.org/doc/4.3/chronyc.html) and [measurement logs](https://chrony-project.org/doc/4.3/chrony.conf.html#log).
4. Only if logs show rejected/unavailable measurements and the proposed recovery
   fits the cause, stop Edge cleanly, preserve logs/config and compare
   `deploy/chrony/aws-burst.conf`. It permits bounded recovery bursts with shorter
   minimum polling; it does not disable measurement tests. If the cause differs,
   diagnose that cause before another change. Record each change and its reason.
5. After synchronization settles, restart the same installation and verify its
   history/config. Require two complete C1 → C4 cycles on the chosen final profile:
   each trial 90 PUT, 90 HEAD, 360 SHA-256 GET, no request errors/unknown outcome,
   no clock violation and a complete contemporaneous clock window. Save evidence
   that new good measurements arrive before the runtime freshness limit, including
   recovery from rejected samples if observed. Report the observed limits rather
   than promising availability during an NTP outage.

Do not relax clock bounds, bypass fencing, enable clock steps during runtime or
remove Chrony filters to pass. Keep native service throttling separate from clock
rejections. Bound native log files and collector storage; stop load before log,
CPU-credit, capacity, cost or time limits make evidence unreliable.

For a 409, join the S3 request ID/stage to AWS request ID, table/action and
`ConditionalCheckFailed` versus `TransactionConflict`. Inspect the affected lease,
reservation and object outcome. Apply a fix only after establishing whether the
conflict is a native CAS retry exhaustion, a business precondition or another
path. No raw error dumping or blind transaction retries.

## Stop and cleanup

Stop new tests at T0+180 minutes, begin cleanup by T0+190, complete by T0+240.
Reserve at least 50 minutes for cleanup. A classified baseline clock failure may
continue to the approved comparison after evidence/outcome reconciliation; an
unexplained error, unsafe authority, budget risk or deadline stops new workload.
Do not repeat unrelated recovery/GC/alarm suites already accepted unless changed
code or new evidence requires it.

Export native Chrony logs before destroying hosts. Freeze all current/historical
instance and EBS IDs, state, outputs and out-of-state certificate. Stop gateways,
verify shutdown, purge synthetic versions/multipart data, review protection
removal and apply the exact delete-only plan. Restore IAM and baseline, remove
run-created service roles/auditor, then run the strict independent absence audit.
Retain failed deletion receipts and boundedly retry stale service references;
never weaken the audit to obtain PASS. Logout and clear codes/operator processes.

Update the runbook immediately after a verified step. The final report must name
source/profile/config hashes, baseline failure and correction evidence, C1/C4
results, conflict findings, shutdown proof, final audit and cost estimate. The
follow-up acceptance PR must retain its failed/unverified verdict until its own
criteria pass; merging the diagnostic PR does not change those verdicts.


## Remaining work after the focused recheck

The focused run established real native read-limit throttling while historical
closure/sealing and independent S3 verification succeeded. Edge still timed out
exporting the current epoch, repeatedly acquiring authority and restarting
current-page discovery. Peers later finished all current export progress. Preserve
these separate observations; missing progress at the Edge timeout did not mean
retained producer inputs were lost.

Before another paid run:

1. Add bounded classifications for each failed flush attempt and its stage using
   existing safe diagnostics. The current stage logs cannot identify the exact
   `workErr` behind every outer retry. Never log raw backend messages or keys.
2. Reproduce sustained read-cap pressure using the retained history/current
   inventory sizes, concurrent exporters and service-scale recovery windows.
   The earlier single transient page fault was insufficient to establish this
   eight-minute availability property. Measure completed logical pages and
   repeated discovery separately from raw retry counts.
3. Choose the owning discovery/backoff correction based on that reproducer.
   Retain the frozen schema, ordering, exact closure, fencing and crash-safe
   receipts unless a separate contract decision is justified. Do not simply
   increase the table cap or shutdown deadline.
4. Keep GET late-cancellation diagnosis separate. All candidate C4 client bytes
   and SHA-256 checks passed without retry. A local final-emit cancellation
   reproducer explains how the server can still return an error; the three
   original IDs lack proxy byte evidence. Use the sanitized ID/byte trace to
   classify a recurrence before changing public failure semantics or retries.
5. Scope the next AWS proof to the remaining correction, reuse verified setup,
   and obtain a fresh concrete paid window. Do not repeat full clock cycles or
   unrelated suites merely because infrastructure is available.
