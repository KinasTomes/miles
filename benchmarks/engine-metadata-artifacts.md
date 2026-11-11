# Engine/metadata artifact procedure

This procedure prepares evidence for the [technical report](../docs/engine-metadata-report.md).
It separates local native smoke, historical pilot evidence and future live pilot
measurements. No throughput, cost saving or real archive restore result is implied
by successful checksum verification.

## Evidence scopes

| Source | Scope | Permitted use |
| --- | --- | --- |
| `engine-metadata-pilot` bundle | Small native local engine/metadata smoke, DynamoDB Local + S3/Moto, object-service PUT/GET and authenticated Admin handlers | Identity, accounting, artifact pipeline and verified GET correctness |
| `warm-aws` bundle | Historical single-node Badger/Warm run, source `590b1c6`, backup30 | Reuse its original dataset checksums and historical conclusions with original scope |
| Fresh live Cloud/Edge pilot | Requires its own retained inputs, source/policy/workload, permissions and run evidence | Future performance/cost conclusions only after actual execution |

The name of the local bundle describes its handoff purpose; `run.json.kind`
explicitly says `local-native-engine-metadata-smoke`. It is not an AWS pilot.
The smoke dataset has nine small payloads. Do not generate backup30 or run a large
performance workload on the operator machine; use the dedicated approved host.

## Reproduce a measured local bundle

1. Use a clean committed checkout and the pinned Go 1.27.2 toolchain. Start only
   the isolated DynamoDB Local/Moto services from `deploy/local/compose.yaml`.
   Set `MILES_DDB_LOCAL_ENDPOINT` and `MILES_RECOVERY_S3_ENDPOINT` to their explicit
   `http://127.0.0.1:<port>` endpoints. The helper accepts only loopback origins.
2. With both fixture `_REQUIRED` flags enabled, choose a NEW absolute output path:

   ```sh
   MILES_DDB_LOCAL_REQUIRED=1 MILES_RECOVERY_S3_REQUIRED=1 \
   MILES_DATASET_ARTIFACTS_OUT=/tmp/new-engine-metadata-capture \
   go test -v -count=1 -timeout=6m ./internal/app -run '^TestNativeDatasetArtifacts$'
   ```

   The collector explicitly creates a smoke dataset in a temporary private
   directory. It refuses an existing output directory. Without the export env
   variable, normal tests skip export. Output contains no payloads, issued
   credential responses, runtime state or AWS/private endpoints.
3. Inspect source cleanliness and scope in `run.json`. The collector records
   actual source commit, dirty-source flag, Go version, test executable SHA-256,
   input-manifest SHA-256, phase windows and limitations. A development capture
   may be dirty; the publisher refuses it. Commit the collector first and rerun
   from that clean source before publishing evidence. Do not rewrite provenance
   fields to make an earlier dirty capture look clean.
4. Publish to a NEW directory; the destination parent must already exist:

   ```sh
   python3 -B scripts/dataset_artifacts.py publish /tmp/new-engine-metadata-capture \
     --out /tmp/new-engine-metadata-bundle
   python3 -B scripts/dataset_artifacts.py verify /tmp/new-engine-metadata-bundle
   ```

   Publication checks measured semantics before copying an explicit allowlist,
   adds a scope README and generates SHA-256 over exact published bytes. The
   verifier rejects mixed/stale snapshots, incorrect ratios, bad GET coverage,
   quota/identity/allocation mismatches, unexpected files and symlinks.
5. Commit only the small verified public bundle. Keep payloads, credentials and
   private runtime output outside Git. Remove only the owned disposable Compose
   project after all tests finish. Retention in a live installation still applies;
   this procedure grants no right to delete real Cold/recovery roots.

## Dataset and snapshot interpretation

The three sequential cohorts have distinct owners and bucket incarnations.
Owner A and B share a domain; C uses another domain. Each uploads all nine files
under distinct keys, preserving all three dataset versions as live objects. GETs
are verified from the Edge object service against full-file SHA-256. This bypasses
the SigV4 front door and is not a client HTTP throughput benchmark.

The collector exports empty/A/A+B/A+B+C phases. The incremental same-domain
upload should leave global unique identities unchanged; C adds another set of
domain-scoped identities. Do not compare the raw phase ratios as independently
reset performance trials. The same-domain phase includes two logical copies,
and the last phase includes three logical copies across two domains.

Each phase explicitly refreshes the bounded production stats collector only
after workload completion, then reads raw Admin response bytes for global stats,
all bucket stats and owner usage. Final-phase layouts sample version zero of
each entropy family/owner. Global/bucket snapshot IDs and collection times must
match; layout AsOf/revision/domain/canonical placement must bind to that report.
The existing layout API has no snapshot ID; the bundle does not invent one.

Stats remain approximate multi-record observations. No periodic job scheduler is
started for this controlled run; durable control/history bytes and setup objects
are still in complete listed physical totals. Temporary restored billing,
infrastructure, interface/wire bytes and AWS costs are not measured.

## Byte integrity across operating systems

Evidence JSON/text paths are pinned to LF in `.gitattributes`; binary archives
remain binary. Do not reformat or normalize an artifact before verification.
Windows CRLF checkout bytes can differ from the original Linux manifest bytes
even though parsed JSON is equal. A mismatch is reported, never accepted as a
successful checksum merely because the parsed values match.

For an untouched historical text file, compare its recorded digest with the
original Git blob and independently check the archive's member digests. Restore
canonical original bytes only after confirming the worktree has no user edits;
do not regenerate historical measurements or update their hashes to fit a new run.

```sh
python3 -B scripts/dataset_artifacts.py audit-historical benchmarks/evidence/warm-aws
```

The audit hashes every public file, reads archive members without extracting or
executing them, checks all original result digests and full GET hashes, and
reports their original release/scope. It does not assert that the old input
payload files still exist or that current code has reproduced old performance.

## Missing evidence

An incomplete or failed collection is not a completed bundle. No fabricated
zero, template response or simulator result may replace unavailable pilot stats.
Keep failed/excluded run records with their reason. For a new live dataset,
record its source manifest, verified GETs, policy and snapshot window anew; reuse
the existing pilot report checks for complete inventory and matched topology.
