# Cluster operations live validation

The acceptance run used Singapore AWS resources on 2026-11-03. Two Cloud
`t4g.small` instances in two AZs shared DynamoDB and S3 with one AWS Edge
`t4g.small`. One Cloud replacement brought the run's total to four instances
and four disposable encrypted 20-GiB root disks. Oracle ran operator CLI tools.
This was a functional run with 2 GiB per node.

The common three-node runtime release was `12e0fe9b40fa53a23faa1b321137f3c9891fe630`.
Its archive SHA256 is
`6cd14df3ef6e4d0781934329126ab01049ea91a052732eddd633fe683e827601`.
The binary SHA256 verified on those three processes is
`1ee6cee47796608674745c1bd92ab8a031f03f8e9e302c2bbae68f5eb4349c53`.
Earlier jobs and the in-flight replacement ran release `528b385`; the cache
ownership correction was then installed in place and the shared jobs and S3
round trips ran again on that common release.

| Behavior | Native result |
| --- | --- |
| Shared Admin jobs | Create, get, list, cursor handoff and cancellation worked through separate Cloud/Edge gateways with policy revision 1. Ordinary owner tokens received 403 for operational routes. |
| Cache fan-out | Three-node purge succeeded. A suspended Edge produced an explicit partial result. |
| Worker fencing | A suspended Edge-owned job transferred to a Cloud worker after R1 expiry. The resumed old worker preserved the successor's terminal outcome. |
| Cancellation | A running parent reached Cancelled and all dispatched children were terminal. |
| Periodic scheduling | Each of journal flush, purge uploads and recover uploads produced 54 deterministic period jobs. The native history scan found no overlapping period-job lifetimes for any type. |
| Expired upload purge | An upload and part were created through S3. A conditional test-only expiry change on that exact synthetic upload triggered periodic purge. Reserved quota fell from 102400 to zero and a second purge preserved quota. |
| CloudWatch | Real `Miles/DSG` readback contained Cloud and Edge samples. An isolated monitoring TLS failure preserved the same Edge process and fifteen readiness samples of 200. Export resumed after restoring routing. |
| Cloud drain | A held PUT completed with exact read-back after remote drain and native ASG termination. The drain job reported admission closed, flush complete and ALB unused. |
| ALB fail-open | Both live targets were unhealthy while controllers were briefly paused. A signed request received the application's S3 503 and request ID. Both health endpoints returned 503. |
| Edge drain | The accepted job reported admission closed and `flush_complete=false`. The runtime stayed alive and its health/S3 gates returned 503. |
| Crash restart | After the cache correction, actual SIGKILL restarted the service with a different process invocation and node incarnation, then readiness returned 200. |

The first concurrent S3/purge probe received one `409 OperationAborted`.
A complete serial retry and that release's complete signed S3 suite passed.
This run does not establish throughput or a retry rate under sustained load.

The final Edge drain closed the last operator tunnel while its POST was in
flight. The response was lost. A consistent native Jobs scan reconciled the
single accepted Edge job to Succeeded without submitting another drain.

The crash probe initially exposed a legacy cache ownership marker blocking
systemd restart. The correction holds a kernel flock on a persistent inode and
releases ownership on process death. For the existing legacy marker, the
operator stopped the unit, proved no gateway process remained and retained the
marker outside the disposable cache before upgrading. The subprocess regression
also checks live-owner exclusion, SIGKILL/reopen, legacy-marker rejection and a
lock-file symlink. Old releases still require that operator migration.

GC, compaction, tiering, scrub and recovery-checkpoint executors remain supplied
by their owning components. Acceptance closes the orchestration foundation and
its installed executors. Those future implementations consume this foundation
and retain ownership of their physical execution and integration tests.
Requests for uninstalled executors return
`501 job_type_unavailable` before enqueue. The run created no jobs of these
uninstalled types. Maintenance-compatible destructive authority and real
checkpoint/rebuild remain integration prerequisites. Frozen completion recovery
has native DynamoDB Local/S3-Moto contract coverage. This AWS run exercised its
periodic discovery and the normal multipart path.

The final Edge-only correction is `b23741eacbca407c1dcc0558a5742a778336f4ba`.
Its archive SHA256 is
`423323d78657d6be9e70cae1a317686b6e76a209f0608c76260b4f560a85bd57` and its binary SHA256 is
`226aa16bd81a664a8d171700ef18f45bdba5d25b7e5711fa49b6822a341b2237`.
After all Cloud nodes completed clean drain, the remaining Edge initially failed
its 30-second shutdown export deadline. The operation ended after 30.09 seconds,
and a conditional replay's PutObject 412 hid the failed readback. The correction
runs the existing renewal supervisor during a flush, bounds export to eight
minutes within the remaining process shutdown budget and preserves both errors.
The final Edge was upgraded in place, acknowledged a drain through a direct
loopback client with HTTP202 and completed a clean shutdown. Cloud had already
been removed, so the last correction was exercised live on Edge. Local race
and full CI cover the shared code.

Native cleanup verification at 08:30:56 UTC confirmed all four EC2 instances
terminated, all four root disks absent, all twenty tables gone, and no remaining
run VPC, endpoint, network interface, ASG, ALB, data bucket, test certificate,
Roles Anywhere provider or SSM session document. The remote managed state was
empty. The synthetic bucket inventory contained 222 versions, all deleted.

The Edge role cleanup needed a final IAM-only action because the deploy policy
omitted `ListInstanceProfilesForRole` on its certificate-only role. The operator
matched its ARN and native RoleId to the owned Terraform record, confirmed no
profiles or policies, deleted it through the IAM session and removed exactly
that already absent role from state under the normal backend lock.

The run retained bootstrap/state, twenty native SYSTEM table backups until
2026-11-04, a KMS key scheduled for 2026-12-03 and the secret's thirty-day deletion
window. Earlier backups retain their own expiry. Billing reconciliation remains
pending. The approved spending envelope was 5 USD before tax and its window
ended at 08:57:30 UTC.

IAM baseline defaults were restored and the temporary inline grant and policy
versions were removed. Native results and source digests are available in
[result.json](evidence/cluster-operations-live/result.json) and
[provenance.json](evidence/cluster-operations-live/provenance.json).

The run-created Roles Anywhere and ELB service-linked roles were deleted too.
ELB initially refused deletion while its usage inventory still referenced the
already deleted load balancer. A later IAM-only retry succeeded at 08:41 UTC.
Run private keys, client credentials and Admin tokens were removed locally.

Subsequent review corrections add transactional Admin actor checks, retirement
of superseded worker leases and upload sweep coordination that permits epoch
closure. Their HTTP/race and DynamoDB Local/S3-Moto proofs are recorded in the
[regression suite](cluster-operations-validation.md#review-regressions). The AWS
results in this document identify the historical releases they exercised.
