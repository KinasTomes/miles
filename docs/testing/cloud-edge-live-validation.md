# Cloud and Edge live validation

## Result

The Singapore acceptance run on 2026-11-02 established a working shared deployment,
but did **not** complete live acceptance. A retained unknown commit blocked the
hourly recovery cut and subsequent writes. The local correction below reproduces
and fixes that failure. Keep the change in draft until the corrected runtime
passes the remaining AWS scenarios.

Two private ARM64 `t4g.small` Cloud nodes ran in separate AZs behind HTTPS ALB,
with shared S3, 20 DynamoDB tables and private AWS endpoints. Edge ran on the
existing Oracle host for this run. The approved two-GiB fallback establishes only
bounded sequential functionality, not production sizing or concurrent capacity.
Future runs must place Edge on a separately budgeted AWS host too.

## Release and checks

The corrected runtime release was built from commit
`be8981ca88f4de1027265fca7c5a3af688277f16`. All three running nodes reported binary
SHA256 `5b8d11b762ddd869216b39b6f918ea5fe5a2a22ed79d856efd9c9c80b561604b`.
The pinned archive SHA256 was
`65519b15358a222dce35b724084ea944c17115f5d11be7e30aa2c1843e496e7a`.
The later infrastructure-only change `2a0133f` raises the fleet table request
ceiling; it does not change that runtime binary.

| Check | Native result and limit |
| --- | --- |
| Two-AZ fleet | Two healthy ALB targets, private addresses, SSM online, no NAT. |
| Shared data path | Signed PUT/GET/HEAD/Range with exact byte comparisons across Cloud and Edge passed. Full copy/delete/multipart sequence did not finish. |
| TLS and SigV4 path | Explicit signed `Host:443`, Unicode, literal percent/plus, encoded slash, double slash, dot segments and underscored metadata passed through the tested paths. A dedicated test CA was trusted only by test clients. |
| LIST continuation | Cloud-to-Edge continuation passed after using the shared runtime key. The original process-random key failed across nodes. |
| Edge credentials | Actual Roles Anywhere sessions refreshed twice in the same process. A write succeeded after the first session expired. Making the helper key unreadable caused startup failure with no ready listener or credential fallback. |
| Owner access | A separate owner token read its own usage through pinned SSH forwarding to loopback Admin. Moving the tunnel between healthy nodes after controlled process stops passed. Physical instance replacement was not tested. |
| Edge Internet ingress | Strict HTTPS through the local proxy passed. An independent Internet client reaching Edge port 443 was not established. SSH access alone does not prove this route. |
| Final shutdown | Edge and one Cloud node emitted invocation-specific `shutdown complete`. The other Cloud node exited with a pending-reconciliation error and no clean-shutdown proof. A slow in-flight request drain was not tested. |

## Failures and corrections

Three deployment fixes were committed separately:

- Inject the shared cursor key into S3 LIST handlers instead of generating a
  process-local signing key. Tests cover independent handlers, restart, key
  copying and rejection of a foreign key.
- Probe the owner's `/usage` route through the tunnel. The user-detail route
  requires system-admin privileges and rejected the deliberately restricted
  owner token.
- Raise fleet on-demand read/write ceilings from 10 to 128 units per table.
  Native DynamoDB returned `ThrottlingException` during shared R1 operations at
  the old ceiling. The new value is a request ceiling, not provisioned capacity
  or a hard billing cap. It was applied, but the full functional sequence could
  not be rerun after the recovery gate closed.

The remaining blocker is separate from those fixes. Strong reads showed a
writer lease closed with a pending commit, and the corresponding original
operation retained in `Dispatching` with a pending request and no retained
result. The hourly gate was in `RecoveryCut`; new writes were fenced. Cloud
shutdown reported `reconcile original operation before release or takeover`.

No result row is not proof that a commit did not happen. The run did not clear
the pending marker, reset history, forge a closure proof or force the gate open.
The metadata backend had a fenced close-attempt primitive, but this observed
operation was not reconciled by the composed runtime. The local correction below
does not retrospectively turn the interrupted AWS sequence into a passing run.

