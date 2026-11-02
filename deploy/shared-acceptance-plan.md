# Cloud and Edge live acceptance plan

This is a deployment proposal. Local/emulator tests do not check these boxes.
Apply, spending and deletion require approval of the private saved plans and
resource inventory. Use the existing bootstrap/backend; inspect live state first.

## Topology and boundaries

- Two private AL2023 ARM64 ASG nodes in different Singapore AZs, one HTTPS ALB,
  one versioned S3 bucket and 20 DynamoDB tables. Use interface endpoints in both
  AZs plus S3/DDB gateway endpoints, with no NAT. Start with reviewed non-bursting
  compute or standard-credit instances sized for the declared engine budgets.
  Verify account/AMI/type eligibility through the native EC2 API before launch.
  A two-GiB ARM64 fallback can check bounded sequential functions; it does not
  establish that concurrent read budgets plus OS/runtime headroom fit the host.
  Fleet tables cap on-demand reads/writes at 128 units per table; the single-node
  dev ceiling stays 10. The lower ceiling throttles shared R1 fences even in a
  small functional run. These are request ceilings, not provisioned capacity or
  a billing hard cap.
- For subsequent runs, provision a separately budgeted AWS EC2 host for the Edge
  profile as well as the Cloud fleet. Keep deployment runtimes and their temporary
  services on AWS; the Oracle workstation holds only source, CLI and evidence.
  The functional rerun proposes one `t4g.small` Edge host with a disposable,
  encrypted 20-GiB root disk, no instance profile and a temporary operator SSH key.
  Limit SSH/HTTPS to the operator's public IPv4 /32; transfer private files over
  SSH only after pinning its host key through authenticated EC2 console output.
  Inspect the saved plan for the extra instance, key pair and security-group rules.
  Install chrony, Nginx and the verified Roles Anywhere helper on the Edge host.
  Keep S3/Admin/metrics on loopback behind an allowlisted HTTPS Nginx listener.
  Its credential_process remains certificate based on EC2; no IMDS or developer
  credential fallback is allowed. A workstation HTTPS probe now reaches a
  separate AWS public endpoint, avoiding the previous host's public-IP hairpin.
- Use the same release archive SHA and binary on all three nodes. Configure the
  same backend, history, global policy and shared secret. Give Edge a separate
  Roles Anywhere identity with 15-minute sessions; keep its private certificate
  key outside Terraform and the release.
- No domain is currently available: use test DNS names and a dedicated short-lived
  local CA. Import the Cloud leaf into ACM and explicitly trust the CA only in test
  clients. Resolve Cloud through the ALB DNS and Edge through its verified public
  address while retaining the signed Host/SNI, including explicit `:443`. Never
  disable TLS verification or install this test CA as a production root.
- Admin/metrics remain inaccessible from Internet probes. The owner uses a
  separate user token through SSH local forwarding. Pin host keys; review the
  additional SSM SSH document permission if that private path is used.

## Execution order

1. Inspect current state, identity, policies, active data, Edge services/network
   and clock. Generate private saved foundation/active/cleanup plans. Derive
   operator IAM with `iam-policy-autopilot generate-policies` from those JSON
   plans, compare with the existing boundary and review exact temporary grants.
   Root, if separately authorized, changes only IAM; deploy role runs workloads.
   On a new account, prepare the AWS-managed Roles Anywhere and ELB service-linked
   roles before first use. Scope custom ASG role tag/read/delete/PassRole and
   RunInstances on the owned launch template as well as its referenced resources;
   simulator success alone does not establish these native dependencies.
   If an old secret name is reserved by scheduled deletion, use a fresh
   `runtime_secret_name` inside the approved prefix. Do not restore, overwrite or
   force-delete the old secret to reuse its name; preserve its recovery window.
   For interactive CLI login, present the freshly generated authorization URL as
   a clickable link; the operator only copies the returned code into the private
   code file. Copying the URL is a fallback for an observed browser/link error.
   If the source Console role session expired, switch back and assume the deploy
   role again before authorizing CLI access; verify STS after login.
2. Apply the approved foundation with no serving nodes. Provision the secret
   outside state and initialize empty metadata/history through the documented
   offline command. Partial provisioning must be reconciled, never reset.
3. Upload/pin the clean release, provision ACM and the public Roles Anywhere CA,
   then apply the approved active plan. Confirm two healthy ALB targets in two
   AZs, real ready probes, Edge HTTPS and the same binary version/hash.
4. Run signed S3 requests through ALB and Edge: create bucket/cache policy,
   alternating PUT/GET, HEAD/Range/LIST, overwrite, copy/delete and multipart
   complete/read-back. Cover Unicode, percent, plus, encoded slash, double slash,
   underscored metadata and `Host:443`; compare exact bytes/checksums. Use bounded
   synthetic data only. Record node/target evidence without credentials or raw
   signed URLs. Presigned client compatibility remains a separate suite.
5. Keep Edge active through credential expiration/refresh (at least 16 minutes),
   verify a new temporary session succeeds without restart, then simulate helper
   failure and verify no fallback, then restore the helper. Hold a bounded signed
   request open through ALB and identify its Cloud node using the existing private
   active-request metric. Request that instance's termination through ASG without
   decrementing desired capacity. Prove admission closes while the request remains
   active, then finish it and verify its bytes. Require a clean process invocation
   and journal export before lifecycle completion. Move the pinned owner tunnel to
   the surviving node and prove the old instance is replaced by a different,
   healthy instance, restoring two targets in different AZs. Account for a
   temporary fourth EC2 instance and fourth root disk during replacement; verify
   native vCPU quota first. Do not force lifecycle completion or reset deadlines.

## Cost, cleanup and evidence

The rerun proposes up to two and a half hours including bootstrap, one real
hourly epoch cut and cleanup; approval of the prior run does not extend to this
new window. Keep a small synthetic dataset
(at most 256 MiB logical per test generation). The request will use a conservative
5 USD spending envelope, subject to live regional rates and the saved plan; this
is not a billing hard cap. Two-AZ interface endpoints, ALB, EC2/EBS, DynamoDB, KMS,
Secrets Manager, metrics/log requests and Internet transfer all have costs.
Budget the separate Edge EC2, its disk and any Internet/network path explicitly;
include them in the next saved plan and cost approval.

Record the approved resource IDs before creating data. After collecting evidence,
drain serving nodes, destroy only this test's active topology and synthetic data
with the reviewed cleanup plan, remove the temporary certificate/provider/grants
and remove the dedicated Edge host. The run creates four disposable 20-GiB root
volumes in total, including the replacement node's disk, with up to four present
at once. Deleting all four without a backup still requires explicit approval.
Existing datasets/state outside the run are preserved. Bootstrap/state and KMS or
secret deletion windows may retain costs; report exactly what remains.

The evidence must include saved-plan digests, same-binary digests, ALB target AZs,
native storage round trips, strict TLS/path/Host results, credential refresh,
owner-only tunnel failover, drain/export outcome and actual cleanup inventory.
Do not mark live acceptance complete until these results exist.
