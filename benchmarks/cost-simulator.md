# Cost / simulator prototype

W04-HUNG-03 supplies an offline accounting replay, shared metric definitions,
native DynamoDB adapter timing and integration proofs. It reuses `internal/clock`,
`config`, `policy` and `pricing`; there is no second tiering engine or AWS deployment.

## Run

```bash
make build
# The parent exists; output files must be new and are created with permission0600.
./bin/costsim -trace benchmarks/traces/cloud-boundary.json -out /tmp/cloud-cost-model.json
./bin/costsim -trace benchmarks/traces/edge-revival.json -out /tmp/edge-cost-model.json
# Includes both traces, native DynamoDB contracts and the original paired Warm smoke.
python3 -B scripts/cost_simulator_smoke.py --out /home/ubuntu/.local/state/miles/costsim-run
```

Use a new output path on every run. `-pricing` selects an explicit catalog;
default is the shared Singapore example dated2026-10-19. The synthetic January
timeline is a counterfactual using that dated catalog, not historical AWS billing.
Exit0 means a valid prototype artifact,1 means input/model/output failure,2 means
CLI usage error. Partial costs with unpriced items are valid, labelled subtotals.

## Trace v1

See the two [synthetic fixtures](traces/cloud-boundary.json) and
[Edge revival fixture](traces/edge-revival.json). JSON is bounded to8MiB and4096
events, rejects unknown fields/trailing documents, and preserves input order for
events at equal timestamps. Offsets must be nonnegative, sorted and within the
declared duration. Cancellation/overflow/invalid evidence fails without a report.

The envelope declares name/kind (`synthetic` or `observed-accounting`), Region,
profile, metadata backend (`badger`, `dynamodb`, `dynamodb-local`, `none`), topology,
start UTC, duration nanoseconds, bytes per billable GB and storage month seconds.
These are declarations, not runtime observations. A complete global `policy` and
exact `policy_revision` pass the same snapshot validator as the runtime. Missing
fields/revisions do not select defaults/current policy. Nonempty `omissions` must
describe coverage that is absent; W4 never emits a complete total.

Each event may provide:

| Field | Units / interpretation |
|---|---|
| `snapshot` | Live logical head bytes; unique raw/stored bytes count each live domain/hash once; physical bytes include retained/dead/control storage |
| `write_counters` | Delta over this event/window: chunk and duplicate **occurrences**, revived **distinct identities per request**, actual revived stored bytes; not a partition |
| `traffic_bytes` | Observed or explicitly modelled deltas per directed route; no cache-hit extrapolation |
| `usage` | Rate ID, exact catalog unit and nonnegative decimal/rational quantity string; max16 lines/event |
| `billable_storage_bytes` | Full inventory by class after the event, held piecewise constant; omitted/null preserves it, `{}` clears it |
| `tiering` | Exact pack/state revision, unique live/total stored bytes and explicit assumed facts (`0` unknown,`1` valid,`2` invalid) |

Routes are client→gateway, gateway→client, gateway→S3 upload, S3→gateway download,
metadata/control plane, inter-node, client→S3 and S3→client, named in `cost.Route`.
Control/inter-node totals are raw traffic observations; their billable route/rate
mapping must be supplied separately in `usage`. Cloud Internet client output
remains logical gateway→client traffic even when backend downloads are zero.
Edge hits may reduce S3→gateway downloads; nothing in the model invents a hit ratio.

Storage integrates each inventory interval as
`bytes × elapsed_ns / (bytes_per_gb × storage_month_ns)` using exact rationals.
It includes the tail through the declared end, not just the final inventory.
Old archive/dead/retired storage stays charged until an explicit inventory change.
Inventory must already contain billable minimum sizes, archival metadata and
temporary restored copies, or list those missing effects in omissions. The W4
converter does not implement early deletion/minimum duration rules. Supplying the
same class through inventory and normalized usage is rejected to avoid double charge.

`EvaluateTiering` receives the virtual Clock's domain plus explicitly modelled,
validated zero-uncertainty UTC samples. Missing safety facts stay indeterminate.
An eligible decision does **not** issue a transition or mutate inventory. A later
explicit observation must account for actual class/storage/request changes.
This is policy decision parity, not a production UTC/R1/R2 proof or lifecycle job.

## Pricing and definitions

`internal/cost` owns shared supplied-observation definitions and exact pricing.
The parser and `storage_rules` remain owned by `internal/pricing`. Input rate
units must match the catalog, quantities aggregate per rate across the trace,
and then split across progressive tiers. Quantities allow64 digits per component;
rational accumulators cap numerator/denominator at4096 bits, rejecting adversarial
coprime-denominator growth. Aggregation starts at zero for this
trace; it is not account-wide AWS tier aggregation. DDB gross normalization is
preserved; already billed read/write units are never multiplied by a transaction
factor again. No prices are refreshed or added by this task.

Reports retain catalog Region/as_of/basis/sources, priced lines, exact rational
USD strings, missing/unpriced usage, omissions and `complete_total_usd=null`.
There is no net saving/break-even claim without a matched, complete direct-S3
baseline. Revival counters add no separate fee: supplied PUT/storage/network
usage already accounts for those bytes. Zero-denominator ratios are null;
dedup=logical/unique_raw, compression=unique_raw/unique_stored and total reduction=
logical/physical. The ratios need not multiply. Filtered bucket physical allocation,
retention charges and four-policy comparisons remain W06-HUNG-01.

## Integration and measured evidence

The smoke script creates a private run directory with source revision/dirty state,
tool hash, input/catalog/policy hashes, per-route counters and explicit backend.
Its local Unix Docker projects and per-test tables are isolated and cleaned.
The required CI job `benchmark-contracts` runs this path without AWS credentials.

- Simulator results are **modelled** supplied observations; the fixture's backend
  label does not mean a database was queried.
- `dynamodb-metadata.json` is a **measured native adapter timing probe**,30 samples
  for preparation/commit/peer manifest reads of ten4-byte overwrites. It verifies
  head revisions, outbox visibility, idempotent replay and unchanged quota. Table
  setup/cleanup and checks outside those samples are excluded. Native DynamoDB
  Local with a test-only lease is not AWS latency, billed capacity or HTTP throughput.
- Policy persistence, multipart/completion and producer contracts run on native
  DynamoDB. Admin revision/token/cursor primitives, operational CLI transports,
  config consumers and multipart CRC contracts run together; this is integration
  of ports, not missing production HTTP handlers.
- The original Warm runner still verifies whole-file SHA256 on C1/C4 for real
  Miles/Badger plus direct Moto. Result schema v1 adds
  `mvp_threshold_applied=false`; old artifacts keep their original schema/data.
  The harness also accepts declared DynamoDB backends for an actually composed
  endpoint; it does not start or provision one.

Full Warm HTTP on DynamoDB needs W05-HUNG-01 runtime composition and real R1/R2;
full Admin/multipart HTTP waits on W04-CHIEN-01/02 (still Todo in the live backlog
on2026-10-30), rather than being claimed by these port tests. W06-HUNG-01 adds
the four policy comparison models, more traces and complete matched cost coverage.
No live AWS apply/publish, final performance target, user billing or resource
permission changes are part of this task.
