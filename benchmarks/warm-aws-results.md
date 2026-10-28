# Preliminary single-node Badger/Warm results

W03-HUNG-02: the real AWS run completed on 2026-10-28 UTC (2026-10-29 UTC+7).
All six direct-S3/Miles pairs passed: concurrency 1 and 4, three independent
trials per setting, 12 runs, 7596 operation samples, zero failures. Every run
verified all 90 files in four GET passes: **4320 full-file SHA-256 checks**.
Logical LIST/HEAD/DELETE checks passed as well.

## Reproduction and environment

The measured clean source is `590b1c6c604446b13d843a49fc211a43d95ab44d`;
the build uses Go 1.27.2 and the SDK versions/hashes in each raw result.
Dataset `backup30`, seed `miles-w02-v1`, contains 30 versions in each of three
groups, 90 files and 754994670 logical bytes. The identical manifest,
concurrency, SHA-256 PUT header, single-PUT protocol, SSE-KMS `alias/aws/s3`,
Region and read-repeats=3 were used for each pair. No SDK retries or multipart.

One dedicated `m7i-flex.large`, 2 vCPU/8 GiB, in `ap-southeast-1`; Intel Xeon
Platinum 8488C, Linux 6.1.189, encrypted 20 GiB gp3 at default 3000 IOPS/125 MiB/s.
AWS CLI 2.38.0. The planned `m6i.large` was rejected by the account's Free Plan,
with no instance/volume created by that attempt. The same-size eligible Flex
replacement has a lower public hourly price; no account-plan upgrade occurred.
Flex has a [40% baseline and opportunistic burst](https://docs.aws.amazon.com/ec2/latest/instancetypes/instance-types.html).
No CPU utilization/throttling
time series was collected; these numbers cannot establish fixed CPU capacity.

The client shares the EC2 with Miles. Direct uses HTTPS to S3; Miles uses
loopback HTTP plus its own HTTPS backend. Direct always runs first; order is
not randomized. S3's service cache is uncontrolled. Each Miles trial starts a
new process, Badger/domain and 64 MiB LRU with no admit-on-write; the physical
bucket is verified empty. Direct uses a fresh, verified-empty random prefix;
tools/evidence coexist outside that prefix. The fixed development policy uses
FastCDC 256 KiB/1 MiB/4 MiB, zstd 3 / minimum saving 10%, read-ahead 4 and a
1 GiB logical quota. This is not the final Cloud/Edge topology or policy.

## Measurements

Each cell is **pooled p95 milliseconds / logical MiB per second**. Each phase
contains 270 samples per target/concurrency, from three trials. P95 uses
nearest-rank over the original raw samples; throughput is summed successful
logical bytes divided by summed phase wall time. GET timing includes hashing.
Do not average the three p95 values or interpret logical bytes as backend I/O.

| Concurrency | Target | PUT | Initial verify GET | Third repeated GET |
| --- | --- | ---: | ---: | ---: |
| 1 | Direct S3 | 129.27 / 74.16 | 138.11 / 76.09 | 121.48 / 84.01 |
| 1 | Miles | 378.99 / 33.17 | 273.56 / 55.16 | 232.15 / 60.58 |
| 4 | Direct S3 | 175.98 / 241.09 | 93.05 / 362.39 | 92.04 / 361.72 |
| 4 | Miles | 1007.84 / 50.12 | 251.35 / 223.44 | 235.32 / 233.22 |

Miles was slower than Direct S3 in these measured phases. The full phase
N/failures/latency/throughput/bytes, including HEAD/LIST/DELETE and all repeated
GETs, are in the [summary](evidence/warm-aws/summary.json). No performance
threshold is declared achieved and no causal attribution to cache/CPU is made.

## Retention, cleanup and cost window

After logical DELETE and graceful Miles shutdown, each trial retained 61 packs
and 61 sidecar indexes: 122 S3 objects / 260116123 bytes. Badger allocated about
3 MiB per installation; exact allocated and apparent file sizes are recorded
separately. Producer payload/outbox remained in Badger without exporter, epoch
seal or checkpoint; their bytes were not separately counted. These end snapshots
are not high-water marks, physical-GC proof, billed bytes or achieved savings.

Only after joining Miles did orchestration remove that whole disposable
synthetic installation. Before final teardown, an independent LIST found the
pack bucket empty and only tools/evidence in the direct bucket. All original
raw result hashes were checked after export outside EBS. The approved teardown
then deleted all 20 Terraform resources, including test EBS without backup.
Independent AWS inventory confirmed the instance terminated, its volume gone,
both buckets absent, and test network/runtime IAM removed. The two temporary
inline grants and temporary boundary were removed; the original ten managed
deploy policies, trust and boundary were unchanged. Root CLI was logged out.
Bootstrap/state were outside the resource plan. Service-owned AWS-managed KMS
keys remain. See the [redacted cleanup proof](evidence/warm-aws/cleanup.json).

EC2 launched at 17:41:22 UTC and cleanup finished at 18:12:13.559 UTC: at most
0.5144 billed-window hours. The EC2/EBS/IPv4 public-price estimate for that
window is **USD 0.06549 before tax**, excluding S3/KMS requests/storage and
small evidence transfer. This is not a final bill. The full conservative
[planning case](warm-aws-run-plan.md) remains USD 0.645345 against the approved
USD 1 allowance; actual request/billing totals were not queried. No test EC2,
EBS, bucket or temporary IAM grant remains active after cleanup.

## Evidence and downstream use

[Original raw results and SHA-256 inventory](evidence/warm-aws/README.md) ·
[dataset manifest](evidence/warm-aws/dataset-manifest.json) ·
[exact executed orchestration](evidence/warm-aws/orchestration.py).

These measurements belong to source `590b1c6`. Subsequent integration of #43
keeps benchmark mode memory-only with admit-on-write explicitly disabled;
normal local mode uses the merged memory/disk node and write policy. The
historical local smoke artifacts also retain their original source/profile.
Neither this AWS run nor those old artifacts measure the newer disk-cache
profile. Current local smoke declares the new profile and checks its protocol.

W04-HUNG-03 may consume these raw logical samples and declared environment,
but must keep measured values, operator declarations and modelled costs
separate. Final DynamoDB, distributed UTC/R1/R2, exporter/GC, Cloud/Edge and
cost/target acceptance remain integration/benchmark work in their own tasks.
