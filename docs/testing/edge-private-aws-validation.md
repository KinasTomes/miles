# Private Edge AWS validation

## Result

The 2026-11-10 Singapore attempt stopped at the first Edge C1 PUT. The runtime used release source `e87c665`, a dedicated private Edge/client pair, shared DynamoDB/S3, certificate-based Roles Anywhere credentials and TLS. The client generated 90 synthetic objects totalling 754,994,670 bytes. LIST succeeded. The first PUT returned HTTP 404 with `NoSuchKey`, and fail-fast skipped the remaining 89 PUTs. C4, native recovery/GC and the eight alarm transitions were not accepted in this attempt.

The deployment window began at 15:31 UTC. Foundation provisioned successfully after correcting the test CA's certificate-signing key usage. Edge became ready, authenticated its Admin API and shut down with `shutdown complete`. The Cloud ALB listener was created, but the Cloud ASG did not launch because its custom service-linked role already existed. That pre-existing role was imported for reconciliation and removed from the cleanup state to preserve the baseline resource.

## PUT failure and fix

Frozen metadata contained the new bucket, a quota reservation and no ObjectHead or pack. There was no persisted bucket policy. The shared runtime's bucket-policy callback returned the missing-row error, while the Admin API defines an unedited bucket's policy as revision 1 with `DefaultCachePolicy`.

The runtime now uses that same virtual default for a missing policy, while preserving bucket-not-found and other errors. Regression coverage performs a PUT before any Admin policy edit and verifies that the write does not materialize a policy. The regression passed under Go's race detector with DynamoDB Local and Moto. This correction has not yet been validated in another AWS workload.

## Host telemetry packaging

The recorder failed preflight because the copied script could not import `runner_diagnostics`. The attempted workload therefore has no verified contemporaneous host-telemetry window. The deployment gate should have checked the running recorder before dispatching the workload.

Release packaging now includes both the recorder and its parser dependency. A regression launches the bundle from an isolated temporary directory with Python isolated mode and verifies that the parser is available. Any next run must execute the bundle's recorder, confirm a valid probe and recorded samples, and only then dispatch the workload.

## Cleanup evidence

A private frozen manifest binds Terraform state, native instances/volumes, account baseline and the explicit ACM certificate to this attempt. The synthetic bucket's 14 versions were exported in an inventory and deleted. All 20 tables were exported before deletion. The Edge process stopped cleanly before its disk was removed.

Cleanup required additional narrowly scoped permissions for DynamoDB deletion/readback, network disassociation, alias deletion, and IAM role readback. These gaps were recorded with the temporary grants and their rollback ledger. The temporary grants and newly created policy versions must be removed before final comparison.

The final account audit and scheduled KMS/secret exceptions are recorded in operator-private evidence. Its final status is added below after completion. Billing reconciliation remains pending AWS reporting.
