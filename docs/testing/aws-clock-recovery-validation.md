# AWS clock recovery and transaction diagnostics

Run date: 11 November 2026 (UTC). Status: live acceptance **not achieved**; cleanup is in progress. No further workload will be dispatched.
Initial Cloud/Edge runtime: `ad317688bbf8363c619e8f4a68997cc5c306456a`.
Edge-only CreateBucket fix: `783ace07a481ed1b1109ae4a8fbcdac0834ba6e2`.
Repository head `7a454d6` packages a revised candidate which was **not installed**.
ARM64 release SHA-256: `579dc555f1474a312d0a47ee10e9600e3564426aca6f79ec8fe1a24a5f6e77c1`.

## Scope and setup

Singapore, two Cloud gateways, one Edge and one client, all on AWS. Four private
`t4g.small` ARM64 instances, CPU Standard, disposable encrypted 20 GiB gp3 roots,
internal TLS ALB and private endpoints; 117 Terraform-managed resources.
Oracle ran operator CLI only. For the initial baseline/burst comparison, runtime binary, startup configuration,
shared installation and metadata backend were unchanged. A later Edge-only
release changed CreateBucket error classification; that replay is separate.

Chunks max read was 512; all write caps and other table read caps, including
Outbox, remained 128. All 20 caps were read back natively before load. IAM and
paid execution were separately reviewed; root performed IAM/audit only and the
deploy role performed workload mutations. Bootstrap/state and account baseline
are outside the disposable workload.

## Results

Counts are successful PUT / HEAD / SHA-256 GET operations. Each trial used a
fresh synthetic user/domain/bucket and the same 90-file, 754,994,670-byte dataset.
GET counts include verification and three repeat passes. Client retries were
disabled; retained data and failed evidence were preserved between trials.

| Trial | HTTP and counts | Clock window | Tracking samples | Max reference age (s) |
|---|---|---|---:|---:|
| Baseline C1 | passed 90/90/360 | UNVERIFIED | 273 | 32.4121 |
| Burst C1, cycle 1 | passed 90/90/360 | OK | 265 | 18.1575 |
| Burst C4, cycle 1 | passed 90/90/360 | OK | 219 | 18.8206 |
| Burst C1, cycle 2 | passed 90/90/360 | OK | 268 | 17.4460 |
| Burst C4, cycle 2 | CreateBucket 500 before benchmark | Setup failed | — | — |
| Fixed-source C1 | failed 90/90/320; one GET failed, 39 skipped | UNVERIFIED | 250 | 32.7878 |
| Four-attempt candidate | NOT RUN | NOT VERIFIED | — | — |

The initial baseline C1 harness attempt stopped before constructing the benchmark
client because its dataset directory was root-owned `0700`. It is excluded from
this table. Ownership was corrected without changing the manifest hash, and the
preflight now verifies every file as the benchmark user before CreateBucket.
An earlier bootstrap configuration was rejected before installation because a
generated operator node ID exceeded 48 characters; no native R1 gate/token had
been created. Both operational checks are in the [runbook](edge-aws-runbook.md).

## Native clock evidence

The baseline uses packaged `deploy/chrony/aws.conf`, fixed polling at 16 seconds.
All 273 in-window tracking samples were valid and no HTTP request failed. The
strict summary remained UNVERIFIED: extrapolating the sample at 12:11:33.183 to
the next observation at 12:11:35.194 reaches age 34.422 seconds against its
34.4-second limit. Native tracking logged a new update at 12:11:33, before that
limit. This is an unresolved observation span, not a reproduced runtime clock
failure; its original verdict is retained.

