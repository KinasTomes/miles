# Presigned compatibility validation

## Scope and behavior

Presigned GET/PUT share the normal SigV4 credential lookup, user status and
bucket authorization path. Expiry is 1..604800 seconds inclusive; a request at
the exact expiry instant is valid, one after it is denied. A seven-day URL does
not extend the five-second authorization snapshot lifetime. Writes retain the
existing authoritative publication checks; this change adds no authorization
cache or credential bypass.

Signed query headers are now materialized after verification of the original
wire request. Checksum, metadata, copy-source and unsupported-feature checks
then see the same values as header-signed requests. Differing query/header
values, case aliases or bytes invalid in HTTP header names/values fail closed.
A query copy-source selects CopyObject
and needs read on the source as well as write on the destination.

Before the correction, an SDK-signed query containing a SHA-256 checksum
accepted a same-length modified body with HTTP 200. The regression now requires
HTTP 400 BadDigest. The fixture explicitly puts the checksum in the query;
it does not claim the pinned Go SDK hoists checksum headers by default.

## Local checks

Use the repository's pinned Go 1.27.2 and dependency versions in go.mod/go.sum:
AWS SDK core v1.47.5, S3 v1.114.3 and smithy-go v1.28.7. Requests are generated
by the SDK signer/presigner independently of the gateway verifier. Existing
captured CLI/boto3 transport fixtures retain their own client versions; no new
CLI/boto3 compatibility result is claimed.

- `internal/api/s3/presigned_test.go`: existing authorization matrix and upload
  binding cases using query signatures, exact expiry/cap/future-skew boundaries,
  method/path/Host/scope tampering, HTTP HEAD error with no body, query checksums,
  metadata, unsupported ACL/checksum, query/header conflicts and copy-source
  read authorization. The matrix uses a fake metadata store and handler probes;
  it covers write-only and cross-user multipart authorization without claiming
  full multipart storage execution.
- `internal/api/admin/presigned_acceptance_test.go`: real Admin mutations on
  Badger, replaying the identical GET/PUT URL before and after credential revoke,
  grant revoke and user disable at the 4999/5000-ms snapshot boundary. A revoked
  bearer cannot elevate a write-only grant.
- `cmd/miles/presigned_test.go`: both Cloud and Edge profiles in explicit local
  mode through the real composition root, Badger, pack SDK, object writer,
  multipart service and reader. Moto emulates physical S3 only. Presigned PUT/GET,
  HEAD metadata/length, Range, copy, multipart complete/read-back, bad-checksum
  non-publication and delete/LIST assert actual bytes and state. Timeout is 80 s
  per HTTP call, 90 s per profile and 3 min for the selected test invocation.
  This does not run shared production DynamoDB/Clock/runtime adapters.
- `deploy/edge/proxy_test.go`: digest-pinned Nginx 1.30.6, real TLS with a
  throwaway trusted CA, native Router and echo handlers. Signed/presigned Host
  `edge.example:443`, Unicode, percent/plus, encoded slash, double slash and dot
  segments must arrive unchanged. Copy/delete and every multipart route preserve
  method, body hash and URI. The opt-in slow cases hold both GET before its first
  response (preflight-shaped delay) and Complete for 65 s, each with an 80-s
  client timeout; elapsed time must be at least 65 s. These are transport tests,
  not slow real engine/preflight/Complete or AWS ALB proof. TLS verification and
  redirect refusal stay enabled. The suite timeout is 5 min for both slow cases.

From the repository root, after activating the Go toolchain:

```sh
make check
go test -race -count=1 -timeout=3m ./internal/api/s3 ./internal/api/admin ./internal/auth ./internal/multipart ./internal/object

# Use a unique project name per concurrent run. No shared volumes or AWS data.
docker compose -f deploy/local/compose.yaml -p miles-presign-acceptance up -d s3
docker compose -f deploy/local/compose.yaml -p miles-presign-acceptance port s3 5000
# Substitute the returned loopback address; do not use real AWS endpoints.
MILES_RECOVERY_S3_ENDPOINT=http://127.0.0.1:PORT MILES_PRESIGNED_LOCAL_REQUIRED=1 \
  go test -v -race -count=1 -timeout=3m ./cmd/miles -run '^TestPresignedLocalProfiles$'
MILES_EDGE_PROXY_TEST=1 MILES_EDGE_PROXY_SLOW_TEST=1 \
  go test -v -race -count=1 -timeout=5m ./deploy/edge
docker compose -f deploy/local/compose.yaml -p miles-presign-acceptance down --volumes --remove-orphans
```

The profile test skips when no fixture endpoint is supplied; setting
`MILES_PRESIGNED_LOCAL_REQUIRED=1` turns a missing endpoint into a failure.
CI runs it against the job's disposable Moto endpoint. The deployment smoke
runner enables both slow TLS cases and records both 65-s thresholds.

## Remaining live acceptance

Local validation is separate from the previous AWS release and from the task's
ALB/Edge acceptance. The user selected code/local tests first. No AWS credentials
or live endpoints were supplied and no cloud resources were provisioned.

Before live acceptance, identify the deployed commit/binary on both front doors,
approved test bucket/users/grants, strict TLS trust, client versions, request
timeouts and cleanup scope. Replay the S3/multipart/copy/delete sequence through
both ALB and Edge, including presigned GET/PUT with explicit Host :443 and special
paths, credential/grant/token revocation, write-only/cross-user multipart and
HTTP errors/HEAD. Hold actual preflight and Complete for more than 60 s and prove
read-back, not just an open connection. Record results against that release,
without credentials, bearers or signed URLs. Costs and destructive cleanup need
a specific approved scope. Do not mark the live checklist complete from local
fixture results or from the earlier deployment's evidence.
