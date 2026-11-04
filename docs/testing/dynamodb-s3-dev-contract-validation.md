# DynamoDB and S3 dev contract validation

The acceptance harness runs the native adapter against an isolated DynamoDB
inventory and checks the pack write, object commit, read-back, and delete path
against a versioned S3 dev bucket. It uses fixture writer authority. The test
does not exercise deployed Cloud/Edge coordination.

Set these values in the test process before enabling AWS acceptance.

| Variable | Value |
| --- | --- |
| AWS_REGION | Region of the dev tables and pack bucket |
| MILES_AWS_DEV_ACCOUNT | Expected AWS account ID, verified through STS |
| MILES_S3_DEV_BUCKET | Dedicated versioned bucket in the expected account |
| MILES_S3_DEV_KMS_KEY | Existing dev key ARN if required by the bucket policy |
| AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN | Temporary credentials obtained from the dev profile or role |
| MILES_DDB_DEV_ACCEPTANCE | Set to 1 to enable AWS calls |
| MILES_DDB_DEV_MEASURE | Set to 1 to log transaction footprint |

The DynamoDB role needs create, describe, delete and native data operations
on tables under the generated miles_hai_contract_ prefix. The S3 role needs
versioning read, pack and sidecar reads/writes, and version deletion on the
dev bucket. The fixture logs its generated prefix and records each exact
table name before CreateTable. For an uncertain create, cleanup polls for
up to five seconds if DescribeTable keeps returning ResourceNotFound. If the
result is still unknown, cleanup reports the table as unresolved and excludes
it from the verified count. The fixture checks context cancellation before
recording a dispatch attempt. It preserves tables rejected as name collisions
and reports unresolved names with the run prefix.
It deletes pack and sidecar versions by their returned version IDs. An S3
write with an unknown outcome may need scoped follow-up inspection.

Run the following commands on Linux with the temporary environment above.
Use a fresh shell for the credentials and save verbose test output for evidence.

~~~sh
go test -count=1 -v -timeout=15m ./internal/metadata/dynamodb -run '^TestAWSDev(BatchContract|WriteCommitWarmReadHandoff|LostResponseReplaysOriginalOutcome|HotKeys)$'
go test -count=1 -v -timeout=15m ./internal/metadata/dynamodb -run '^TestAWSDevMetadataContract$/^(full_quota|completion_all_or_nothing|unknown_outcome)'
go test -count=1 -v -timeout=15m ./internal/metadata/dynamodb -run '^TestAWSDevMultipartContract$/^replace-part-and-complete$'
~~~

The batch contract splits the first real read, injects throttling on the
pending subset, then lets the backend complete that subset. A separate case
exhausts the retry budget. The reported throttle in this contract is injected.
The lost ACK case forwards the real final transaction before suppressing its
successful response. It reopens the Store and checks the original outcome,
head, ledger, outbox, and hard quota before allowing subsequent operations.

The optional footprint logger reports maximum action count, SDK JSON wire
bytes, largest native Put image, and sum of native Put images. These are
independent maxima for a selected scenario and may come from different
transactions. ConditionCheck actions contribute to the action and wire counts.
The adapter's full native transaction guard also counts observed images for
those actions. Its boundary tests cover the 400 KiB item, 100-action and
4 MiB native aggregate caps.

## DynamoDB Local baseline

The probe used 16 operations with concurrency four. Capacity below is capacity
returned for successful transaction calls. Latencies include adapter retries.
Conflict reasons count observed transaction responses, including responses that
the adapter resolved on retry.

| Workload | Success | Transaction calls | Conflict reasons | Returned capacity | p50 | p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Shared owner | 16/16 | 33 | 17 | 192 | 43.5 ms | 106.0 ms |
| Distributed owner | 16/16 | 16 | 0 | 192 | 14.2 ms | 21.0 ms |
| Shared chunk | 16/16 | 4 | 3 | 6 | 1.5 ms | 16.5 ms |
| Distributed chunk | 16/16 | 16 | 0 | 96 | 9.6 ms | 15.2 ms |

