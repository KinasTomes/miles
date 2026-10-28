# Warm benchmark harness

Task: W03-HUNG-02. `cmd/warmbench` runs the same single-PUT workload against an
explicit direct S3 or Miles endpoint. It reuses dataset v1 and the installed Go
S3 SDK. It does not provision infrastructure, start a gateway, or supply an
event producer. All local executions use the small `smoke` profile.

**Task acceptance is still pending:** on the implementation baseline, PR #39
supplies object handlers/service, but `cmd/miles/s3.go` only wires bucket handlers.
LIST/DELETE P0 is W03-CHIEN-03; authoritative producers and runtime adapters are
also pending. A successful emulator run checks the harness, not Miles Warm.
Do not mark the full task Done from this PR or substitute fixture events in runtime.

## Reproduce a run

1. Build with `make build`. Generate the existing smoke dataset into a new directory:

   ```bash
   go run ./cmd/datasetgen --profile smoke --seed miles-w02-v1 --out /private/test/dataset
   ```

2. Copy [the config example](warm-harness.example.json) to a private file, fill the
   actual endpoint, Region, bucket and environment, and set mode 0600. Keep its
   parent private. The bucket must already exist; credentials must have PUT,
   GET/HEAD, LIST and DELETE access. Use a dedicated test bucket/domain and
   short-lived credentials; do not create persistent AWS access keys for this tool.

   Credentials use the local runtime format, with optional STS session token:

   ```json
   {"access_key_id":"<temporary-or-DSG-key>","secret_access_key":"<secret>","session_token":"<STS-token-if-needed>"}
   ```

   Both config and credential files must be regular non-symlink files with mode
   0600. Endpoint must be an origin: HTTPS, or literal loopback HTTP for local
   development. No default AWS credential chain, proxies, redirects or TLS bypass.
   `direct-s3` requires HTTPS; label emulator runs `local-s3-smoke`.
   `miles-warm` requires declared metadata `badger`, gateway commit and policy
   revision. Environment fields are operator declarations; confirm them against
   the installation and retain the private config. Optional `kms_key_id` requests
   SSE-KMS; it does not configure the Miles physical backend.

3. Run both targets with the **same executable, manifest, concurrency and repeats**:

   ```bash
   ./bin/warmbench --config /private/test/direct.json --manifest /private/test/dataset/manifest.json --out /private/test/direct-run --concurrency 1 --read-repeats 3
   ./bin/warmbench --config /private/test/miles.json --manifest /private/test/dataset/manifest.json --out /private/test/miles-run --concurrency 1 --read-repeats 3
   ```

   Each output directory must be new. For concurrent workload, run a separate
   matched pair with e.g. `--concurrency 4`; do not call it single-stream throughput.
   Generate `backup30` or run performance workloads only on the dedicated test host.
   Real AWS execution requires separate authorization for its resources and charges.

## Workload and safety

Before network I/O, validate the bounded manifest, relative paths, regular files,
sizes and every full-file SHA-256. Source files must remain unchanged during a run;
PUT also sends the manifest SHA-256 as an explicit checksum header. Dataset reads
use `os.Root`, so traversal and symlinks cannot escape the dataset directory.

Each run allocates a random `warmbench/<128-bit-id>/` prefix and checks it is empty.
The manifest order feeds a bounded worker pool; network completion order may differ.
Keep each file's group/version key so versions are not overwritten. Phases are:

1. PUT all files, then HEAD sizes. PUT is single-request, no multipart, SDK maximum
   attempts 1, fixed `application/octet-stream`, Standard, SHA256 header.
2. GET every file, stream hash and compare exact length/SHA-256. No gzip decoding.
3. LIST with pagination, checking exact keys/sizes and rejecting duplicate/missing
   keys or looping/empty continuation tokens.
4. Additional GET passes, each separately timed and fully verified (default 3).
5. DELETE only the manifest keys in this run, then LIST to verify logical absence.

Every request has the existing 30-minute hard context budget. Interrupt/SIGTERM
cancels calls and waits for workers before writing final evidence. A failed phase
stops subsequent phases, writes failed samples and retains possible objects for
diagnosis. Never blindly delete the prefix later: inspect the private run record.
No bucket deletion, version purge or physical pack deletion is performed.

Fresh prefixes prevent accidental overwrite; **they do not reset domain dedup or
cache**. For comparable trials, use a separately verified fresh node/domain/backend
or record the reset method, state and retained overhead per [baseline](baseline.md).
Repeated GET passes are not automatically labelled cold/warm and do not prove a
cache hit ratio. The harness does not purge retention roots or drain async work.

## Artifact and metric definitions

`result.json` (0600 in a new 0700 directory) records schema v1, status, UTC start/end,
prefix, full manifest plus its hash, Region, declared environment, actual Go/SDK/OS/
architecture/CPU count, executable SHA-256 and VCS revision/modified state when
available. Config URLs, buckets, KMS IDs, credential paths/tokens and raw SDK error
messages are excluded. Environment declarations can still contain private text;
review/redact any artifact before publication. A hard crash can leave `running`;
it is never a completed trial.

Raw per-file samples include phase/group/operation, latency ns, logical payload
bytes, output SHA-256 and a bounded error code. LIST has one sample per page. Sample
arrays remain in manifest dispatch order. Phases record wall time, N, failed count,
successful logical bytes, bytes/second and nearest-rank p50/p95/max over **all**
samples, including failures. Failed bytes are excluded from throughput, not silently
removed from evidence. GET records received bytes including partial failed reads;
PUT records declared payload size, not actual transferred bytes after an error.
HEAD/LIST/DELETE have zero payload bytes in these throughput summaries.

Latency includes SDK/signing, body I/O and GET hashing; upload opens its local file
inside the sample. File verification and provisioning are outside phase throughput.
Read passes and operation phases never share a throughput or percentile denominator.
Use raw group samples for entropy breakdown; small-N percentiles remain small-N.

Logical payload counters are **not** wire bytes, read-ahead, retries, S3 physical
storage, backend/control requests, DDB billed units or cost. Async journal/pack
flush and retained versions/tombstones/packs need external observations and an
accounting window before drawing those conclusions. W3 data is preliminary
Badger/Warm only, not the final R1/R2 or MVP performance gate.

The downstream W04-HUNG-03 simulator may reuse these schema v1 inputs and samples;
it must keep measured fields separate from declarations and modelled/billed values.
Do not combine unmatched manifests, toolchains, checksum settings, Regions,
encryption, node configurations, isolation or concurrency into a claimed baseline.

See [validation](warm-harness-validation.md) for checks actually executed.
