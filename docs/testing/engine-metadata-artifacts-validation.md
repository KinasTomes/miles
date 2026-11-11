# Engine/metadata evidence and accounting review

## Scope and provenance

The [technical explanation](../engine-metadata-report.md) references real code,
contracts and fixtures. The [local bundle](../../benchmarks/evidence/engine-metadata-pilot/README.md)
was captured from committed, clean source `94889a0` with Linux Go 1.27.2 and the
race detector, pinned DynamoDB Local 3.3.0 and Moto 5.1.15. Its
[run record](../../benchmarks/evidence/engine-metadata-pilot/run.json) retains the
actual executable checksum, source identity, collection windows and limitations.

The collector uses foreground object services on composed Cloud/Edge nodes and
authenticated Admin handlers. It does not measure the SigV4 front door, separate
host latency, throughput, infrastructure cost, real AWS behavior or restore time.
Periodic job scheduling is not started. Snapshots are retained listed observations
at collection time, not the installation's final post-cleanup storage or bill.

## Measured local domain experiment

Nine generated smoke files total 589,914 payload bytes. All 27 PUTs were followed
by full GET SHA-256 verification from the Edge object service. Distinct keys keep
all dataset versions live. A and B have different owners/buckets in one domain;
C uses another domain. This is a sequential incremental demonstration, not three
independently reset performance trials.

| Observed phase | Logical bytes D | Unique raw Uraw | Unique stored Ustored | Retained listed bytes P |
| --- | ---: | ---: | ---: | ---: |
| Empty | 0 | 0 | 0 | 3,278 |
| A | 589,914 | 458,782 | 262,453 | 268,895 |
| A+B, same domain | 1,179,828 | 458,782 | 262,453 | 268,895 |
| A+B+C, C isolated | 1,769,742 | 917,564 | 524,906 | 534,652 |

The first nine objects use seven identities because the three compressed-identical
files contain exactly the same gzip bytes. B doubles live logical bytes without
adding global identities. C adds seven identities in its own domain: 14 total.
Owner logical usage is 589,914 bytes each after the final phase regardless of
reuse; reservations, staging and active upload counters are zero in those captured
usage responses. Same-domain reuse does not discount user quota.

The unchanged P between A and B is this controlled run's observation; it is not a
promise that duplicate uploads never add physical control/journal/history bytes.
The final physical value includes sidecars and retained setup/control bytes.
Metadata storage in DynamoDB and infrastructure are separate cost inputs.

All final layouts retain their observed domain, revision, pack/generation and
metadata-only integrity status. The independent verified-GET file provides actual
payload hashes; layout 200 alone is not a physical integrity proof.

## Definitions reviewed against implementation

| Name | Numerator / denominator | Scope and traps |
| --- | --- | --- |
| Dedup ratio | D / Uraw | Live heads versus distinct live `(domain, hash)`; not cumulative attempted writes |
| Compression ratio | Uraw / Ustored | Unique raw versus selected canonical payload; not whole pack/object/bucket size |
| Total reduction | D / P | Whole-system retained LISTED physical, including overhead and dead/protected data |
| Codec minimum saving | `1 - stored_len/raw_len` | Per-chunk selection threshold; not the API compression ratio |
| Saving fraction derived from compression ratio R | `1 - 1/R` | A derived payload saving, not an independently measured cost saving |

The measured aggregate compression ratio is approximately 1.748054 in every
nonempty phase. The A+B dedup ratio is approximately 2.571653; after adding C it
is approximately 1.928740. That decrease is expected when the denominator gains
another domain's identities while the cohort composition changes. It is not a
compression regression or an independent same-domain-versus-isolated benchmark.

Default codec selection requires stored/raw <= 0.9 for a saving threshold of
0.1 (equivalently raw/stored >= 10/9 for that selected chunk). An aggregate ratio
does not mean every chunk was compressed. Already-gzipped/high-entropy input can
remain raw; gzip payload size is taken from the manifest, including its overhead.
Smoke is too small to establish FastCDC small-edit backup efficiency.

Zero denominators remain JSON null. Empty global total reduction is 0 here
because setup physical bytes are nonzero; empty bucket total reduction can be
null when allocated physical is zero. Unknown/incomplete physical is not a zero.

Bucket unique totals overlap. Their allocated physical payload is shared among
distinct referencing bucket incarnations; residual/structural/control/history
bytes stay unallocated according to the returned methodology. The additive
invariant is allocated bucket physical plus global unallocated equals global P.
Neither bucket unique totals nor bucket reduction ratios may be summed to build
a global value.

In the A+B snapshot, a bucket's unique stored total exceeds its allocated physical
share. `cost.Snapshot` is a whole-system input and validates unique_stored <=
physical, so feeding that bucket's overlapping unique totals alongside its split
allocation into this model is invalid. The review preserves the global guard;
it does not relax the model to make mismatched units pass.

`TestMeasuredDatasetRatiosAgreeWithCostDefinitions` compares actual global
snapshots against `internal/cost`. `TestAllocatedBucketPhysicalIsNotAGlobalCostSnapshot`
guards the attribution mismatch. No pricing rate is refreshed, no four-policy
simulation is presented as run, and no new cost-saving number is produced.
Future cost input must pin pricing hash/units/window and separately account for
requests, DynamoDB, compute/network, storage class minimum duration and temporary
restore billing. Missing observations must stay unknown or block that projection.

## Historical dataset audit

[The audit record](../../benchmarks/evidence/engine-metadata-history-audit.json)
checks the existing historical Badger/Warm bundle without rewriting any original
measurement. Its input manifest has 90 files and 754,994,670 bytes. The verifier
checked all public bundle hashes, all 12 original archived result hashes and
4,320 recorded full GET SHA-256 results. The original manifest digest is
`04014dd767e799d9f70081536d01c988fa6a02c0b7653c34db11a48c9abc1556`.

These are original source `590b1c6` results, not a new Cloud/Edge pilot or proof
that old payload files still exist. A Windows checkout had CRLF text bytes with
a different digest; the untouched Git blobs matched the historical digests.
Only original Git bytes were restored locally. New attributes pin evidence text
to LF while binary archives and original hashes remain unchanged.

## Validation and handoff

- Full Linux `go test -p 4 -count=1 -timeout=12m ./...` passed with DynamoDB
  Local and S3 fixture endpoints required; `go vet ./...` and `go build ./cmd/...`
  passed. These are local results, not a CI or live AWS acceptance claim.
- Targeted race tests passed for stats, cost, engine and benchmarkdata. The full
  Python suite passed 127 tests, including 12 artifact verifier tests.
- Native collector with `-race`: 27 GETs and 14 identities exported from clean source.
- Bundle verification covers all snapshots/identities/layouts/GETs and exact bytes.
- Negative Python tests rehash mutated captures before testing semantic rejection:
  mixed snapshot, incomplete physical, wrong ratio, non-null zero denominator,
  wrong GET hash, dirty source, sensitive fields, boolean byte counts and unexpected files.
- Separate byte/path tests reject CRLF substitution and parent traversal.
- Artifact procedure and all report links are checked; payloads, credentials,
  runtime directories and private endpoints are excluded from the public bundle.

Reproduce with [the artifact procedure](../../benchmarks/engine-metadata-artifacts.md).
The small bundle and historical checksum audit are ready as report inputs. A
fresh live performance/cost pilot still needs its own execution and evidence;
none of its missing measurements are filled from this smoke or from a simulator.
