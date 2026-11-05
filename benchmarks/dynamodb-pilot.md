# DynamoDB Cloud/Edge pilot

This pilot measures the complete shared runtime, with R1 admission/leases,
transactional R2 outbox, export/seal, checkpoint/GC jobs, cache and CloudWatch
running. It supplies preliminary cost/performance inputs, not a final throughput
threshold or a guarantee about production sizing. Use the same release on all
nodes. Edge runs on a dedicated AWS VM; the operator machine only builds and runs
CLI tools. EC2 Edge tests the profile, not an on-premises network or its costs.

## Runtime observations

Prometheus `/metrics` and the existing60s CloudWatch exporter share the same
cached snapshot. Scraping never contacts DynamoDB/S3/CloudWatch. CloudWatch uses
`Miles/DSG` with **both** `profile=cloud|edge` and
`installation=<exact ddb_table_prefix>`; job counters additionally retain their
existing type/state dimensions. Update metric consumers from profile-only series.

| Observation | Meaning and limit |
| --- | --- |
| `miles_ddb_calls_total`, `miles_ddb_transactions_total`, `miles_ddb_call_seconds_total` | SDK calls, transactional calls and summed elapsed seconds, including failures. Call seconds are not a percentile or phase wall time. Runtime clients retain disabled hidden retries. |
| `miles_ddb_consumed_units_total`, `miles_ddb_capacity_responses_total` | Sum of `ReturnConsumedCapacity=TOTAL` values actually present in SDK outputs and count of outputs reporting them. Failed/unknown calls can consume capacity without a response. This is not billed/exact total consumption. |
| `miles_ddb_read_units_total`, `miles_ddb_write_units_total` | Only explicit read/write breakdown fields reported by AWS. Missing fields contribute nothing; do not interpret zero as no charge or derive a read/write price from aggregate units. Cross-check native table metrics. |
| `miles_ddb_lease_write_attempts_total` | Mutation actions targeting the physical Leases table in dispatched transactions; excludes condition checks. Attempts include failed/repeated transactions, not acknowledged lease changes. |
| `miles_ddb_retries_total`, `miles_ddb_throttles_total` | Explicit adapter read/CAS/unprocessed-batch retries, and SDK throttling responses including transaction cancellation reasons. No object/user/table IDs are labels. |
| `miles_recovery_cut_age_seconds` | Age of the durable latest **Sealed** epoch's CutAt. Before the first close, age from the initial durable hourly schedule. A first Closed-but-unsealed epoch makes telemetry unavailable; its advanced schedule cannot reset cut age. Successful export alone never resets it. |
| `miles_journal_export_lag_seconds` | Age of the oldest currently unexported event found by bounded base scan for this history;0 when a complete observation finds none. Approximate live discovery, not exact closure. |
| `miles_r1_gate_closed_seconds` | Current RecoveryCut duration from its durable StartedAt; for other closed gates, time since this process first observed closure. Open gate is0. Sampling can miss short gates; record controlled GC/cut timelines for complete duration distributions. |
| `miles_stuck_completing_uploads` | Completing uploads whose LeaseUntil has expired at observation. A signal to inspect/reconcile, never permission to abort or reclaim roots. |
| `miles_runtime_observation_age_seconds`, `miles_runtime_observation_failures_total` | Background collector freshness/failures. Each15s refresh has a10s budget and32×128-record page cap per scanned table. Failed/partial discovery retains old data only until60s; stale gauges disappear rather than publish healthy zeros. |
| `miles_warm_read_{requests,aborted,integrity_failures,cache_hits,cache_misses}_total` | A single process-owned Warm reader counter set, reused by request service instances. Cache observations never reduce logical client delivery accounting. |

New metadata transaction plans request capacity before their exact idempotent
input is persisted. Telemetry leaves a frozen request's capacity setting intact,
including legacy requests without it, to preserve AWS idempotency parameters.

