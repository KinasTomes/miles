# Private Edge AWS recheck

## Latest run: recovery, alarms and shutdown

**Partial acceptance; keep Draft.** Recovery, all eight alarms and Edge shutdown passed. Clock-source freshness under load and the original PUT conflict remain unresolved.

This Singapore run began at 06:17:48 UTC on November 11 with an approved four-hour, 10 USD envelope. Cloud ran `5143cfe`. Edge progressed from that source to `1ad93fe` for safe conflict diagnostics, then `0ee6d2f` for admission cleanup recovery. No clock threshold or Chrony tuning changed.

| Edge trial | PUT | HEAD | Successful SHA-256 GET | Result |
|---|---:|---:|---:|---|
| Initial C1, `5143cfe` | 47/48 dispatched | — | — | PUT returned 409 OperationAborted; fail-fast stopped dispatch |
| Diagnostic C1, `1ad93fe` | 90/90 | 90/90 | 360/360 | PASS with a verified Chrony window |
| Diagnostic C4, `1ad93fe` | 64/68 dispatched | — | — | Four writes failed after the NTP reference became stale |
| C1, `0ee6d2f` | 90/90 | 90/90 | 160 | GET stream failed at 08:45:26 UTC with `clock_uncertainty_exceeded`; corrected collector returned UNVERIFIED |
| C4, `0ee6d2f` | — | — | — | Skipped after C1 failure |

The first hourly epoch sealed 934 events. Independent S3-only, write-forbidden `VerifyCut(epoch0)` passed. Empty epoch 1 subsequently sealed and `NextToSeal` advanced to 2. Available CloudWatch samples for the 08:23–08:33 UTC idle window reported zero Outbox read capacity and no native Chunks/Outbox read throttles; metric-delivery lag is recorded with the raw query results. Only Chunks read capacity changed to the approved 512; writes and all other tables remained 128.

All eight Cloud/Edge alarm cases passed the 08:49:23 UTC snapshot: exact definitions, native OK → ALARM → OK histories, corresponding metric samples and fresh runtime telemetry near recovery. Controlled corrupt-pack GETs returned 502 while active, then 200 with the expected SHA-256 after expiry. Injected SDK errors are recorded separately from natural DynamoDB capacity throttles.

The pre-diagnostic `5143cfe`, post-DynamoDB-fault `1ad93fe`, and final `0ee6d2f` Edge invocations each exited 0 with a matching `shutdown complete` message. The corrected Edge stopped at 08:50:41 UTC. Both Cloud instances terminated without operator ABANDON; this alone is not exact-invocation clean-shutdown proof for Cloud.

A native regression reproduced the permanent cleanup-error boolean after a failed lease close was later durably recovered. `0ee6d2f` tracks individual fences, confirms release or safe supersession and preserves unknown pending work. Active/released/superseded/pending cases passed native race tests; `make check` passed. The latent error was not reproduced live. `a928c21` corrects collector freshness coverage between observations and the final response. `f56e265` corrects counter-alarm validation to accept repeated breaches before a quiet recovery interval; its regression still rejects breaches within that interval. CI passed 5/5 on that source.

The original 409 was not reproduced or claimed fixed by safe stage logging. The later streaming GET failure independently confirms the clock availability issue. A post-failure NTP snapshot reported 507 TX, 507 valid RX and 449 good RX; cumulative counters do not identify why the accepted reference was stale in the failure window. See the [continuation plan](../planning/edge-aws-acceptance-continuation.md) for timestamped measurement logging and the unchanged fail-closed boundary.

Cleanup started at 08:49 UTC. Evidence from all 20 tables was exported and 3,991 synthetic S3 versions removed after gateways stopped. Terraform destroyed all 117 owned managed resources and its state is empty. The imported certificate was deleted, deploy policies/boundary restored, temporary grants removed and auditor absence verified. Both run-created service-linked roles are absent. ELB role deletion initially failed because AWS retained the deleted ALB reference; bounded retries succeeded at 09:29 UTC. Independent final audit at 09:34:25 UTC returned **PASS_WITH_PENDING**: 88 identifiers checked, 86 absent, exactly the approved new KMS key and secret pending deletion for 30 days, zero unapproved residuals/errors and baseline preservation accepted. Bootstrap/state and the existing Auto Scaling role were preserved. Operator tunnels/bridge and the private audit CLI profile were removed, both AWS CLI profiles logged out, and login-code files cleared. Cleanup finished before the 10:17:48 UTC deadline.

The operating estimate is approximately 4.46 USD before tax, including service reserves; it is not a final bill. Raw evidence, frozen provenance, failed attempts and cleanup ledgers are retained privately under `edge-acceptance-recheck/edge-final-check-20261111T060908Z`.

## Earlier completed run

**Partial acceptance; keep the PR draft.** Edge C1/C4, native R1/R2/GC, live epoch sealing and independent S3-only cut verification passed. Six of eight runtime alarm histories passed; both throttling recovery cases remain incomplete under the 128 read caps. The subsequent shutdown retry correction passed local/native tests but has not run on AWS. Cleanup completed at 05:46 UTC with the independent final audit accepted: 94 identifiers absent and only the two approved KMS/secret deletion windows pending.

