# Cloud and Edge live acceptance plan

This is a deployment proposal. Local/emulator tests do not check these boxes.
Apply, spending and deletion require approval of the private saved plans and
resource inventory. Use the existing bootstrap/backend; inspect live state first.

## Topology and boundaries

- Two private AL2023 ARM64 ASG nodes in different Singapore AZs, one HTTPS ALB,
  one versioned S3 bucket and 20 DynamoDB tables. Use interface endpoints in both
  AZs plus S3/DDB gateway endpoints, with no NAT. Start with reviewed non-bursting
  compute or standard-credit instances sized for the declared engine budgets.
- One Edge process on the operator's current ARM64 Ubuntu host. Install chrony,
  Nginx and the verified Roles Anywhere helper; inspect existing services first.
  Keep S3/Admin/metrics on loopback behind an allowlisted HTTPS Nginx listener.
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
   failure and verify no fallback. Prove owner tunnel failover to the other healthy
   node, then restore the helper. Drain while a request runs; require a clean
   process invocation and verified journal export before lifecycle completion.

## Cost, cleanup and evidence

Allow up to two hours of active AWS resources and a small synthetic dataset
(at most 256 MiB logical per test generation). The request will use a conservative
5 USD spending envelope, subject to live regional rates and the saved plan; this
is not a billing hard cap. Two-AZ interface endpoints, ALB, EC2/EBS, DynamoDB, KMS,
Secrets Manager, metrics/log requests and Internet transfer all have costs.
The current Edge host is an existing paid machine; do not present it as free
infrastructure or silently alter unrelated services.

Record the approved resource IDs before creating data. After collecting evidence,
drain serving nodes, destroy only this test's active topology and synthetic data
with the reviewed cleanup plan, remove the temporary certificate/provider/grants
and restore Edge configuration. Deleting the two disposable root/cache volumes
does not require a backup of synthetic data, but still requires explicit approval.
Existing datasets/state outside the run are preserved. Bootstrap/state and KMS or
secret deletion windows may retain costs; report exactly what remains.

The evidence must include saved-plan digests, same-binary digests, ALB target AZs,
native storage round trips, strict TLS/path/Host results, credential refresh,
owner-only tunnel failover, drain/export outcome and actual cleanup inventory.
Do not mark live acceptance complete until these results exist.
