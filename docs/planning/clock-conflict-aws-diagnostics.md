# AWS clock freshness and PUT conflict diagnostics

Goal: identify why Chrony stops accepting fresh measurements under load, verify a
host correction with the existing clock bounds, and correlate any PUT 409 with
its actual backend cause. A successful replay alone does not close either finding.

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
export cursor and failure behavior. Identify whether time is spent acquiring
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
results, conflict findings, shutdown proof, final audit and cost estimate. Keep
Draft if clock or conflict acceptance remains unresolved.