The November 11 Singapore run began at 02:03 UTC. Cleanup finished before the 06:03 UTC deadline, within the approved four-hour, 10 USD envelope. The initial runtime source was `328b700`. The ARM64 release archive SHA-256 is `b7f59f92b1db5d7d9914f60a746329bbafc5be876aa701cc86b417fe4a94db61`. Terraform source `9332e90` adds a reference to the pre-existing Auto Scaling service-linked role. Both Cloud nodes, Edge and client ran privately on AWS.

## Results collected

| Trial | UTC window | PUT | HEAD | SHA-256 GET | Clock evidence |
|---|---|---|---|---|---|
| Edge C1 | 02:31:57–02:40:17 | 90/90 | 90/90 | 360/360 | Verified, 252 Chrony samples |
| Edge C4 replay | 02:49:06–02:55:36 | 90/90 | 90/90 | 360/360 | Verified, 196 Chrony samples |

Both trials used synthetic `backup30`, 90 files totalling 754,994,670 bytes, with manifest SHA-256 `04014dd767e799d9f70081536d01c988fa6a02c0b7653c34db11a48c9abc1556`. They completed with zero failed benchmark requests. Each trial had a fresh user/domain and bucket. CreateBucket and LIST preceded the first PUT, with no Admin policy GET/PUT in between. This validates the virtual default-policy correction on AWS.

The original C4 trial also completed all requests successfully. During recovery from an interrupted operator session, an incomplete result snapshot was read and the recorder was stopped early. Its clock window is `UNVERIFIED` with `unobserved_chrony_window_end`. Original and resumed segments are retained privately. The replay used a new bucket and its own complete recorder window.

Two Cloud instances were InService and both ALB targets healthy before C1. Edge Roles Anywhere identity, TLS, readiness and the packaged telemetry probe were verified. Recorder summaries cover the trial start and final response, with successful recorder exits and raw/summary hashes retained privately.

The Edge runtime throttle counter increased by two during the C4 replay window. No benchmark request failed. Native CloudWatch metrics recorded 1,139 Chunks `ReadMaxOnDemandThroughputThrottleEvents` in the captured replay window, with the table read cap at 128 request units per second.

## Deployment correction

Importing the existing Auto Scaling role into the initial fleet configuration proposed description and tag changes. `existing_autoscaling_role_arn` now references the baseline role outside managed state. The operator backed up state, removed the imported binding and verified the saved plan excluded SLR mutation. Terraform mock tests passed 51/51. The runtime release remained pinned throughout.

## Native fixtures

At 02:56 UTC the deploy session expired before the first native fixture started. After refresh, R1 exposed missing fixture access in the DynamoDB gateway endpoint policy. R2 exposed missing S3 endpoint permission to read the journal bucket's versioning. Both gateway policies were saved, then temporary additions were limited to the deploy role, fixture prefixes and the approved expiry. Action probes from the AWS client preceded the successful fixture retries. Earlier table attempts were reconciled and their absence verified.

R1 passed at 03:03 UTC with 20 fixture tables verified absent. R2 passed at 03:06 UTC, removing its S3 object versions and verifying absence of its 20 tables. GC passed at 03:29 UTC, covering PUT reuse/retirement, GET, copy compaction and version sweep. Its 20 fixture tables were verified absent. These native-backend fixtures use modeled clocks and in-process nodes. Both gateway endpoint policies were restored after the fixtures; comparison accounts for AWS canonicalizing single-element Action/Resource arrays to strings.

## Deployed-runtime observations

Cloud-a cache purge timed out twice with `partial_cluster/node_timeout`. The target child never started despite fresh Ready heartbeats. A long-running `journal_flush` job held that node's serial runner during this interval; other periodic jobs resumed afterward and a third purge succeeded. Preserve both failed outcomes. The successful retry does not establish a bound for control-job latency under journal backlog.

The first real hourly epoch began closing at 03:21:51 UTC and reached Closed at 03:23:08 with 1,242 events. It remained unsealed at the 03:33 observation; 1,022 events had persisted export descriptors. The runtime correctly withheld recovery observations while the first closed epoch lacked a seal. Both recovery-age and telemetry-age alarms entered ALARM. The later live correction below resolves this observation.

At 04:35 UTC, all 1,242 events had export descriptors, but neither the first epoch nor the subsequent empty epoch was Sealed. The first epoch's owner had advanced to generation 15 of the periodic flush job. Export eventually completed; publication of a usable recovery cut did not. The eight-alarm acceptance remains incomplete. Do not describe this as a permanent export stall or claim data corruption from these observations.

Controlled corrupt-pack reads returned HTTP 502 through both proxies, incremented the runtime integrity counter and triggered both integrity alarms. Removing the faults restored GET 200 with the expected SHA-256. This differs from the operator script's initial HTTP 500 assertion; that assertion was not used as evidence of runtime failure. A separate 180-second DDB-throttle fault was then applied to all three gateways. The final alarm and cleanup results are recorded below.

