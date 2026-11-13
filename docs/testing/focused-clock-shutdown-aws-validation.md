# Focused AWS clock and shutdown validation

Run date: 12 November 2026 UTC. **Shutdown acceptance not achieved. Cleanup independently verified.**

Runtime source: `376d1308dd2091487651e2c575eaa9f5eccc236f`.
ARM64 release SHA-256: `d4ea898be748506c6ec6771c468acaaa1e9f6c2612cf26018c55a21b105a7d72`.
The subsequent documentation commit does not change the tested runtime.

Singapore topology: two Cloud gateways, one Edge and one client on private
`t4g.small` ARM64 instances with CPU Standard, four encrypted 20 GiB root disks,
an internal TLS ALB/private endpoints and shared S3/20 DynamoDB tables.
Terraform managed 117 resources. Oracle only coordinated the run. Native readback
confirmed Chunks read cap 512, every other read cap 128 including Outbox, and
all write caps 128. The approved envelope was 10 USD before tax and four hours
including cleanup; it was not a billing hard cap.

## Clock evidence

C1 and C4 mean client concurrency one and four respectively. Each complete trial
performed 90 PUT, 90 HEAD and 360 SHA-256-verified GET operations with client
request retries disabled.

| Trial | PUT / HEAD / verified GET | Strict clock verdict | Tracking samples | Maximum reference age |
|---|---|---|---|---|
| Baseline C1 | 90 / 90 / 360 | UNVERIFIED | 262 | 33.0893 s |
| Four-attempt candidate C1 | 90 / 90 / 360 | OK | 247 | 17.4323 s |
| Four-attempt candidate C4 | 90 / 90 / 360 | OK | 199 | 17.0121 s |

Baseline had no invalid observed tracking samples or failed collector commands.
The sole unverified span extrapolated the old reference 0.097532 s past its bound
between 03:38:55.283 and 03:38:57.292. Native tracking logged an update at 03:38:55,
before the old deadline 03:38:57.195678 even allowing one second of log precision.
This does not prove a real stale span; the strict verdict remains UNVERIFIED.
The recorder exited 1 because of that verdict, not because chronyd crashed.
Baseline C4 was not dispatched.

Before profile rollout, Edge stopped cleanly in 47 seconds, with exit 0 and
`shutdown complete` from the exact invocation. Its native stop window had zero
throttles; this is not stressed-shutdown acceptance.

The candidate uses packaged `minpoll 2 maxpoll 4 burst`, configuration SHA-256
`9ef26df7116601ea4f7658d537ba60365b0747ac24464f7ad6ed3e33f712353a`.
Binary, backend, filters and Miles clock guards stayed unchanged. Native candidate
C1 logs show isolated delay rejections at 04:03:25 and 04:07:30, followed by accepted
measurements two seconds later. C4 had no rejected samples. Both candidate
recorders exited 0. CPU credit balance 9.64–13.96 and zero surplus charges do not
support credit exhaustion as the cause.

This establishes one final-profile C1/C4 cycle. At owner steering, the extra C1
was interrupted after 89 successful PUTs, one skipped PUT and no HEAD/GET phase.
No second C4 was dispatched. The original two-cycle criterion was not completed;
the remaining window was used for native throttling and cleanup.

## GET cancellation and PUT 409

Candidate C4 verified all 360 GETs without retry, while the gateway logged three
`object_get_stream/canceled` records at 04:14:24, 04:15:22 and 04:16:55. No client
byte/hash failure was observed. A deterministic local reader reproducer shows
cancellation after the final verified emit can return an error after all 4,508
fixture bytes are delivered. That mechanism fits the successful client results;
it does not establish the cause of each AWS request. The three request IDs lack
proxy byte-count evidence because access logging was disabled.

Sanitized proxy request-ID/status/byte-count logging was subsequently enabled
without changing routing or timeouts. The interrupted extra trial issued only
PUTs, so it added no GET correlation evidence. Keep late-cancellation diagnosis
open; do not add blind GET retries to successful requests or conflate these logs
with a proven clock safety failure.

The original PUT 409 did not recur. Request-ID/AWS-ID transaction correlation
remained enabled. Non-recurrence does not establish a repair or classify the
historical conflict.

## Shutdown and native throttling

The natural hourly cut closed epoch 0 with 1,125 producers and 1,242 events.
During final shutdown it became Sealed and all historical events had export
progress. An independently built read-only S3 verifier checked the immutable cut
without metadata reads or writes. This does not prove current-epoch completion.

After client mutations quiesced, two read-only pulses ran on the AWS client:

| Pulse | Workers / Scan page limit | Duration | Successful calls | Throttle responses | Read units |
|---|---|---|---|---|---|
| First | 4 / 128 items | 120 s | 390 | 0 | 17,550 |
| Second | 4 / 1,024 items, still bounded by AWS's 1 MiB page | 120 s | 160 | 225 | 40,208 |