Native measurement flags were `111 111 1101`: the third quality test,
`maxdelaydevratio`, rejected the measurement. For example, peer delay at
12:11:17 was 2.079 ms versus roughly 0.2 ms normally; the next good measurement
arrived 16 seconds later. Since baseline profile installation, native logs
captured 116 fully accepted measurements and 13 such quality rejections.
[Chrony documents these measurement flags](https://chrony-project.org/doc/4.3/chrony.conf.html#log).

After a verified clean Edge stop (exit 0 and `shutdown complete` from the exact
systemd invocation), the operator selected the already packaged
`deploy/chrony/aws-burst.conf`: `minpoll 3 maxpoll 4 burst`. Its exact config
SHA-256 is `42780781502650e15917539ba7c4941ecc15e134fcfa3459016e2dcec5bc254f`.
Startup configuration and binary hashes were unchanged; clock bounds settled
before the gateway restarted. Cloud remained the comparison deployment.

At 12:24:41, 12:24:59 and 12:28:15, rejected measurements at poll 4 were followed
by accepted measurements approximately two seconds later (native log resolution
one second). Filters still rejected the bad samples. This directly demonstrates
bounded recovery bursts; it does not establish availability during an NTP
outage. No runtime freshness/uncertainty bound, measurement filter, collector
acceptance rule or retry policy was relaxed.
[Chrony explains the bounded burst behavior](https://chrony-project.org/doc/4.3/chrony.conf.html#burst).

## PUT 409 diagnostics

The historical PUT 409 has not been reproduced in the completed trials; do not
claim its original cause is fixed. The new logging was verified against real
AWS: 97 request-scoped cancellations in baseline C1 carried both the generated
S3 request ID and native AWS request ID. Allowlisted reasons were
`TransactionConflict` and `ConditionalCheckFailed`, on SystemConfig. All that
trial's HTTP operations still succeeded: native CAS cancellations can be retried
internally, so a cancellation warning alone does not prove an HTTP 409.

On the next 409, correlate request/stage, AWS request ID, table/action index and
reason, then inspect lease/reservation/object outcome. Raw cancellation messages,
items, keys, values, tokens and signed URLs remain excluded from these logs.

## CreateBucket during the scheduled epoch cut

At 12:56:37–39, the final C4 setup received three CreateBucket `InternalError`
responses, before any benchmark PUT. Server request diagnostics identify
`r1_gate_busy`; native Chrony measurements at 12:56:22 and 12:56:38 passed every
test. A consistent native read confirms no bucket was published; the R1 gate
subsequently reopened in epoch 1. This failure is separate from the historical
PUT 409.

The admission layer correctly returns `ErrR1Busy`, which already wraps
`ErrUnavailable`. CreateBucket wrapped that storage error and the router fell
back to HTTP 500. The fix reuses `MetadataWriteError`, producing retryable
`ServiceUnavailable`/503 for busy, fenced or unavailable authority and retaining
403 authorization failures and 409 revision conflicts. No transaction is retried
or fence bypassed. HTTP regression/race tests and native shared-runtime tests
pass. The completed old-source trials above are retained; fresh trials on the
final source remain required.

## Consecutive rejection and shutdown failure

The fixed-source C1 completed all 90 PUT and 90 HEAD, then failed one GET in the
last repeat phase: 320 successful SHA-256 GET, one failed and 39 skipped. The
server recorded `object_get_stream` / `canceled` at 13:42:28.782, request
`514CC88972A391B5`. Its exact cancellation cause was not logged and remains
correlated with the independently observed clock failure.

Native logs show rejected samples at 13:42:11 and 13:42:13. The two-attempt burst
was exhausted; the next scheduled response at 13:42:29 was rejected too, and the
next good measurement arrived at 13:42:31. Tracking still referenced
13:41:54.530 with interval 16.1 seconds and an age limit of 34.2 seconds. At the
stream failure, that reference was approximately 34.252 seconds old. A raw
sample just after the failed client window observes age 34.7999, then 36.8087.
The strict window summary retains UNVERIFIED; the additional samples prove a
real stale span, rather than only the baseline's conservative observation gap.

Repository commit `7a454d6` changes the packaged candidate to
`minpoll 2 maxpoll 4 burst`, permitting up to four bounded attempts at poll 4.
All 37 telemetry/deployment tests pass; filters and runtime bounds are unchanged.
The archive was copied to Edge but **never installed or run**: the required
pre-rollout clean-stop gate failed. Do not count the earlier three passing trials
as acceptance of this revised candidate or as two complete final-source cycles.

The Edge service exited 1 at 13:55 with
`shutdown journal flush: context deadline exceeded`, after its eight-minute
flush budget. The exact invocation had no `shutdown complete`. Its process was
gone; rollout stopped and no further test load was dispatched. Earlier two Edge
stops had complete success proofs. The flush timeout now requires a separate
native reproduction/classification before another paid run; it is not fixed by
changing a clock polling profile.

## Cleanup and limits

Cleanup in progress: native logs exported, four VM/root IDs frozen, Edge process
gone, Cloud scaled to zero and draining. The reviewed delete-only plan covers
exactly 117 managed resources; twenty table protections have been removed in a
private cleanup copy. Purge only after all application processes are gone, restore
IAM, remove run-owned service roles/auditor and verify strict absence. Preserve
bootstrap/state; only the new key/secret may remain in approved 30-day deletion.

Approved envelope: 10 USD before tax, at most four hours including cleanup from
11:36:41 UTC; no new tests after 14:26:41, cleanup starts by 14:36:41 and completes
by 15:36:41. Initial Pricing API estimate was 6.42 USD with reserves, not a billing
hard cap. Final cost evidence and independent cleanup verdict are pending.

This is functional clock/conflict diagnosis on small instances. It does not
replace sizing/performance acceptance or the already accepted unrelated
recovery/GC/alarm suites. Keep the PR Draft while the original 409 diagnosis
remains unresolved.
