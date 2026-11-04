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

## Live fixture and staged acceptance

The live fixture in `tests/live` uses the pinned Go SDK signer against deployed
Cloud and Edge front doors. It creates fresh `acceptance-<random>` users and
logical buckets through the normal Admin and S3 APIs. Supply the system Admin
token through an existing operator-owned loopback tunnel. The fixture verifies
TLS with the reviewed CA, preserves an explicit Host port 443, disables proxies
and refuses redirects. AWS provider credentials stay with the deployment tools.

Prepare a private 0700 directory outside the checkout and a 0600 JSON config.
The token file must also be a regular 0600 file. Replace the endpoint and file
placeholders using the deployed inventory and pinned SSH tunnels.

```json
{
  "region": "ap-southeast-1",
  "ca_file": "/private/presigned/tls-ca.pem",
  "token_file": "/private/presigned/system-admin-token",
  "result_file": "/private/presigned/regular-result.json",
  "slow_state_directory": "/private/presigned/slow-state",
  "endpoints": [
    {
      "name": "cloud",
      "url": "https://cloud.installation.test:443",
      "connect": "REPLACE_ALB_DNS:443",
      "admin": "http://127.0.0.1:9001"
    },
    {
      "name": "edge",
      "url": "https://edge.installation.test:443",
      "connect": "REPLACE_EDGE_IP:443",
      "admin": "http://127.0.0.1:9003"
    }
  ]
}
```

Run the regular matrix after approval and deployment.

```sh
MILES_PRESIGNED_LIVE_CONFIG=/private/presigned/regular.json \
  go test -v -count=1 -timeout=30m ./tests/live -run '^TestPresignedLiveFrontDoors$'
```

Each front door covers seven-day presigned PUT/GET, special paths, underscore
metadata, HEAD, Range, checksum rejection without publication, copy, both delete
APIs, multipart completion/read-back and abort. Identical GET/PUT URLs are replayed
after credential revoke, grant revoke and user disable, waiting 5.1 seconds for
auth snapshots to expire. Write-only multipart, cross-user rejection, revoked
bearer grant mutation and empty HEAD error bodies are also checked. Per-request
timeout is 180 seconds. Each result records status, elapsed time, time to first
response byte, body length and SHA-256. Signed URLs and credentials stay out of
the evidence. Record the deployed release hashes and client module versions
alongside these results.

### Slow engine operations

Use three additional configs with the same endpoints and slow-state directory,
and a fresh `result_file` for each phase. Prepare synthetic multipart data with
backend latency disabled. The `prepare` phase uploads 70 distinct 5 MiB parts per
front door and persists the upload bindings, credentials, ETags and expected
full-object hash in private 0600 files. This phase requires its own approved
synthetic data and cleanup scope.

```sh
MILES_PRESIGNED_LIVE_CONFIG=/private/presigned/prepare.json \
MILES_PRESIGNED_LIVE_PHASE=prepare \
  go test -v -count=1 -timeout=60m ./tests/live -run '^TestPresignedLiveFrontDoors$'

# Apply the reviewed, bounded latency experiment to DynamoDB traffic on the
# disposable AWS nodes, retaining lease renewal and per-call transport timeouts.
MILES_PRESIGNED_LIVE_CONFIG=/private/presigned/complete.json \
MILES_PRESIGNED_LIVE_PHASE=complete \
  go test -v -count=1 -timeout=10m ./tests/live -run '^TestPresignedLiveFrontDoors$'

# Remove that experiment, then apply the reviewed latency experiment only to
# physical S3 traffic. Keep a timer that removes the traffic-control rules.
MILES_PRESIGNED_LIVE_CONFIG=/private/presigned/preflight.json \
MILES_PRESIGNED_LIVE_PHASE=preflight \
  go test -v -count=1 -timeout=10m ./tests/live -run '^TestPresignedLiveFrontDoors$'
```

Complete must return its real receipt after more than 60 seconds before the first
response byte. GET must spend more than 60 seconds before its first response,
then stream all 350 MiB with the expected SHA-256. Inspect the object layout and
backend observations to establish that preflight covered distinct physical packs.
Calibrate the experiment within the 120-second ALB idle timeout and 180-second
client timeout. Remove latency rules before recovery, drain or cleanup.

Private slow state supports the three explicit phases. A failed or incomplete
prepare requires inspection and a fresh isolated fixture. Retain those files for
the approved teardown, then remove credentials, tokens and private test keys.

## AWS acceptance results

The ARM64 release at `438ed46` ran on two Cloud nodes behind an ALB and one
AWS Edge node. All three used the same binary SHA-256
`7b763e4d3236094909093ba2f06c409c67ba18fee0c4c4bf740d14802849cefc`.
The [sanitized evidence](evidence/presigned-live/summary.json) records the release,
latency experiment, response timings, byte counts and body digests.

