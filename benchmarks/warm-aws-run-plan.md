# Real AWS acceptance run and cleanup plan

W03-HUNG-02 / PR #40. Approved within USD 1 including synthetic-data cleanup,
and executed on 2026-10-28 UTC. [Results and cleanup](warm-aws-results.md)
record the measured source, instance substitution and limits. The procedure
below describes the bounded run; it does not authorize another execution.

## Resources and duration

Region `ap-southeast-1`, one dedicated Linux `m7i-flex.large` (2 vCPU/8 GiB), one
encrypted 20 GiB gp3 root volume, one public IPv4 for outbound SSM/S3, no inbound
application/SSH rules. Use SSM to operate; both Miles listeners remain loopback.
Use a tagged test instance role/profile limited to the test buckets and required
SSM/S3/KMS actions; deploy uses the approved non-root operator identity. Inspect
actual IAM permissions and the resource plan before provisioning; an IAM change
outside that test scope needs a separate report. No NAT, ALB, DynamoDB, ASG or
rebuild of the previously destroyed full dev stack.

The original plan used `m6i.large`; the account's Free Plan rejected it before
creating any instance/volume. The eligible same-size Flex replacement costs
USD 0.1197/h versus USD 0.12/h. No account upgrade was made. Its 40% CPU baseline
and opportunistic burst limit interpretation of the preliminary numbers.

Create two dedicated S3 Standard buckets, direct and physical packs, SSE-KMS with
the same AWS-managed `alias/aws/s3` key. It has no key-storage monthly fee; KMS
requests remain billable. Both bucket/public-access/TLS boundaries are checked
before writing. Only synthetic data, no existing bucket, EBS or user data.

Allow at most two hours, including setup and cleanup. Set instance-initiated
shutdown to terminate, root DeleteOnTermination, and an in-guest shutdown timer
as a fallback; independently verify the instance/volume/IP gone after cleanup.
The timer is not a global AWS billing cap; a failure still requires inventory and
cleanup of any residual resources. Collect evidence outside the disposable root.

## Workload and acceptance

1. On this test host, generate `backup30` with the existing seed/toolchain. Record
   actual manifest/file hashes, sizes and every installed client/build version.
   Do not generate this larger performance dataset on the development workstation.
2. Build one clean commit for Miles/warmbench. Run matched direct-S3/Miles pairs
   at concurrency 1 and 4, read-repeats 3, three independent trials per setting.
   Keep files/keys/checksum header/Region/encryption/schedule identical. Use fresh
   Badger/cache/domain metadata, an empty physical bucket and a fresh empty
   direct benchmark prefix per trial (tools/evidence use other prefixes);
   previous trial data is inventoried, retained until all work stops, then removed
   before reuse. Never reset by bypassing active retention/maintenance gates.
3. Export all raw samples, phase N/failures/p95/throughput/logical bytes; every GET
   checks full-file SHA-256. Record actual EC2/disk/network, fixed dev policy,
   cache state and the absent exporter/seal workers. Keep unsuccessful trials and
   reasons. Results are preliminary Badger/Warm, not final distributed R1/R2,
   Cloud/Edge or savings acceptance. No final performance threshold is inferred.
4. Stop/join Miles and requests; capture Badger/outbox and S3 start/end inventory,
   retained payload/pack/sidecar bytes and async debt. Logical DELETE is not
   physical deletion. Preserve redacted artifacts plus private evidence separately.
5. Delete **only this run's** synthetic objects/buckets and temporary IAM/network/
   instance/volume resources, including test EBS without backup after exporting
   evidence. Never touch bootstrap/state or prior project evidence. Report residuals;
   do not call a failed cleanup complete. AWS-managed KMS key remains service-owned.

## Planning allowance, USD before tax

Checked 2026-10-28 against public AWS prices; no account billing API was used.
No Free Tier, credits, commitment discounts or waived charges assumed. Planning
uses 730 hours/month and deliberately generous request allowances:

| Component | Assumption | Estimated USD |
| --- | --- | ---: |
| m7i-flex.large Linux | 2 h × 0.1197/h | 0.239400 |
| gp3 storage | 20 GiB × 0.096/GiB-month × 2/730 | 0.005260 |
| Public IPv4 | 2 h × 0.005/h | 0.010000 |
| S3 Standard storage | 10 GiB × 0.025/GiB-month × 2/730 | 0.000685 |
| S3 PUT/LIST | 10,000 × 0.000005 | 0.050000 |
| S3 GET/HEAD | 100,000 × 0.0000004 | 0.040000 |
| Symmetric KMS calls | 100,000 × 0.03/10,000 | 0.300000 |
| Total planning case | Calculated with Python Decimal | **0.645345** |

Approved **USD 1 before tax** as the operating allowance. This is an estimate and
approval boundary, not a real-time AWS billing hard limit. Stop and report if
setup, duration or observed request volumes require a larger run. S3/EC2 traffic
stays in the same Region; do not download the dataset to a remote client. Small
evidence export and setup downloads still need recording in the final cost window.

Sources: [EC2/EBS Singapore Price List CSV](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/ap-southeast-1/index.csv)
(m7i-flex.large SKU `YT6URNNK7PK3VC3Y`, gp3 `X8SY8CJFS8WV7VQN`, effective 2026-10-01),
[S3 Singapore Price List](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/ap-southeast-1/index.json),
[S3 transfer pricing](https://aws.amazon.com/s3/pricing/),
[KMS pricing](https://aws.amazon.com/kms/pricing/),
[public IPv4 pricing](https://aws.amazon.com/vpc/pricing/).
Recheck prices, permissions and the concrete resource plan before apply.