Pulse retries were disabled and the Outbox cap remained 128. CloudWatch
independently recorded max-on-demand read-limit throttles, including minute bins
04:38=252, 04:39=118, 04:40=94, 04:41=106 and 04:42=96. Metrics may arrive late and
are not per-request causality proof. The second pulse ended at 04:39:15; native
throttles continued after it.

Edge invocation `cf4937b203fd49d5bf5e370c85c925d2` began shutdown at 04:34:06.
Its stages acquired authority/discovered epochs at 04:34:12, completed historical
sealing and entered current-page export at 04:37:24, then repeatedly closed and
reacquired authority and restarted current-page export. It exhausted the unchanged
eight-minute flush budget and exited 1 at approximately 04:42, with no
`shutdown complete`.

Bounded physical-page retries preserve discovery within a call, but retry
exhaustion can still restart the outer flush. Full-table scans remain. Current
logs show the stage, not the classified work error of each attempt; do not claim
that every retry or the entire timeout has one proven cause. The offline injected
throttle proof was insufficient to establish sustained-capacity acceptance.

At the failure observation, epoch 1 had 405 events: 148 with export progress and
257 without it. All 257 retained payloads matched their declared SHA-256; none
were missing or mismatched in that non-atomic snapshot. Missing progress does not
establish missing S3 bytes or data loss. After the Cloud peers terminated
naturally, a second quiescent snapshot recorded progress for all 405 current
events and zero pending events. This later progress neither changes the failed
Edge deadline nor independently verifies every current S3 receipt.

## Evidence retained

Private artifacts live under the run directory `focused-recheck-20261112T023551Z`;
they contain credentials or account data where necessary and are not committed.
The public report summarizes these receipts:

| Evidence | Artifact |
|---|---|
| Baseline classification and candidate rollout | `baseline-clock-classification.json`, `candidate-profile-rollout.json` |
| Client results, telemetry and native Chrony logs | `edge-baseline-c1-r1-*`, `edge-four-c1-r1-*`, `edge-four-c4-r1-*` |
| Clean stop before rollout and failed final stop | `edge-stop-before-burst-proof.json`, `edge-final-stop-proof.json` |
| Native pulses and CloudWatch readback | `native-outbox-read-pulse.log`, `native-outbox-large-page-pulse.log`, `ddb-window-final-flush-after-pulse.json` |
| Independent historical cut proof | `verify-cut-build-proof.json`, `independent-current-cut-proof.json` |
| Pending source verification and later progress | `pending-payload-snapshot-verification.json`, `shutdown-throttle-result.json` |
| Quiescent snapshots before purge | `failure-metadata-snapshot/`, `snapshot-preserved-hashes.json` |
| Cleanup audit and estimated cost | `final-absence-summary.json`, `cost-observed-estimate.json` |

The table snapshots were captured again after peer shutdown; the final frozen
copy reflects that later quiescent state. The original 148/257 observation and
payload verification are retained separately with their timestamps.

## Cleanup and limits

Terraform destroyed all 117 managed resources after the runtime processes were
gone. All four VM/root disks and synthetic data were removed; 4,876 S3 versions
were purged. Cloud nodes terminated naturally, without forcing lifecycle-hook
completion. The imported ACM certificate was deleted. Deploy policies/boundary
were restored to baseline, and the temporary cleanup policy and auditor were
removed. New KMS/secret deletion uses the approved 30-day window; bootstrap and
account baseline resources are preserved.

The independent final audit at 05:18:12 UTC accepted all 88 checks: 86 absent,
two approved KMS/secret pending deletions, zero unexpected errors and zero
unapproved residuals. Both run-created service-linked roles were deleted; IAM
matched baseline. The final audit retained the pending-deletion distinction
(`PASS_WITH_PENDING`), rather than claiming every resource was already absent.
Resource/IAM cleanup and its audit completed before the 07:05 UTC deadline.
At operator closeout at 08:05 UTC, CLI sessions were logged out, the temporary
auditor profile/cache was removed and login-code/URL files were cleared.
Credential export then failed for both operator profiles, as expected.

The conservative observed estimate is 3.89 USD before tax, including 2.70 USD of
reserves. This is not a final bill.

No unrelated full recovery, GC, alarm or node-replacement suite was repeated.
No clock guard, uncertainty filter, fencing predicate, lease or shutdown budget
was relaxed. The [follow-up plan](../planning/clock-conflict-aws-diagnostics.md)
retains sustained-capacity shutdown progress, classified attempt errors and late
GET cancellation as open work. Another paid run needs a fresh concrete approval.
