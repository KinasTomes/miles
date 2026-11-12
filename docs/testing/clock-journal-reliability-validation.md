# Clock and journal shutdown follow-up validation

Updated 12 November 2026. PR #86 implements focused offline fixes after the
[previous AWS run](aws-clock-recovery-validation.md). **This is not new AWS
acceptance.** The [execution plan](../planning/clock-conflict-aws-diagnostics.md)
defines the remaining live gates and cleanup.

## Reproduced mechanisms and changes

| Mechanism | Before | Change and proof |
|---|---|---|
| Renewal cancellation loses its cause | `keepAlive` discarded the renewal error and used plain cancellation. Tests for clock uncertainty, fencing and unavailable backends all received only `context canceled`. | Cause-aware cancellation retains the first failure. Read operations preserve it when transport/metadata returns cancellation; stream logs classify fixed error codes without raw messages. Stop still joins the heartbeat and successful reads stay successful. |
| Later-page throttle loses discovery | A successful first physical scan page followed by `ThrottlingException` returned `ErrUnavailable`. A new flush restarted discovery. The focused reproducer failed before the correction. | Retry the same physical Scan input with bounded SDK backoff, retaining candidates and cursor inside that discovery call. Attempts and the caller's context remain bounded; validation errors are not retried and incomplete scans never return success. Repeated native cursors fail closed. |
| Receipt CAS retry loses its fence | Concurrent exporters triggered a native transaction retry after `MarkOutboxExported` had cleared the captured receipt's `Worker`. The next attempt queried an empty lease ID and DynamoDB Local returned `ValidationException`. | Save attempt authority separately before clearing the durable receipt's worker. Every transaction attempt checks the original fence. A deterministic injected transaction conflict now succeeds on the second attempt; durable progress remains independent of that attempt. |

The scan change does **not** add an ordering index, avoid every full-table scan,
retain an in-memory cursor across crashes, or guarantee progress under sustained
capacity starvation. The existing frozen schema and sorted logical cursor remain
unchanged. After retry exhaustion, a fresh flush may still rediscover work. The
live run must establish whether these bounded corrections meet the existing
eight-minute shutdown budget at Outbox read cap 128.

Shutdown progress logs use only constant stage codes: authority acquisition,
epoch discovery, historical sealing, current-page export, flush completion and
authority closure. Stage logs do not replace exact-invocation exit-zero and
`shutdown complete` proof. Request/AWS-ID transaction diagnostics remain enabled;
no object keys, tokens or raw backend errors are added to these diagnostics.

## Verification

- `go test -race ./internal/coordination ./internal/readpath ./internal/api/s3
  -count=1`: PASS. Includes renewal-cause retention, first-cause ownership,
  parent cancellation, ordinary stop/join, read failures and redacted stream
  classification. Existing HTTP/stream-abort contracts remain covered.
- Focused DynamoDB Local and Moto native race suites: PASS. Includes 134
  historical producers and seven current producers, concurrent exporters, a
  throttled later physical page, two immutable S3-verified seals, existing
  lost-ACK/takeover/closure tests, shared Cloud/Edge runtime and journal S3 faults.
- Deterministic native receipt CAS-retry regression: PASS twice with the race
  detector. This separately verifies the concurrency failure above.
- Outbox unit/race tests: PASS for logical ordering/filtering, retained physical
  discovery, finite retry exhaustion, cancellation/deadline, validation errors
  and looping cursors. These inject service errors; they do not reproduce AWS
  capacity enforcement.
- `make check`: PASS, including all Go packages, 184 Python tests, document-name
  checks, vet and builds. Native tests above use explicitly configured loopback
  fixtures; local runs without those endpoints skip the corresponding suites.

Temporary Docker projects expose only loopback ports, use dummy credentials and
are removed after the checks. No AWS credentials enter fixture processes.

## Clock and conflict limits

The recorded accepted reference and subsequent ages are replayed against the
unchanged 34.2-second freshness bound: observations before the bound pass, the
34.252-second incident and later stale observations fail closed, and a newly
accepted reference restores bounded time. This validates Miles' guard, not
Chrony's packet filtering or the physical cause of NTP delay variation.

`deploy/chrony/aws-burst.conf` already contains `minpoll 2 maxpoll 4 burst`.
It remains a candidate requiring configuration validation on the AWS host and
two complete final-profile C1-to-C4 cycles with complete telemetry. No filter,
freshness bound, uncertainty guard, lease budget or clock-step restriction is
relaxed by this follow-up.

The original PUT 409 remains unclassified. Retain request/stage/AWS transaction
correlation and reconcile authoritative outcomes if it occurs. A passing replay
or unrelated internally retried CAS conflict cannot close that finding.
