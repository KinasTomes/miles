# Lifecycle V1 to V2 rollout gate

Run this procedure before deploying a writer that creates lifecycle action V2 rows.
The inspector is read-only and exits nonzero on any unresolved legacy action or
if it cannot read or decode any required row. It paginates the complete
`SystemConfig` scan and strongly reads each matching action's Job and R1
producer lease.

## Operator procedure

1. Schedule a maintenance window. The service owner reads `GetR1Gate`, records
   its revision, and invokes the supported `SetR1Gate(expectedRevision,
   "Closing")` operation. This CAS closes admission to new leases without
   entering pack-specific Maintenance; do not start a GC/tiering action for the
   upgrade. If the revision changed, reread and retry only after confirming the
   gate is still in an expected `Open`/`Closing` rollout state. Let current
   leases drain, polling `GetR1Gate` until it reports `Closing`, `Active=0`, and
   `Resolving=0`. Then stop every Miles gateway and lifecycle/job worker. Keep
   the gate Closing and services stopped through inspection and rollout.
   Strongly consistent Scan is paginated but not a table-wide snapshot; the
   closed admission gate blocks new leases, and zero active/resolving leases
   means no admitted writer remains to mutate lifecycle state. The executable
   gate verifies the durable Closing state and zero counters independently.
2. With the deployment role's read-only DynamoDB credentials and the deployed
   table prefix/selected Region, run:

   ```text
   dsgctl lifecycle-upgrade-gate --ddb-table-prefix <prefix> --region <region>
   ```

   For DynamoDB Local only, add `--ddb-endpoint http://127.0.0.1:<port>`.
   The command needs `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional
   `AWS_SESSION_TOKEN`. Grant only `dynamodb:Scan` on the prefixed
   `SystemConfig` table, plus `dynamodb:GetItem` on prefixed `SystemConfig`,
   `Jobs`, and `Leases`. No write permissions are needed.

   Minimal IAM statement (replace the bracketed values before attaching it):

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": "dynamodb:Scan",
         "Resource": "arn:[partition]:dynamodb:[region]:[account-id]:table/[prefix]SystemConfig"
       },
       {
         "Effect": "Allow",
         "Action": "dynamodb:GetItem",
         "Resource": [
           "arn:[partition]:dynamodb:[region]:[account-id]:table/[prefix]SystemConfig",
           "arn:[partition]:dynamodb:[region]:[account-id]:table/[prefix]Jobs",
           "arn:[partition]:dynamodb:[region]:[account-id]:table/[prefix]Leases"
         ]
       }
     ]
   }
   ```
3. Proceed only when output says `"gate": "pass"` and exit status is zero. A
   terminal V1 action is allowed only if its Job is terminal with no pending
   step and its R1 lease is Released with no pending action, pins, or resolving
   state. No StepID is inferred or written by this tool.
4. If blocked or inspection errors, do not deploy V2. Keep the service stopped
   and have the service owner inspect each reported action/job/lease. Use the
   existing V1 runtime's supported reconciliation path to establish terminal
   physical outcome, clear the matching R1 pending protection through normal
   fenced reconciliation, and release the lease. Unknown outcomes must be
   resolved from authoritative provider terminal evidence; never edit rows or
   invent a StepID manually. Rerun the gate after the owner confirms quiescence.
5. After the V2 release is healthy, use `SetR1Gate(expectedRevision, "Open")`
   to reopen admission, then start gateways and workers on the same V2 release.
   If rollout fails before any V2 action is created, keep workers stopped and
   use the revision-checked gate operation to reopen on the prior V1 release.
   If V2 actions exist, do not start an old reader (it rejects V2); restore the
   V2 release and have the owner resume forward recovery. Reopen admission only
   after the selected release is healthy and the lifecycle worker set is
   consistent.

The command does not modify DynamoDB, change maintenance state, or deploy
anything. Its exit code is suitable for a deployment pipeline precondition.
