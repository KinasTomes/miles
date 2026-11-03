# Cloud and Edge live validation

## Result

The AWS-only Singapore run on 2026-11-02 passed the native functional acceptance
checks. Two private ARM64 `t4g.small` Cloud nodes served from separate AZs behind
HTTPS ALB; a separate AWS EC2 host ran the Edge profile with Roles Anywhere.
All shared one S3 bucket, 20 DynamoDB tables, history and runtime secret. Oracle
held only source, CLI clients and evidence during this run.

The approved two-GiB instances establish bounded sequential functionality. This
run does not establish production sizing, concurrent capacity or throughput.
Clients explicitly trusted a dedicated test CA; public domain/certificate
provisioning remains a deployment prerequisite for production use.

## Release and native checks

All four gateway processes, including the replacement Cloud node, ran source
commit `b35820182a4d479d4c1cf382b3a64a0d45082f53` from the same archive:

- Archive SHA256: `0a8e2aa9fde15137f8e9b941254b797fc0ca96e3f86d0d9f9e0264e7cf37e007`.
- Binary SHA256: `9a8a6557b6aa21f195d3635168c06250c47abd049c8d88cf8e3dd6bf6c0b0377`.

| Check | Native evidence |
| --- | --- |
| Two-AZ Cloud fleet | Two healthy private ALB targets in different AZs before and after physical replacement; shared storage and private AWS endpoints, no NAT. |
| Shared object path | Signed PUT/GET/HEAD/Range/LIST, overwrite, copy/delete and multipart completion/read-back passed across Cloud and Edge with exact byte comparisons. |
| TLS and SigV4 | Strict HTTPS with signed `Host:443`, Unicode, literal percent/plus, encoded slash, double slash, dot segments and underscored metadata passed. |
| Edge Internet ingress | The Oracle CLI reached the separate AWS Edge HTTPS endpoint over the Internet with certificate verification enabled. This does not rely on reaching a host through its own public address. |
| Management isolation | External TCP probes to Admin 9000 and metrics 9100 failed for both front doors. HTTPS Admin paths returned S3 errors; `/metrics` returned the S3 ListObjectsV1-not-implemented response and no Prometheus data. |
| Edge credentials | Fifteen-minute Roles Anywhere credentials refreshed in the same process and systemd invocation. A write after the original session expired was readable through Cloud. A fresh bootstrap using a failing helper refused startup before listeners, with no ambient/static/IMDS fallback; the serving process stayed unchanged. |
| Owner access | A separate owner token read its scoped usage through pinned SSH forwarding to loopback Admin. The tunnel moved from the draining node to the surviving Cloud node during physical replacement. |
| In-flight drain | A held 16-MiB PUT was attributed to the node selected for termination. Native metrics showed `draining=1` and `active_requests=1` together. After releasing the body, PUT returned 200 and the surviving node returned all bytes with the expected checksum. |
| Physical replacement | ASG termination kept desired capacity unchanged. A new instance became healthy in the vacated AZ; the old instance terminated. CloudTrail attributed lifecycle completion to the gateway runtime role. No operator forced completion. |
| Durable hourly cut | The persisted cut was initialized at 16:03:26 UTC, due at 17:03:26 and observed running at 17:03:51. Epoch zero closed and was Sealed at 17:04:12 with 21 producers. The existing S3-only `VerifyCut` passed; a subsequent write/read passed. No history, gate or deadline was reset. |
| Final shutdown | All four gateway invocations exited successfully and emitted their own `shutdown complete` proof. After shutdown, R1 had zero active/resolving work and no pending actions. |

## Corrections and local proof

The earlier run did not complete acceptance: an unknown commit retained at the
hourly cut blocked further writes and one clean shutdown. Its failure evidence
and cleanup record remain separate; the later passing run does not change that
earlier outcome.

The runtime now reconciles a closed writer's original operation before producer
settlement. `ReconcileR1` CAS-closes the original attempt and settles the producer
receipt atomically. An absent result alone never proves that a commit failed.
Standalone reserved quota is released only after authoritative proof; multipart
staging remains owned by multipart recovery, and physical roots/pins are retained.
Epoch closure revisits reconciled producer receipts. Registry retries occur only
after local work has joined; unresolved physical I/O still prevents clean exit.
See [the reconciliation contract](../contracts/closed-writer-reconciliation.md).

Native DynamoDB Local tests cover unsent commits, committed transactions with
lost responses, a second Store instance, delayed original transactions, lost
closure acknowledgements, idempotent quota accounting, multipart ownership,
retained roots, crash/expiry across the durable hourly cut and Closed/Sealed
progress. Registry tests retain committed results after release and reject false
clean shutdown while work or reconciliation remains unresolved.

