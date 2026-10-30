# Shared R1 gate, lease và pins trên DynamoDB

Task: W04-PHONG-03. Baseline `origin/main` tại `92deb23`.
Board lúc kiểm tra: Doing/Ready, checklist0/3; W03-PHONG-02,
W04-HAI-01 và W04-HAI-02 đều Hard Done.

## Mapping ba checklist

| Checklist | Implementation/evidence |
|---|---|
| Shared producer admission/gate/fencing/incarnation, renewal, read view/location pins | `storage.R1Store`, DynamoDB `SharedAuthority`/R1 native transactions; `coordination.NewSharedR1Gate`; atomic observed-head binding; paged exact PackRef membership |
| PUT/GET/Copy/multipart prepare/publish, maintenance gate, stale workers | Managed WriteSession/StorePublisher và WarmReader bridge; strict producer PUT→peer GET/Range; streaming Copy source→target khi gate Closing; multipart upload/part preparation và rejection sau closure; opt-in maintenance protocol |
| Expiry/skew, busy503, S3 unknown state | Conservative UTC validation, renewal equality/grace vectors; native transaction-settlement orderings; HTTP busy503; native Deleting/Transitioning với unknown physical I/O seam |

Đây là implementation/evidence trên DDB Local/dev. Không tự xác nhận board
Done/Approved/Frozen, Cloud multi-node, real AWS, NTP/chrony attestation hoặc
GC/R2 retention đã sẵn sàng.

## Shared records và transaction boundary

Không tạo database/metadata store thứ hai hoặc đổi table/key inventory.
Records v1 nằm trong các namespace reserved hiện có:

- `SystemConfig/coordination/r1/current`: history, producer epoch, service
  incarnation, gate state/revision, active và resolving membership counters.
- `Leases(resource_id=r1/producer, lease_id=ID)`: exact fence, owner, logical
  operation, deadline, conservative expiry, state/revision, logical roots,
  pending actions, pin count và resumable release cursor.
- `Leases(resource_id=r1/pack/{pack_id}, lease_id=generation)`: exact generation
  pin count. Native version/identity/count bounds được validate trước sử dụng.
- `Leases(resource_id=r1/view/{lease_id}, lease_id={fence_epoch}/pin/{hash})`:
  exact per-lease pin membership và released marker. Hash bind full PackRef;
  lease epoch tách pin membership của slot được tái sử dụng.
- `SystemConfig/coordination/r1/action/{action_id}`: exact maintenance target,
  state revision, action/class, incarnation và Pending/Unknown/Done outcome.

`record_kind=r1_coordination_v1` phân biệt operational R1 records với global
policy. Recovery không replay chúng thành active execution rights. R1 state
không là recovery seal, checkpoint, complete epoch hoặc GC root proof.

Acquire atomically check Open/current incarnation và tạo owner-bound lease,
tăng active/resolving cùng transaction. Same request retry giữ identity/binding;
khác owner/object/deadline/operation conflict. NewSharedR1Gate thêm per-instance
nonce vào owner nên runtime cùng configured node name không tự dùng chung một
executing handle. Init là provisioning-only, không overwrite incarnation đang chạy.

Existing metadata authority seam Observe đưa gate/lease rows vào **cùng**
native mutation transaction. CheckManifest/CheckMultipart yêu cầu logical root
đã bind; strict producer checks history/epoch/logical parent/object binding.
Reader lease không thể dùng làm writer authority. Final commit-attempt
registration cũng condition-check R1 gate/lease revisions. Shared sends dùng
explicit dispatch checks và tắt hidden SDK retries; nguyên token/params vẫn do
existing attempt/outcome protocol giữ khi unknown.

Gate Closing chặn producers/readers mới bằng ErrR1Busy (wrap ErrUnavailable),
nhưng không giết admitted cohort. Existing producer có thể tiếp tục root
preparation/publication dưới active lease. Global counter row là conservative
dev coordination point, có contention; không claim high-throughput distributed
benchmark hoặc dùng counter này như exact R2 event membership seal.

