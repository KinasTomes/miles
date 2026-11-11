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

R1 passed at 03:03 UTC with 20 fixture tables verified absent. R2 passed at 03:06 UTC, removing its S3 object versions and verifying absence of its 20 tables. These native-backend fixtures use modeled clocks and in-process nodes. Native GC, controlled alarm transitions, endpoint-policy restoration, teardown and final baseline-preservation audit remain pending. The latest state and step-by-step log are held in the private operator run directory `edge-acceptance-recheck/edge-retest-20261111T012450Z`.