Export lag uses additive `event_observed_at` on the DynamoDB Outbox row, written
atomically beside a newly published immutable event. It is an observational wall
timestamp, excluded from EventID/payload digest and recovery authority. Export
progress retains it; idempotent retries preserve it. Legacy pending events without
this attribute make the observation unavailable until exported; no fabricated
creation time or automatic backfill. Existing immutable event formats and exact
closure proofs remain unchanged.

Statistics now reuse the existing bounded collector in shared runtime, available
through `dsgctl stats`. It reads metadata and **all listed physical versions**
including retained/dead/control bytes. Collection is approximate and background;
verify `physical_complete`, collection window and isolation before using ratios.
These background reads and inventory requests are part of measured overhead.

## Matched workload

Build `warmbench` once. Build datasetgen with
`go build -trimpath -o bin/datasetgen ./cmd/datasetgen`.
Generate the pinned dataset on a dedicated AWS test client:

```bash
./bin/datasetgen --profile backup30 --seed miles-w02-v1 --out /private/pilot/dataset
```

The existing backup30 workload is90 files, about720MiB, including three entropy
families and30 versions. Smoke proves the harness only; its tiny files do not
establish a useful throughput baseline.

Prepare three0600 configs using [Warm harness](warm-harness.md): direct S3,
Cloud ALB, Edge HTTPS. For gateway configs add `environment.profile` as `cloud`
or `edge`, declare `metadata=dynamodb`, exact gateway release and policy revision.
Keep the same client host/CPU/disk/network, region, encryption, manifest, executable,
concurrency and repeat count. Use properly trusted TLS; no insecure bypass.

Use separate fresh logical buckets/domains for Cloud and Edge and preserve the
reset/isolation record. Fresh prefixes alone do not reset dedup/cache. Compare
concurrency1 separately from4, with three matched trials per concurrency if the
approved time budget permits. Retained previous runs/control/history bytes must be
listed explicitly when interpreting aggregate statistics.

For each target (replace the config/output names):

```bash
./bin/warmbench --config /private/pilot/cloud.json --manifest /private/pilot/dataset/manifest.json --out /private/pilot/cloud-run --concurrency 1 --read-repeats 3 --retain-objects
```

`--retain-objects` stops after verified reads/LIST so the background statistics
collector can observe live heads. The artifact explicitly reports
`retained_owned_prefix_for_observation`. Run all three targets with the same flag.
The report requires this exact retained cleanup state for **each** run; a mixed
policy or three logically deleted prefixes cannot validate post-workload physical
statistics, even if the harness itself returned a successful workload status.
Wait for a successful statistics refresh whose `collection_started_at` is after
the workload finished, then export statistics via a pinned Admin SSH tunnel.
Retain-before-delete is opt-in; the default harness still logically deletes only
its owned keys. Always include retained synthetic keys/versions in approved cleanup.

## HTTP body routes

The harness now records `http_body_bytes` for actual consumed request/response
bodies, including error/partial bodies and protocol responses. Direct records
`client_to_s3`/`s3_to_client`; gateway runs record
`client_to_gateway`/`gateway_to_client`.

Runtime counters independently measure client↔gateway, gateway↔S3 and metadata
request/response bodies. S3 bodies include pack, sidecar, journal, inventory and
background work. Collect **every node directly** through private tunnels; repeated
scrapes through an ALB do not guarantee both Cloud nodes were observed.

These are HTTP-body observations, excluding headers/TLS/TCP/retransmission.
They do not equal interface wire bytes, S3 object size or chargeable transfer.
Preserve EC2 NetworkIn/NetworkOut, ALB processed bytes, endpoint/NAT metrics and
routing inventory separately. Cloud logical Internet GET bytes remain full logical
delivery in the simulator even on cache hits. Do not classify the AWS Edge route
as customer Internet egress or free local infrastructure.

## Report inputs

Capture each node's complete `/metrics` before and after each gateway workload,
with process start time unchanged. The observation envelope includes background
workers and a post-workload statistics refresh. Supply one private JSON file:

