# Native engine/metadata smoke evidence

Measured with a committed native collector, DynamoDB Local and S3/Moto.
This is a small controlled domain-identity demonstration, not a performance
benchmark or live AWS pilot. Raw Admin response JSON is retained; payloads,
credential responses and runtime directories are excluded.

Run metadata pins the source, Go version, test executable and dataset manifest.
The four phases retain distinct object keys: empty, first owner, second owner in
the same domain, third owner in another domain. They are sequential observations,
not independent throughput trials. Physical totals include setup/control/history;
layout is metadata-only and full GET hashes are separate. No ratio implies cost
saving, and absent restore/infrastructure billing is not a measured zero.

Verify from the repository root:
`python3 -B scripts/dataset_artifacts.py verify <bundle-directory>`
Replace `<bundle-directory>` with this bundle's actual directory.

Reproduce using `benchmarks/engine-metadata-artifacts.md`. Actual timestamps,
request IDs, bucket incarnations and pack generations vary between runs.
