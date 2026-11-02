# Closed writer commit reconciliation

`R1Store.ReconcileR1(ctx, fence)` is a trusted metadata operation for authority
already in `Closed`. It cannot renew that authority, retry publication, or turn
an absent result into permission to delete data.

For the exact original writer/object/operation/reservation binding, the backend
either reads the retained committed outcome or CAS-closes the original operation
row against all saved final transactions. That closure, release of a standalone
reservation's reserved/staged quota, settlement of the pending commit and the
producer's terminal receipt share one transaction. A lost acknowledgement keeps
the caller uncertain; a retry reads the durable result without applying quota
twice. A committed outcome leaves its used quota unchanged.

The operation handles only the original `commit/<op_id>` action. Unknown physical
I/O and other action kinds return busy and keep protection. Multipart reservations
remain owned by the multipart recovery protocol: fencing their commit attempt
does not consume upload/staging accounting or claim that an upload was aborted.

Successful reconciliation retains the closed lease's roots and pins. The owner
may release them only after joining its CPU, body/backend and acknowledgement
work. Expiry alone is not quiescence. The recovery cut can reclaim publication
authority after conservative expiry plus grace and reconcile a crashed writer,
while retaining its roots/pins for the independent physical cleanup protocol.

Epoch closure revisits the same producer page after reconciliation; it advances
the proof cursor only after reading the terminal receipt. The immutable seal
continues to require exact producer/mutation membership and verified journal
bytes. No event, head, counter reset or replacement receipt is fabricated.

The node registry's shutdown check retries release for joined writers. It drops
local uncertainty only after the trusted backend confirms reconciliation. Active
work, unknown physical I/O and unavailable reconciliation still prevent a clean
shutdown claim. Reading the original committed result remains valid after a
successful release and does not settle an already released lease again.

This adds an internal shared R1 port; alternate `R1Store` implementations must
implement the same proof boundary. It adds no public HTTP route, changes no
record version and does not make checkpoint/rebuild or retention/GC complete.