The latest state and step-by-step log are held in the private operator run directory `edge-acceptance-recheck/edge-retest-20261111T012450Z`.

## Recovery correction tested live

Release `063cd72` reuses an existing export receipt only after reading back and verifying its descriptor, pages, dependencies and exact native source event. A Closed epoch goes directly to sealing instead of first repeating a complete export scan. Sealing still verifies exact membership and independently reads the finished cut before fenced publication. No metadata format or public API changed.

The new receipt-retry regression failed against the previous source and passed with the correction. Recovery/app/jobs race tests, `make check`, native DynamoDB Local/Moto recovery contracts and process smoke tests passed. A separate existing CAS test occasionally included a request identical to the default policy; its successful no-op made the test incorrectly count two winners. Commit `3193fac` makes all contenders change the policy; 30 repeated race runs and CI 5/5 passed.

The old Edge invocation exited unsuccessfully before restart; the recorded restart command found MainPID 0 and sent no SIGKILL. Original Cloud instances were terminated using explicit disposable-workload ABANDON actions. These are not clean-shutdown acceptance evidence. The corrected Edge became ready at 04:45 UTC. Its takeover sealed epoch 0 by 04:49 and the empty epoch 1 by 04:51. Native state advanced NextToSeal to 2. GET returned 200 with the expected SHA-256 and a fresh Edge cache purge succeeded.

The first replacement Cloud bootstrap failed because the operator uploaded the archive under a filename different from Terraform's release key. The pinned version therefore belonged to the wrong key. Failed console output and native instances/root volumes were preserved in the private ledger. The corrected upload uses the expected key with verified VersionId and length. Both were completed within the original budget and deadline as recorded below.

## Final runtime result

The corrected Cloud pair became healthy behind the ALB, alongside Ready Edge. All three new cache purges succeeded. Controlled corrupt-pack GETs returned 502 through both proxies; removing the faults restored GET 200 with the expected SHA-256. Controlled DDB-throttle faults expired and HEAD recovered on both profiles.

The pre-cleanup native evidence validated six cases, including fresh recovery telemetry: Cloud/Edge recovery-cut age, integrity and telemetry age. Both throttling alarms remained ALARM. Native service metrics recorded real read-cap throttles in Chunks and Outbox separately from the injected SDK errors. Both tables retained read/write caps of 128. The complete eight-alarm acceptance is **not passed**.

At final stop, the corrected Edge process exited 1 because shutdown journal flush encountered a native DynamoDB Scan `ThrottlingException`. Recovery-cut correctness remains verified; clean shutdown is not accepted. Commit `88e605f` retries `ErrUnavailable` after successful lease cleanup, within the original eight-minute flush budget. Fencing and failed cleanup still stop the retry. The added regression failed on the previous source; deadline/fencing race tests and native shutdown/peer-recovery tests passed. This shutdown correction has **not** been retested on AWS.

An optional capacity/90-minute extension proposal was prepared, but no approval arrived by the original 05:13 UTC cleanup gate. The operator kept both caps at 128 and began cleanup. After application processes were stopped or instances terminated, final metadata evidence was exported and 3,686 synthetic S3 versions removed. The 117-resource delete-only plan ran; a duplicate ARN prefix in the private SSM cleanup grant was corrected before deleting the last parameter. Terraform managed state became empty. Temporary grants, managed-policy versions and the runtime boundary were restored to baseline. Both service-linked roles created by the run were removed; the pre-existing Auto Scaling role remained unchanged. ELB role deletion initially reported the already-deleted ALB as in use, then succeeded on bounded retry. The final independent audit at 05:46:53 UTC returned `PASS_WITH_PENDING`: 96 identifier checks, 94 absent, two approved pending deletions, no residuals or unexpected errors, and baseline preservation accepted. The KMS key and secret retain their approved 30-day deletion windows. Operator tunnels/credential bridge were stopped, both AWS CLI sessions logged out, and authorization-code files cleared.

The conservative operating estimate is 4.52 USD before tax, within the approved 10 USD envelope. It uses observed DynamoDB consumption plus regional pricing and reserves for other services; it is not a final bill. Billing reconciliation remains separate from native absence checks.

## Idle Outbox discovery correction

An open epoch with no mutations still caused every journal flush to scan historical Outbox records, including already sealed epochs. Commit `e07cfd7` changes the DynamoDB adapter to check the current producer-enforced gate, the absence of an existing closure record and strongly consistent atomic mutation membership. Only an empty current Open epoch at the start of discovery skips the historical scan. Registered, legacy, closing and Closed epochs retain independent base enumeration and existing pagination. A failed authority/membership read fails closed. This avoids schema/index changes and does not remove final membership/hash verification.

The regression reproduces the unnecessary scan on the previous implementation and checks that missing membership cannot hide Closed Outbox events. Race and native DynamoDB Local/Moto recovery/pagination/seal tests, shared Cloud/Edge runtime tests and `make check` passed. This correction and the shutdown retry require a new approved AWS run; neither is included in the live release evidence above.