## Lease time, closure và takeover

Backend cần explicit SharedAuthority.Time và Lifetime; không fallback host
wall time. `ValidatedUTC` dùng cùng `policy.ValidateUTCSample`: verified source
provenance/restart, sample age<5s, skew≤2s, precision/error/drift và validity
horizon. Request-local monotonic lifetime/deadline tiếp tục được kiểm độc lập.
Fixture UTC oracle chỉ là modeled evidence; production phải supply actual
verified time-source adapter trước khi enable profile phụ thuộc shared UTC.

New dispatch yêu cầu conservative U<lease expiry và request deadline. Renewal
không bắt đầu tại equality/expired authority. Candidate expiry dùng explicit
lifetime, cap bởi original deadline; không kéo dài hard request budget.
Service incarnation, producer epoch, fence epoch và policy revision là các
domains khác nhau.

CloseR1 atomically closes publication authority; không tự giảm membership hoặc
pins. Reclaim chỉ được close khi L≥expiry+explicit grace; không tự cấp quyền mới
hay suy no-commit. Action ID được persist trước synchronous side effect. Timeout,
cancellation/lost ACK và absent head không settle action. Unknown giữ pending
membership/protection. Trusted coordinator chỉ settle sau terminal synchronous
result hoặc authoritative original-outcome reconciliation. Release là internal
quiescence operation sau local work/heartbeat/I/O đã join, không là public API
để peer tùy ý dọn expired lease. Process death cần independent isolation và
reconciliation trước cleanup; không có automatic TTL cleanup/takeover shortcut.

Native tests phân biệt hai orderings: send hợp lệ trước E có thể settle sau E
nếu closure chưa thắng; closure commit trước publication thì native CAS reject
late transaction. Không sửa thành strict server-clock commit-before-E. New
fence của cùng lease slot chỉ được acquire sau old lease Released, epoch tăng
đúng1; unknown hoặc old owner còn retained thì reject. Incarnation rotation
yêu cầu gate Closing và active membership0, không force-rotate live workers.

## Read roots và paged location pins

Shared reader acquisition xảy ra sau existing permission checks. BindR1
condition-check exact current ObjectHead trong cùng transaction ghi view root.
Overwrite sau binding không đổi quyền đọc của view đã admitted. Pin attaches
exact generations và checks lifecycle state under native CAS; resolve barrier
chỉ hạ sau khi mọi batch pin đã thành công. Existing read path vẫn validate
canonical bindings/revisions, physical state, checksums và view trước emit.
Cache hit không bỏ qua shared view hoặc generation protection.

Pin batch≤32, tối đa98 native actions; không đưa toàn bộ pins vào lease item.
Closed authority freeze membership trước consistent base-table Query, không GSI.
Release xử lý32members/trang, cùng transaction decrement counters, mark member
released và advance cursor. Unknown ACK giữ protection và retry cursor idempotent;
active/resolving chỉ hạ sau trang cuối và PinCount0. Test80generations vượt giới
hạn một transaction, không double-pin/decrement sau lost ACK. Member/closure
records được retain; cleanup/retention optimization thuộc downstream lifecycle.

Local work tokens giữ roots tới decode/emitter/body callbacks quiesce. Shared
read release lỗi giữ protection để reconcile, không giả vờ cleanup xong. Local
Retire bị reject trong shared mode; không dùng local map để authorize shared
destructive I/O. Không giữ lock trên toàn GET, nhưng local handle methods có
short serialized RPC boundaries; đây chưa là contention/performance proof.

## Consumer integration và maintenance