`make check` and the scoped metadata/app/coordination/object/write/recovery/readpath
race suites passed with DynamoDB Local and S3/Moto fixtures. The deployment suite
also passed all 14 tests, including installation under umask 077. CI passed all
five checks on the deployed source commit.

Two deployment corrections were proved in the new run:

- Edge's security group needs private TCP 443 access to interface endpoints when
  VPC private DNS is enabled. One internal rule was added; Admin/metrics remained
  private. The active stack had 99 managed resources, including this rule.
- The installer creates missing public package/configuration directories with
  readable permissions under umask 077. Existing private directories retain their
  mode. The rebuilt archive was installed on every gateway process.

Earlier shared LIST cursor-key injection, owner usage probing and the fleet's
128-unit table request ceilings were exercised by the passing sequence. These
ceilings are request limits, not provisioned capacity or a billing hard cap.

## Observation requiring separate characterization

An optional early 17-MiB upload fixture paused for about 90 seconds and received
one `InternalError`. The root cause was not established. A repeated 17-MiB PUT
succeeded, but that fixture's 16-MiB read limit then caused a comparison failure.
Both attempts are excluded from the passing drain evidence above. The corrected
16-MiB held request proved drain overlap, successful completion and exact read-back.

Long-stalled client behavior needs a controlled reproduction before making a
timeout or reliability claim. No speculative runtime change was made from this
single observation. Checkpoint/rebuild, retention/GC, load and performance remain
separate validation work; this run proves their sealed-epoch input, not those
consumers' behavior.

## Cleanup and evidence

Workload teardown completed on 2026-11-03 at about 00:56 UTC. Independent AWS
reads confirmed no run EC2 remained, all four disposable 20-GiB root disks were
absent, and the bucket, 20 tables, ALB/ASG, endpoints, network interfaces and VPC
were gone. The stack state contained zero resources. The temporary ACM
certificate, SSM document, runtime roles/profiles and Roles Anywhere resources
were removed. Original managed-policy defaults were restored at 00:57 UTC;
temporary versions and the cleanup grant were removed. Thirteen local private
key/token/secret files and two temporary public keys were deleted.

The ELB service-linked role remains pending: AWS initially refused its deletion,
reporting the recently deleted ALB in its usage list. The independent ALB API
returned NotFound. The custom ASG and default Roles Anywhere service-linked roles
were deleted. Keep this exception explicit until an IAM retry proves absence.

All gateway processes had stopped cleanly, but the deploy token expired during
the ASG deletion waiter at about 17:45 UTC on 2026-11-02. Terraform saved its
recovery state locally after the backend write also failed. The 18:20 UTC cleanup
target was missed while renewed authentication was pending. A separately
approved 60-minute cleanup grant resumed deletion without launching workloads.
The recovery state matched the backend lineage and the exited apply's lock;
native state serial checks were retained before releasing that owned stale lock.
No history, R1 gate or workload deadline was reset. Cleanup also encountered a
missing IAM instance-profile read for the Edge role; the IAM-only session removed
the identity after verifying its exact ID and absence of profiles/policies, and
the deploy role reconciled the resulting native NotFound in a fresh plan.

The quiescent inventory contained 156 object versions, 62,080,193 physical bytes
including two release archives, no multipart uploads and 386 metadata items.
All versions were deleted with per-object error checks after inventory comparison.
Bootstrap/state and earlier retained resources were excluded from destructive
plans. The following remain outside the empty workload state:

- Twenty SYSTEM backups from this run, identified by the original table IDs.
  Native summaries report expiry on 2026-11-04 around 00:52 UTC. Twenty older
  SYSTEM backups are separate, with expiry on 2026-11-03 around 13:14 UTC.
- This run's KMS key in PendingDeletion, due 2026-12-03 around 00:52 UTC.
- This run's secret scheduled for 2026-12-03 at 00:52:15 UTC. CloudTrail confirms
  a 30-day recovery window; DescribeSecret's DeletedDate alone was not used as
  proof of the scheduled deletion date.
- Existing bootstrap/state and earlier deletion windows.

Private evidence retains saved-plan digests, resource identities, table IDs,
release hashes, signed-client assertions, credential refresh timestamps,
invocation-specific shutdown logs, the immutable seal and S3-only verification.
Keep credentials, private keys, tokens and raw signed requests out of the public
repository. The USD 5 envelope was an authorization limit, not a billing hard cap.
Cleanup did not meet its original deadline; the actual billed total has not been
reconciled. Retained backups, deletion windows and bootstrap storage must not be
described as zero-cost merely because the workload state is empty.
