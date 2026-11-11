# Private Edge AWS recheck

The November 11 Singapore run began at 02:03 UTC. The cleanup deadline is 06:03 UTC, within the approved four-hour, 10 USD envelope. Runtime source is `328b700`. The ARM64 release archive SHA-256 is `b7f59f92b1db5d7d9914f60a746329bbafc5be876aa701cc86b417fe4a94db61`. Terraform source `9332e90` adds a reference to the pre-existing Auto Scaling service-linked role. Both Cloud nodes, Edge and client run privately on AWS.

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

## Outstanding work

At 02:56 UTC the deploy session expired before the first native fixture started. After refresh, R1 exposed missing fixture access in the DynamoDB gateway endpoint policy. R2 exposed missing S3 endpoint permission to read the journal bucket's versioning. Both gateway policies were saved, then temporary additions were limited to the deploy role, fixture prefixes and the approved expiry. Action probes from the AWS client preceded the successful fixture retries. Earlier table attempts were reconciled and their absence verified.

R1 passed at 03:03 UTC with 20 fixture tables verified absent. R2 passed at 03:06 UTC, removing its S3 object versions and verifying absence of its 20 tables. GC passed at 03:29 UTC, covering PUT reuse/retirement, GET, copy compaction and version sweep. Its 20 fixture tables were verified absent. These native-backend fixtures use modeled clocks and in-process nodes. Both gateway endpoint policies were restored after the fixtures; comparison accounts for AWS canonicalizing single-element Action/Resource arrays to strings.

## Deployed-runtime observations

Cloud-a cache purge timed out twice with `partial_cluster/node_timeout`. The target child never started despite fresh Ready heartbeats. A long-running `journal_flush` job held that node's serial runner during this interval; other periodic jobs resumed afterward and a third purge succeeded. Preserve both failed outcomes. The successful retry does not establish a bound for control-job latency under journal backlog.

The first real hourly epoch began closing at 03:21:51 UTC and reached Closed at 03:23:08 with 1,242 events. It remained unsealed at the 03:33 observation; 1,022 events had persisted export descriptors. The runtime correctly withheld recovery observations while the first closed epoch lacked a seal. Both recovery-age and telemetry-age alarms entered ALARM. Whether sealing can finish within the rotating worker's authority budget remains under investigation.

At 04:35 UTC, all 1,242 events had export descriptors, but neither the first epoch nor the subsequent empty epoch was Sealed. The first epoch's owner had advanced to generation 15 of the periodic flush job. Export eventually completed; publication of a usable recovery cut did not. The eight-alarm acceptance remains incomplete. Do not describe this as a permanent export stall or claim data corruption from these observations.

Controlled corrupt-pack reads returned HTTP 502 through both proxies, incremented the runtime integrity counter and triggered both integrity alarms. Removing the faults restored GET 200 with the expected SHA-256. This differs from the operator script's initial HTTP 500 assertion; that assertion was not used as evidence of runtime failure. A separate 180-second DDB-throttle fault was then applied to all three gateways. Alarm recovery, final validation of all eight histories, teardown and baseline-preservation audit remain pending.

The latest state and step-by-step log are held in the private operator run directory `edge-acceptance-recheck/edge-retest-20261111T012450Z`.

## Recovery correction tested live

Release `063cd72` reuses an existing export receipt only after reading back and verifying its descriptor, pages, dependencies and exact native source event. A Closed epoch goes directly to sealing instead of first repeating a complete export scan. Sealing still verifies exact membership and independently reads the finished cut before fenced publication. No metadata format or public API changed.

The new receipt-retry regression failed against the previous source and passed with the correction. Recovery/app/jobs race tests, `make check`, native DynamoDB Local/Moto recovery contracts and process smoke tests passed. A separate existing CAS test occasionally included a request identical to the default policy; its successful no-op made the test incorrectly count two winners. Commit `3193fac` makes all contenders change the policy; 30 repeated race runs and CI 5/5 passed.

The old Edge invocation exited unsuccessfully before restart; the recorded restart command found MainPID 0 and sent no SIGKILL. Original Cloud instances were terminated using explicit disposable-workload ABANDON actions. These are not clean-shutdown acceptance evidence. The corrected Edge became ready at 04:45 UTC. Its takeover sealed epoch 0 by 04:49 and the empty epoch 1 by 04:51. Native state advanced NextToSeal to 2. GET returned 200 with the expected SHA-256 and a fresh Edge cache purge succeeded.

The first replacement Cloud bootstrap failed because the operator uploaded the archive under a filename different from Terraform's release key. The pinned version therefore belonged to the wrong key. Failed console output and native instances/root volumes were preserved in the private ledger. The corrected upload uses the expected key with verified VersionId and length. Cloud revalidation and final cleanup remain in progress within the original budget and deadline.
