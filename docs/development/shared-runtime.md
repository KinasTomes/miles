# Shared Cloud and Edge runtime

Both profiles run the same binary and engine against one DynamoDB table inventory
and S3 bucket. Node-local disk contains only a disposable cache. See the
[Cloud example](../../configs/shared-cloud.example.json) and
[Edge example](../../configs/shared-edge.example.json). Replace every placeholder
outside Git. The minimal original profile examples remain unready skeletons.

## Startup and authority

`runtime_node_id` explicitly enables shared composition; it is an operator-supplied
node label. Each process adds a random incarnation to its registry ID. Startup
requires a provisioned R1 gate, enabled recovery history, verified immutable
catalog/genesis, complete shared global policy and shared encryption/cursor keys.
It does not seed metadata, mint client credentials or reset history on restart.

S3 binds private Cloud IP:8080 or Edge loopback:8080. Admin binds loopback:9000 in
the deployment templates. Health/metrics use private Cloud IP:9100 or Edge
loopback:9100; security groups expose neither management port to Internet clients.
Admission, backend checks and request deadlines still apply after readiness passes.

Every request loads a complete current global policy and binds its bucket's
current cache-policy snapshot before engine work. Read-memory admission and the
node cache remain shared across requests. After creating a bucket, read its cache policy through the Admin API to materialize
revision one before payload I/O; subsequent changes use that expected revision.
Missing policy history in the payload path is an error; only the explicit Admin
policy operation materializes the documented initial default.

Writer admission obtains history/epoch from the same durable lease transaction.
Multipart uploads, input/snapshot streams and destination manifests attach their
roots before publication. Heartbeats and request workers join before release;
unknown actions retain protection. Short-lived control leases close only after
their synchronous native transaction has returned. Failed cleanup prevents a
clean shutdown claim.

The journal supervisor rotates nine-minute hard leases without resetting the
durable hourly deadline. It uses the existing exporter/closure/seal worker, with
the same 60-second export and immutable-byte verification. Shutdown joins it,
waits for admitted requests, then exports remaining committed outbox events under
fresh recovery authority. A failed final export returns an error; it cannot emit
`shutdown complete`. This does not implement checkpoint/rebuild or physical GC.

## AWS provider and UTC source

Cloud uses only the IMDSv2 instance profile. Edge reads only `credential_process`
from the explicit `miles-runtime` profile; static keys, source profiles, web
identity and fallback providers are rejected. The SDK credential cache refreshes
temporary credentials before expiration. AWS identity and gateway client identity
are separate. A broken helper never falls back to the developer's credentials.
TLS, regional service endpoints and redirects are fixed by this wiring; emulator
endpoints exist only in test constructors.

The shared Secrets Manager value is a JSON document containing `key_id`,
`seal_key` and `cursor_key`: two independent, base64-encoded 32-byte random keys.
Provision it outside Terraform; never put values, tokens or private keys in
user-data, state or logs. All nodes must use the same document. Replacing the
sealing key without migrating existing sealed credentials makes them unreadable.
The cursor key also MACs S3 LIST continuation tokens with a separate protocol
binding, so pagination works across nodes and restarts. All nodes need the same
key; replacing it invalidates outstanding tokens (one-hour lifetime).

Install operator-managed chrony with an approved IPv4 upstream. Set
`utc_reference` to its numeric reference address; Cloud uses the Amazon link-local
time source. The Miles user needs only the localhost monitoring commands, not
chrony's privileged Unix control socket. Disable clock stepping after initial
synchronization and restrict who can modify chrony or host time. The adapter reads
fresh CSV tracking observations, checks selected source/reference ID, sync, age,
offset and finite uncertainty, and includes command latency plus frequency error
through a 45-minute horizon. Unsynchronized, stale, changed or overly uncertain
observations reject authority and readiness.

This trusts the local OS/chronyd and the approved upstream; it is not cryptographic
NTP attestation. The uncertainty calculation follows the
[chronyc tracking contract](https://chrony-project.org/doc/4.6.1/chronyc.html#_system_clock_commands).

## First installation and owner forwarding

Provision only after reviewing the real infrastructure plan and operator IAM.
With no nodes running, use an operator-only Edge configuration whose
`credential_process` exports the temporary deploy-role credentials, then run:

```bash
./bin/dsgctl admin-token bootstrap --runtime-config /private/operator-startup.json \
  --user operator --token-file /private/system-admin-token
```

All 20 tables must be empty. The durable 0600 token is written before the first
transaction. Provisioning installs the first user/token/policy, a fresh R1 history
and its immutable genesis/catalog, then enables recovery. The genesis document
`runtime_genesis_version=1` contains the exact offline user, hashed token, quota
and policy records; a future rebuild must consume this base before journal replay.
This base is encrypted in the private backend; it contains no plaintext secrets.
An uncertain or partial failure keeps the token and fails closed. Reconcile the
installation and artifacts before retrying; this command is not a resume/reset tool.

Create a separate owner user and user-scoped token through the Admin API; do not
reuse the system token as the owner's everyday credential. Before serving Edge,
replace its operator-only provider with the scoped Roles Anywhere profile.

Cloud's optional `owner_ssh_public_key` enables a restricted `miles-owner` account:
only local forwarding to 127.0.0.1:9000, no shell/PTY/agent/sudo. The ASG remains
private, with no public SSH listener. An approved private SSH path is required;
SSM SSH forwarding needs a separately reviewed document permission, which the
existing audited-shell grant does not provide. Verify/pin SSH host keys through
the existing trusted management channel before connecting.

```bash
python3 -B scripts/admin_tunnel.py --ssh-config /private/ssh-config \
  --target cloud-a --target cloud-b --token-file /private/owner-token --user-id owner
./bin/dsgctl --endpoint http://127.0.0.1:9000 --token-file /private/owner-token usage owner
```

The tunnel authenticates the owner's own usage read and moves to the next pinned
alias when the process/endpoint becomes unhealthy. A token expiry also fails the
probe; replace the token file through the ordinary token lifecycle. The script
never opens management ports or copies a token to a remote shell command.