```json
{
  "cloud": {
    "before_at": "2026-11-04T00:00:00Z",
    "after_at": "2026-11-04T00:15:00Z",
    "nodes": [
      {"alias":"cloud-a","profile":"cloud","before":"<raw metrics text>","after":"<raw metrics text>"},
      {"alias":"cloud-b","profile":"cloud","before":"<raw metrics text>","after":"<raw metrics text>"},
      {"alias":"edge-a","profile":"edge","before":"<raw metrics text>","after":"<raw metrics text>"}
    ],
    "statistics": {"physical_complete":true,"collection_started_at":"2026-11-04T00:14:00Z","as_of":"2026-11-04T00:14:30Z","bytes_logical":1,"bytes_unique_raw":1,"bytes_unique_stored":1,"bytes_stored":1}
  },
  "edge": {"before_at":"<UTC>","after_at":"<UTC>","nodes":[],"statistics":{}}
}
```

The JSON shows shape only; values must come from actual observations, including
an equivalent complete Edge window. Do not use the example as evidence.

```bash
python3 -B scripts/dynamodb_pilot.py --cloud /private/pilot/cloud-run/result.json --edge /private/pilot/edge-run/result.json --direct /private/pilot/direct-run/result.json --observations /private/pilot/observations.json --out /private/pilot/summary.json
```

Output must be new and is0600. Input files are bounded to32MiB. The script rejects
mismatched workload/client declarations, backend/profile/release mismatches,
failed/unhashed GETs, missing routes, restart/reset counters, absent nodes, stale
observations, partial statistics and integrity failures. It copies only selected
fields; inspect artifacts before publication. Local emulator reports remain
`local_contract_only`; labels are declarations, not independent AWS attestation.

The report computes phase-specific throughput ratio and retained
logical/unique-raw/unique-stored/physical ratios. Do not merge initial/repeated GET
phases or hide small samples. Telemetry deltas span the declared wider observation
window; they are not attributed to one foreground request or a billing ledger.

## Alarms and native AWS evidence

Enable paid alarms explicitly with Terraform `enable_alarms=true` and the profiles
actually running in `monitored_profiles`. Five alarms/profile watch cut age>3600s,
any integrity error, any observed DDB throttle, expired Completing leases and stale
telemetry. Each evaluates60s periods,1-of-3. Gauges use Maximum and missing=breaching;
sparse error deltas use Sum and missing=notBreaching. Default alarm actions are
empty (state only); supply existing SNS topic ARNs for notifications. No alarm
performs automatic data cleanup or instance replacement. Pause removes the alarms.

Before the first successful refresh, freshness and state gauges are absent;
missing-data handling makes startup/collector failure visible. Shared gauges are observations,
never cluster sums. Capture both ALARM and recovery to OK with isolated synthetic
inputs, exact dimensions and release hashes. `SetAlarmState` alone is not proof of
threshold evaluation. Do not inject corrupt data or throttle a shared environment.

For every metadata table retain timestamped CLI readback of `describe-table`
(TableSizeBytes and ItemCount are approximate), `describe-continuous-backups`
(PITR enabled/window/earliest/latest recoverable times) and native CloudWatch
ConsumedReadCapacityUnits/ConsumedWriteCapacityUnits plus read/write throttles and
transaction conflicts. PITR is a status/storage-cost observation, not an invented
application counter. Use exact AWS dimensions and operation where required.
Cancelled/unknown transactions may incur consumption not returned to the client.

Retain network inventory, endpoint count/AZs/active hours, NAT count/hours and
processing bytes (explicit0 if absent), ALB/LCU/IPv4, EC2/EBS, S3 requests and all
versions, KMS/secret, DDB/PITR/backups and custom metric/alarm counts. Obtain current
regional rates at run time, keep price source/date, report measured quantities
separately from estimates and billing lag. Include whole-hour charging/retained
resources, avoid double counting gateway endpoint traffic as NAT.

[AWS DynamoDB metrics](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/metrics-dimensions.html),
[transaction consumption](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/transaction-apis.html),
[CloudWatch dimension identity](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/cloudwatch_concepts.html).

Real AWS creation/IAM changes, costs and synthetic data/EBS teardown require a
reviewed run-specific plan and approval. Use the existing deployment/cleanup
workflow; no runtime services or persistent test datasets on the operator machine.