- The regular matrix passed 58 observations on each front door, 116 total.
- GET preflight passed with 70.739 seconds to the first byte through ALB and
  68.669 seconds through Edge. Both streamed exactly 367,001,600 bytes with
  SHA-256 `275d2b9684072f4e26588eae0ff54d49a1cc335861ac9a9300f9da2aec550bd0`.
  Latency was removed during the remaining body transfer before the client timeout.
- Cloud Complete returned its real receipt after 71.767 seconds. Edge Complete
  returned HTTP 409 after 67.011 seconds, and a second attempt under sustained
  latency also returned 409. Its upload reopened Active with all parts retained.
  Replaying the same input after removing latency returned 200 in 2.197 seconds.
- Temporary Edge builds with added diagnostic logging reproduced the commit-stage
  conflict. Their results are diagnostic observations. Acceptance timings above
  come from the recorded production release.
- A diagnostic Edge process exited with status 1 during shutdown, reporting
  `R1 admission or maintenance gate busy`. The combined cleanup error does not
  establish whether writer reconciliation or journal flush produced that error.

The diagnostic stack identified a stale transaction in DynamoDB manifest
preparation: page/chunk verification retained the earlier R1 observations until
the Verified-owner CAS. A legitimate heartbeat or change in gate membership
invalidated those observations during slow verification. Preparation now keeps
the frozen owner, then rechecks current writer authority and that exact owner
in a fresh bounded transaction before publishing Verified. It does not repeat
the manifest scan or relax writer fencing.

The correction at `49c2d8d` passed all five CI checks. Native DynamoDB Local
regressions explicitly pass renewal and peer membership changes while rejecting
a closed writer and a changed owner. This correction has not been deployed to
AWS; the live results above remain results for `438ed46`.

Review of the first correction found that the enclosing publication transaction
still retained its pre-scan lease. Commit now separates preparation from final
publication, discards pre-scan observations, and rechecks all publication conditions
in a fresh transaction. A native regression uses the production 60-second lease,
advances 75 seconds during enumeration with five successful heartbeats, and
requires the first commit to publish revision 1 and its outbox event. Related
cases reject revocation and expiry during enumeration without publication.

Effective presigned payload modes also use the signature parser's existing
validation before handler dispatch. Independently signed requests with a malformed,
empty or repeated `X-Amz-Content-Sha256` query value must return InvalidArgument.

A native shutdown regression reproduces R1 busy while a peer still holds the
recovery authority for a closed epoch. Shutdown flush now waits and retries
discovery within its original eight-minute budget. It seals after the peer
releases authority, while cancellation and fencing still fail the shutdown.
Operation and lease-cleanup errors are returned separately. A busy operation is
retried only after successful lease cleanup. A native regression injects a
release failure while the peer holds the epoch and requires both errors to
propagate, with the failed lease still Active and no second attempt started.
Writer reconciliation, journal flush and recovery-authority cleanup errors now
identify their stage. This reproduces one cause of the recorded error, while
the combined historical log cannot establish that it was the only cause in
the diagnostic AWS run.

Acceptance stays open pending sustained-latency Complete and shutdown confirmation
on the corrected AWS release. The live results above still describe `438ed46`.

## Verified cleanup

The [cleanup record](evidence/presigned-live/cleanup.json) confirms deletion of
all three instances and their 20 GiB root disks without backup, the workload
bucket and its 1,637 versions, all 20 metadata tables, ALB, network/endpoints,
Edge credential provider, test certificate and audited session document.
The workload Terraform state is empty. The synthetic table inventory contained
5,886 items before deletion.

Temporary IAM permissions were removed and the previous managed-policy versions
restored. All three run-created service-linked roles were deleted. ELB initially
refused deletion of its role while still reporting the deleted ALB as in use;
the eighth retry succeeded and native IAM confirmed role absence. Edge role
cleanup required the approved IAM administrator because the deploy grant lacked
`iam:ListInstanceProfilesForRole`; its exact native identity was verified before
deletion, and Terraform state was reconciled only after native absence proof.
Three operator tunnels and twelve temporary key/credential files were removed,
and both AWS CLI login sessions were cleared.

Bootstrap and remote state remain. The runtime KMS key is pending deletion on
2026-12-03. The secret is scheduled for deletion with its Terraform recovery
window configured to 30 days; the evidence preserves its returned `DeletedDate`
separately from that configuration. Twenty DynamoDB SYSTEM backups created at table
deletion expire on 2026-11-04; forty pre-existing backups remain untouched.
The evidence records completion before the approved cleanup deadline. It does
not claim verification of the final AWS billing statement or live acceptance
of the corrected release.