Checkpoint/rebuild and retention/GC consume closed, sealed epochs. They must not
treat the blocked cut as a valid seal or infer success from an absent result.
This finding blocks the live deployment acceptance; it does not replace those
consumers' independent tests.

## Evidence and follow-up

Private evidence retains saved-plan hashes, native inventories, credential
refresh timestamps, TLS/client assertions, authoritative pending-operation
reads and shutdown logs. Credentials, private keys and signed requests must not
be published with this record.

Prepare a new AWS-only Cloud/Edge plan with an explicit Edge host cost, then
repeat full CRUD/copy/delete/multipart, external Edge HTTPS and an in-flight
drain. Existing emulator/CI success does not close those native checks.

## Reconciliation correction: local proof

`ReconcileR1` now CAS-closes the original commit attempt and settles the producer
receipt atomically. It releases standalone reserved quota only after that proof,
keeps multipart-owned staging intact and retains the closed lease's physical
roots/pins. Epoch closure revisits the producer receipt rather than skipping it.
The process registry retries release only after its local work has joined.

Native DynamoDB tests reproduce both an unsent final transaction and a committed
transaction with a lost response. They cover a second Store instance, rejection
of a delayed original transaction, lost closure acknowledgements and idempotent
quota accounting, expired/crashed authority past the durable one-hour cut,
Closed/Sealed progress, retained roots and rejection of unknown physical I/O.
Registry tests verify that live work or unavailable reconciliation cannot report
clean shutdown, and that a retained committed result remains readable after release.

`make check` and race suites for DynamoDB, app composition, coordination, object,
managed writes, recovery and readpath passed with scoped DynamoDB Local/Moto
fixtures. Existing multipart settlement and short-lease hourly scheduling
regressions also passed. Each fixture's containers/data were removed on exit.
These results require a new AWS run for deployment acceptance.

## Cleanup

The reviewed teardown completed. Independent AWS reads confirmed both EC2s
terminated, both disposable root volumes absent, and no run-owned ALB, ASG,
interface/gateway endpoints, network interfaces, VPC, data bucket or metadata
tables. The stack state contains zero resources. The imported ACM certificate,
temporary SSM document, runtime roles/profiles, Roles Anywhere provider and all
three service-linked roles created for the run were also removed.

The synthetic inventory contained 147 object versions (62,028,242 physical bytes,
including two release archives), no multipart uploads and 599 metadata items.
All versions were deleted with per-object error checks before bucket destruction.
Authoritative failure evidence was retained before deleting the tables.

The application stopped at about 12:18 UTC, EC2/ALB teardown completed by 12:56,
and the native API confirmed interface endpoints absent at 13:02. Final state and
permission reconciliation finished later. Cleanup crossed the 13:01 target while
handling source-role expiry and delete/waiter IAM permissions; no new serving
resources were launched to extend the run. This is not evidence of a strict
two-hour cleanup deadline or an exact billed total.

The following remain deliberately outside the empty workload state:

- Twenty DynamoDB `SYSTEM` backups. The native inventory reports expiry on
  2026-11-03 at approximately 13:14 UTC; do not substitute the service maximum
  retention period for these observed expiry timestamps.
- The run's KMS key in `PendingDeletion`, due 2026-12-02 at 13:16 UTC.
- The run's secret scheduled for 2026-12-02 at 13:14 UTC. CloudTrail confirms
  `recoveryWindowInDays: 30` and that deletion date. Its `DescribeSecret`
  `DeletedDate` field showed the request time, so that field alone was not used
  as proof of the recovery window.
- Existing bootstrap/state and pre-existing deletion windows. They were excluded
  from the run's destructive plans.

Temporary IAM grants were removed, original managed-policy defaults restored,
temporary policy versions deleted and the root CLI login cache cleared. A final
read-only CloudTrail audit grant was also removed immediately after verification.
Oracle's temporary gateway/proxy/time services, service user, firewall rule,
signing helper, ten private key/token/secret files and disposable build/provider
caches were removed; the original Apache and time synchronization were restored.
Source, release archives and private audit evidence remain.

The USD 5 envelope was an authorization limit, not a billing hard cap. Billing has
not been reconciled; retained backups, deletion windows and bootstrap storage must
not be described as zero-cost merely because the workload state is empty.
