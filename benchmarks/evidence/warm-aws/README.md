# Preliminary real AWS Warm evidence

Twelve original result JSONs are in `runs.tar.gz`, named
`c{1,4}-t{1,2,3}/{direct,miles}-result.json`. `artifacts.json` contains their
original SHA-256 values; `sha256sums.txt` checks this public bundle. Nothing in
the raw results was rewritten. Account, instance, bucket and credential data
are excluded; private infrastructure/inventory evidence remains outside Git.

Each run uses clean source `590b1c6c604446b13d843a49fc211a43d95ab44d`, backup30,
90 files / 754994670 bytes, and checks every file in four full GET passes.
`summary.json` pools raw latency samples across three trials per concurrency
and target; throughput uses summed logical bytes / summed phase wall time.
The raw results preserve phase-level values, all samples and actual client
versions. `dataset-manifest.json` contains the exact generator/seed/file hashes.

`orchestration.py` is the exact executed orchestration artifact, not a new
supported provisioning command. On an already authorized, disposable host,
it verifies the instance identity and three binaries, generates the dataset,
runs the pairs, joins Miles, inventories retained data, exports evidence and
removes only the synthetic physical installation between trials. It never
provisions infrastructure. The matching private config was replaced with
placeholders in `reproduction-config.example.json`; fill these for a fresh
installation with its bounded instance role. The binaries were cross-built
from the clean source above with Go 1.27.2, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`
and `go build -trimpath`; exact hashes are in the summary.

Extract and independently inspect raw samples with standard tools:

```bash
sha256sum -c sha256sums.txt
mkdir extracted
tar -xzf runs.tar.gz -C extracted
python3 - <<'PY'
import hashlib, json
from pathlib import Path
for item in json.loads(Path('artifacts.json').read_text()):
    path = Path('extracted') / item['path']
    assert hashlib.sha256(path.read_bytes()).hexdigest() == item['sha256']
    result = json.loads(path.read_text())
    assert result['status'] == 'passed'
    expected = {f['path']: f['sha256'] for f in result['dataset']['files']}
    gets = [s for s in result['samples'] if s['operation'] == 'GetObject']
    assert len(gets) == 360
    assert all(s['sha256'] == expected[s['file']] for s in gets)
    assert all(not s.get('error') for s in result['samples'])
PY
```

This is preliminary single-node Badger/Warm evidence. The Flex host, fixed
pair ordering, different client transport paths, retained async work and
measurement limits are recorded in `summary.json`; no final R1/R2, Cloud/Edge,
performance target or cost saving is inferred.