There was one canonical insert for the shared chunk and 16 for the distributed
case. Subsequent shared-key calls can read the winner, so their lower capacity
does not measure the throughput of repeatedly writing one chunk key. All four
local probes reported zero throttling reasons.

| Contract scenario | Max actions | Max SDK wire bytes | Max native Put bytes | Max native Put sum |
| --- | ---: | ---: | ---: | ---: |
| Same-size streaming overwrite at full quota | 11 | 15,286 | 11,207 | 11,207 |
| Full quota overwrite, stale cleanup and delete | 11 | 15,215 | 11,151 | 11,151 |
| Replace a part and complete | 12 | 25,288 | 18,707 | 18,707 |

These are one-run local measurements. Keep AWS results separate and record the
source commit, toolchain, scenario, concurrency, request counts, latency,
conflicts, throttle reasons, and cleanup outcome. Use owner-counter transaction
retries and initial canonical insert contention to select candidates for a
larger follow-up experiment.

## AWS dev acceptance on 2026-11-04

The run used Go 1.27.2 on Linux in ap-southeast-1. Preflight verified the
expected account through STS, AWS DynamoDB access, and enabled versioning on
the dev bucket. The standalone S3 Warm contract passed range reads, immutable
generations, sidecar metadata, and failed or cancelled writes. Its three
created versions were deleted and the two generated pack IDs were checked
for remaining versions and delete markers. The combined handoff passed
write, metadata commit, full and ranged read-back, and tombstone checks.
Its recorded S3 versions were deleted by version ID.

An initial batch run verified retry behavior but was interrupted during
cleanup because the default deletion waiter serialized long waits. All
remaining tables from that exact generated prefix were removed and the
prefix was verified empty. The harness now submits all scoped deletions
before waiting for absence. The rerun passed batch retry and verified
removal of all 20 tables in that fixture.

The AWS quota and multipart cases verified the committed head and outbox,
rejected stale overwrite credit after the head changed, released the stale
reservation twice without double-debiting counters, and denied growth past the
hard quota. The replacement part advanced its revision and replaced the staged
byte total. Complete charged logical bytes once and its retry returned the
stored result. A separately injected lost response was reconciled through a
fresh Store by op_id after a real DynamoDB commit.

| AWS contract scenario | Max actions | Max SDK wire bytes | Max native Put bytes | Max native Put sum |
| --- | ---: | ---: | ---: | ---: |
| Full quota overwrite, stale cleanup and delete | 11 | 15,275 | 11,195 | 11,195 |
| Atomic Complete with lost ACK | 12 | 17,150 | 12,601 | 12,601 |
| Replace a part and complete | 12 | 25,356 | 18,754 | 18,754 |

Both unknown-outcome cases passed. Before commit, the operation remained
unpublished until the reservation claim and replay resolved it. After commit,
the original result and committed ledger survived takeover. Counter checks
passed after each retry. The Linux Go suite also passed with mandatory
DynamoDB Local and Moto fixtures, along with the batch race regression.

| AWS workload | Success | Transaction calls | Conflict reasons | Returned capacity | p50 | p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Shared owner | 16/16 | 37 | 29 | 192 | 810.9 ms | 1,681.8 ms |
| Distributed owner | 16/16 | 17 | 1 | 192 | 456.8 ms | 833.3 ms |
| Shared chunk | 16/16 | 4 | 3 | 6 | 161.9 ms | 446.9 ms |
| Distributed chunk | 16/16 | 20 | 7 | 96 | 298.4 ms | 530.1 ms |

The AWS probe used 16 operations per workload with concurrency four. No
service throttle was observed in this probe. The shared chunk workload
inserted one canonical row, while distributed keys inserted 16. The
shared owner workload needed more transaction calls at the same returned
capacity. This run gives a baseline for measuring the cost of repeated owner
counter conflicts under larger workloads.

The final AWS run verified removal of all 180 tables across nine isolated
fixtures. A final ListTables check returned zero tables with the test prefix.
The S3 handoff deleted two exact object versions and the standalone S3
contract deleted three.
