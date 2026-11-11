# Private Edge AWS validation

## Result

The 2026-11-10 Singapore attempt stopped at the first Edge C1 PUT. The runtime used release source `e87c665`, a dedicated private Edge/client pair, shared DynamoDB/S3, certificate-based Roles Anywhere credentials and TLS. The client generated 90 synthetic objects totalling 754,994,670 bytes. LIST succeeded. The first PUT returned HTTP 404 with `NoSuchKey`, and fail-fast skipped the remaining 89 PUTs. C4, native recovery/GC and the eight alarm transitions were not accepted in this attempt.

The deployment window began at 15:31 UTC. Foundation provisioned successfully after correcting the test CA's certificate-signing key usage. Edge became ready, authenticated its Admin API and shut down with `shutdown complete`. The Cloud ALB listener was created, but the Cloud ASG did not launch because its custom service-linked role already existed. That pre-existing role was imported for reconciliation and removed from the cleanup state to preserve the baseline resource.

## PUT failure and fix

Frozen metadata contained the new bucket, a quota reservation and no ObjectHead or pack. There was no persisted bucket policy. The shared runtime's bucket-policy callback returned the missing-row error, while the Admin API defines an unedited bucket's policy as revision 1 with `DefaultCachePolicy`.

The November 8 runner fetched the Admin bucket policy after CreateBucket and before the benchmark. That GET persisted default revision 1 and allowed the write path to find it. The November 10 runner performed CreateBucket, LIST and PUT without the Admin policy call. The two releases used the same policy lookup in the shared runtime. [Repeatable sequence and evidence log](edge-aws-runbook.md).

The runtime now uses that same virtual default for a missing policy, while preserving bucket-not-found and other errors. Regression coverage performs a PUT before any Admin policy edit and verifies that the write does not materialize a policy. The regression passed under Go's race detector with DynamoDB Local and Moto. This correction has not yet been validated in another AWS workload.

## Host telemetry packaging

The recorder failed preflight because the copied script could not import `runner_diagnostics`. The attempted workload therefore has no verified contemporaneous host-telemetry window. The deployment gate should have checked the running recorder before dispatching the workload.

Release packaging now includes both the recorder and its parser dependency. A regression launches the bundle from an isolated temporary directory with Python isolated mode and verifies that the parser is available. Any next run must execute the bundle's recorder, confirm a valid probe and recorded samples, and only then dispatch the workload.

## Cleanup evidence

A private frozen manifest binds Terraform state, native instances/volumes, account baseline and the explicit ACM certificate to this attempt. The synthetic bucket's 14 versions were exported in an inventory and deleted. All 20 tables were exported before deletion. The Edge process stopped cleanly before its disk was removed.

Cleanup required additional narrowly scoped permissions for DynamoDB deletion/readback, network disassociation, alias deletion, and IAM role readback. These gaps were recorded with the temporary grants and their rollback ledger. The temporary grants and newly created policy versions must be removed before final comparison.

The independent read-only final audit completed at 16:28 UTC with `PASS_WITH_PENDING`. It checked 82 identifiers, finding 80 absent and the newly created KMS key and secret scheduled for deletion in 30 days. There were zero unapproved residuals or unexpected errors. All temporary policies and created managed-policy versions were removed, deployment policies and runtime boundary matched the pre-run baseline, and the temporary audit role and both newly created service-linked roles were absent. Terraform state contained no managed resources. Bootstrap/state and baseline resources were preserved. The root login profile was logged out after the audit.

The operating-cost estimate is approximately 1.25 USD before tax, using rounded endpoint/ALB hours and conservative service reserves. Observed DynamoDB consumption was 2,982 read units and 1,904 write units, approximately 0.0018 USD at the verified regional request prices. Metric arrival delays and billing reconciliation remain outstanding. The estimate excludes resources that existed before this run.

## Conditions for the next run

- Build the release containing the virtual bucket-policy default and telemetry parser fixes. Confirm the recorder probe and a recorded window before dispatch.
- Query the custom Auto Scaling service-linked role before planning. If it already exists, reconcile/import it with the matching active variables before the saved fleet plan, record it as a baseline exclusion, and preserve it during teardown.
- Prepare cleanup grants against the frozen resource identifiers before starting workload. Include deleted-table readback, both key and alias resources for alias deletion, and the Edge role’s instance-profile readback.
- Include certificate key usage `keyCertSign` in the test CA. Create and verify the complete chain before attempting the trust anchor.
- Preserve original failed evidence and use a fresh trial identity for the next workload. Re-run C1 before C4 and keep later gates conditional on successful C1/C4.
