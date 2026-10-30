# Cost / simulator prototype validation

Task: W04-HUNG-03. Hard dependencies W02-HUNG-02/W03-HUNG-02 and soft dependencies
W04-HAI-01/W04-PHONG-02 are Done in the live backlog. Implementation starts from
main893b9ff, including merged metadata/policy/jobs/cache/multipart producer ports.

## Verified

`make check` passed: Go formatting/vet/tests/build, Python tests and document-name
checks. Targeted `-race -count=1` passed for `internal/cost`, `internal/simulator`,
`cmd/costsim` and `internal/benchmark`. Input regressions cover missing/mismatched
units, unpriced/missing rates, exact progressive tier aggregation and DDB gross
normalization, rational precision growth, ambiguous leading-zero fractions,
trace bounds/order/Region/policy, cancellation, double-supplied storage and invalid
routes/snapshots. CLI results are private, carry input hashes and refuse overwrite.

The simulator proves shared-policy age boundary at30days−1ns versus30days,
missing authority/protection facts stay indeterminate, repeated replay is identical,
and policy selection alone does not mutate storage class/usage. Byte-time includes
the final interval. Revival counters add no extra price or unique identities;
ratios and route counters retain their distinct units.

The clean-source local integration run passed on
**b795d831bb47d280a39275c98011e8a865145d9f** using pinned DynamoDB Local/Moto:

| Proof | Result / scope |
|---|---|
| Cloud and Edge synthetic traces | Exact subtotal/provenance/omissions; no full total or MVP gate |
| Native DynamoDB adapter timing |30 samples;10 committed4-byte overwrite revisions, peer reads, outbox, idempotence and quota checked; not HTTP or AWS throughput |
| Native policy/multipart/producers | Shared contracts on real DynamoDB Local transactions; test-only R1 authority |
| Admin/config/multipart consumers | Token/cursor/revision primitives, CLI transport, config integration and CRC contracts |
| Existing paired Warm harness | C1/C4, real Miles/Badger and direct Moto;4 runs ×36 full-file SHA256-verified GETs |

[Published evidence, checksums and original run archive](../../benchmarks/evidence/cost-simulator-prototype/README.md).
Private complete run: `/home/ubuntu/.local/state/miles/cost-simulator-release-evidence`.
The smoke cleaned its Docker projects/tables/processes; no credentials/configs
are published. Required CI `benchmark-contracts` runs this combined path; `check`
checks the published evidence hashes. CI status belongs to the actual PR head.

## Remaining integration

W04-CHIEN-01 Admin APIs and W04-CHIEN-02 multipart APIs remain **Todo** in the
live backlog on2026-10-30. Those HTTP PRs cannot yet be merged/exercised here;
the third task checklist is only satisfied at the existing port level. Keep the
task under implementation/review until the full HTTP integration is accepted.

The production DynamoDB/R1/R2 runtime is not composed; W05-HUNG-01 supplies it.
The Warm harness now accepts explicit `dynamodb`/`dynamodb-local` declarations
for an actual endpoint but does not manufacture a production server, authority
provider, measured billing units or a final DynamoDB throughput result.

W06-HUNG-01 owns four-policy comparisons, additional traces, retention/minimum
charges, full route/infrastructure coverage, matched direct-S3 cost comparisons
and final report/API wiring. W4 uses shared policy definitions and a dated pricing
snapshot, emits partial supplied-usage costs and applies no provisional MVP target.
No AWS apply/publish, IAM mutation or live paid benchmark was performed.
