# Lifecycle V1 to V2 rollout gate

Run this procedure before deploying a writer that creates lifecycle action V2 rows.
The inspector is read-only and exits nonzero on any unresolved legacy action or
if it cannot read or decode any required row. It paginates the complete
`SystemConfig` scan and strongly reads each matching action's Job and R1
producer lease.

## Operator procedure

1. Schedule a maintenance window. Through the supported R1 maintenance
   procedure, close writer admission and wait until the durable R1 gate is
   `Maintenance` with `Active=0` and `Resolving=0`. Then stop every Miles
   gateway and lifecycle/job worker. Keep them stopped through inspection and
   rollout; DynamoDB strongly consistent Scan is paginated but is not a
   table-wide snapshot. The executable gate independently verifies the durable
   R1 maintenance state before scanning.
2. With the deployment role's read-only DynamoDB credentials and the deployed
   table prefix/selected Region, run:

   ```text
   dsgctl lifecycle-upgrade-gate --ddb-table-prefix <prefix> --region <region>
   ```

   For DynamoDB Local only, add `--ddb-endpoint http://127.0.0.1:<port>`.
   The command needs `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional
   `AWS_SESSION_TOKEN`. Grant only `dynamodb:Scan` on the prefixed
   `SystemConfig` table and `dynamodb:GetItem` on prefixed `Jobs` and `Leases`.
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
5. If rollout fails after deployment begins, keep workers stopped and roll back
   to the prior V1 binaries only if no V2 action has been created. If V2 rows
   exist, do not start an old reader (it rejects V2); restore the V2 release and
   have the owner resume forward recovery. Reopen admission only after the
   selected release is healthy and the lifecycle worker set is consistent.

The command does not modify DynamoDB, change maintenance state, or deploy
anything. Its exit code is suitable for a deployment pipeline precondition.