Managed write session bind preparation root trước pages/canonical mutations;
Do ghi pending action trước dispatch và settle sau ACK/terminal result. Existing
commit reconciliation đọc original outcome trước khi settle retained commit ID.
WarmReader dùng opaque shared view cùng existing scheduler/cache/policy. Copy
consumer compose source read view và target write session riêng: native test
stream qua pipe, chuyển gate Closing giữa stream, rồi commit target thành công
và verify copied bytes. Không bổ sung CopyObject HTTP endpoint trong task R1.
Multipart callers bind upload/candidate roots vào admitted writer trước prepare;
native upload/part publisher tests kiểm root protection và stale rejection.
Complete API/snapshot orchestration vẫn thuộc consumer task, không được hứa
hoàn tất chỉ vì shared hooks đã có.

`RunSharedMaintenance` persist pack Deleting/Transitioning và exclusive gate
trước **một** physical dispatch. Action timeout/error giữ Unknown và gate
Maintenance; không reopen qua SetR1Gate, không redispatch cùng action ID. Finish
cần guard xác nhận original terminal outcome/isolation, không HEAD hoặc age.

**Default vẫn chặn destructive lifecycle.** `ProtectPackStore` không đổi.
SharedAuthority.Maintenance nil cũng reject trước I/O. Opt-in cần
R1MaintenanceGuard.Prepare/Terminal do GC/R2 owner supply, condition-check
authoritative reachability/retention/roots và immutable write-ahead intent qua
Observe trong cùng transaction. R1 counters/pins hoặc việc pass task này không
tự thay các proof đó. Tests dùng explicit durable fixture proof rows và physical
unknown-action seam; không claim actual AWS DELETE/COPY reconciliation.

## Verification và tái lập

Fixture project riêng: `miles-r1-contract`, pinned DynamoDB Local trong
`deploy/local/compose.yaml`, loopback only, in-memory. Native tests luôn đặt
`MILES_DDB_LOCAL_REQUIRED=1` và explicit endpoint; thiếu emulator là fail,
không được skip rồi báo native pass. Tables tạo random scoped prefix và cleanup
chính xác các tables test sở hữu. Không dùng AWS credentials/account thật.

Cases chính:

- `TestLocalR1AdmissionClosureRenewalAndTakeover`: concurrent independent stores,
  one owner, renewal equality, grace, unknown retains pins, stale fences/incarnation.
- `TestLocalR1LostAcknowledgementsKeepExactMembership`: lost acquire/action/close
  ACK, retained exact membership và no duplicate authority.
- `TestLocalR1PagedPinsReleaseResumesAfterLostACK`: 80 generations, bounded batches,
  persisted cursor/counters, reader lease không được publish như writer.
- `TestLocalR1DispatchSettlementBeforeAndAfterClosure`: two native LP orderings.
- `TestLocalR1ManagedPutPeerGetAndCopySourcePins`: strict authoritative producers,
  managed PUT→independent reader GET/Range, streaming Copy, source work retention.
- `TestLocalR1MultipartPreparationAndStalePublisher`: native upload/part preparation,
  successful part publication and rejection of publication after peer closure.
- `TestLocalR1MaintenanceUnknownKeepsClosedPhysicalState`: delete và transition,
  missing proof reject trước dispatch, unknown closure và terminal reconciliation.
- `TestSharedUTCRejectsSkewStaleAndUnverifiedSource`, `TestSharedR1GateBusyMapsTo503`.

Linux Go1.27.2/container chạy `go test ./...`, rồi native race suites cho
coordination/DynamoDB/write/readpath/multipart, `go vet ./...`, `go build ./cmd/...`.
Windows chạy native R1 cases và targeted regressions. Không đồng nhất DDB Local
với AWS multi-node/time-bound attestation hoặc destructive production enablement.

Kết quả 2026-10-30 trên baseline `92deb23`: full Go suite, các native race
suites nêu trên, `go vet ./...` và `go build ./cmd/...` đều pass. Regression
multipart publication bổ sung cũng pass với race detector và native DDB Local.
Linux Python script suite: 19 tests pass. Đây là bằng chứng dev fixture cho
ba checklist, không phải chứng nhận production AWS hoặc nguồn UTC thực tế.
