# W03-PHONG-02 — R1 integration skeleton trước chạy bảo trì

Baseline: `aa219407850dd6178e3e8906e10599daf2a4e1c3` (`origin/main`, fetch ngày
28/10/2026). Task chính thức: [W03-PHONG-02](https://miles.extase.dev/tasks/W03-PHONG-02).
Owner: Phong. Đây là skeleton **một process**, chờ review; API mới là PROPOSAL.
Không tuyên bố R1 distributed, API Frozen hoặc task Done.

Nguồn: [R1 §3–8](../contracts/r1-and-cache.md),
[Clock §3.4/§5](../contracts/clock-and-policy.md),
[storage handoff](../contracts/storage-handoff.md),
[decoder memory](../contracts/decoder-memory.md),
[Warm reader evidence](warm-read-validation.md).
Không sửa shared storage types/ports hoặc contracts W1.

## Checklist và phạm vi evidence

| Checklist Miles | Điểm tích hợp | Evidence |
|---|---|---|
| C1 — Ghép admission/read view/location pins/writer registration vào write/read interfaces | Reader dùng WarmGate/work token; BeginWriteSession quản lý authorize → registration → heartbeat → Prepare/Reserve/Commit/Delete → quiesce/release | WriteSessionOwnsPreparePublishAndCleanup (Fake và Badger); WarmR1ReadOverlapsManagedOverwriteAndDelete (cùng Badger/S3 adapter/gate) |
| C2 — Heartbeat/deadline/mất lease dừng stream/publish; destructive lifecycle chưa bật | Lease monotonic, hard deadline bất biến, Lost terminal; giữ token trong cả body/CPU work; wrapper chặn delete/tiering ở MVP | WriteSessionHeartbeatAndCopiedCloseWaitForBodyQuiescence; R1ReadLeaseHeartbeatAndHardDeadline; R1WriterHeartbeatCannotMoveRequestDeadline; WarmR1LeaseEqualityStopsNextChunk; ExplicitDevAndRuntimeUnavailable |
| C3 — Fake two-worker races; service_incarnation/fence xuyên prepare/publish | Barrier trước dispatch/sau effect; hai sessions tranh head CAS hoặc closure; backend nhận nguyên WriterFence | WriteSessionTwoWorkersHeadCAS (cả hai thứ tự); WriteSessionTwoWorkerFenceClosureBeforePublication; WriteSessionRecoveryIncarnationStopsPrepare; CoordinatedPreparePublishPropagatesExactFence |

Tiêu chí Miles: có điểm tích hợp R1 và failpoints; MVP không chạy GC/tiering
cạnh tranh bằng cơ chế chưa kiểm chứng. Evidence ở đây chỉ bao phủ skeleton.
Hưng quyết định nghiệm thu task; tài liệu này không tự tick checklist.

## Read integration và lifetime

`coordination.NewR1Gate(clock, serviceIncarnation, LeaseLimits)` tạo boundary local.
Lifetime/Heartbeat được inject, phải dương và Heartbeat < Lifetime. Ví dụ trong
tests dùng 60s/15s từ Clock contract, không thêm production defaults.
`NewWarmGate` vẫn giữ chế độ Warm cũ, không có lease/heartbeat mới: consumer muốn
R1 skeleton phải chọn constructor mới. Không đổi ngầm behavior consumer cũ.

Luồng của WarmReader:

1. Authorize tại admission, pin bucket policy và resource admission như trước.
2. Acquire resolving guard; BeginWork giữ view cho toàn request.
3. Start KeepAlive bằng Clock.Sleep được inject; bind root, resolve canonical
   rows, pin exact generations theo batch <=100, đọc lại location revision.
4. Chỉ bỏ resolving guard sau khi đã pin toàn requested range. Logical root
   không tự bảo vệ mọi physical generation. Whole-range archive preflight,
   policy evaluator, resource reservation và production verification giữ nguyên.
5. Check view/incarnation/deadline ở các safe points trước fetch/cache/emit.
   Read authorization vẫn chỉ lấy tại admission; không reauthorize mỗi chunk.
6. Cancel/join heartbeat, đợi scheduler/fetch/decode/emitter quiesce, hoàn tất
   work token rồi Release. Cancellation/expiry/invalidation không tự unpin.

Release đóng attach/new work ngay. Nếu còn work token, guard/pins vẫn tồn tại;
token cuối cùng Finish mới cleanup. Bản sao handle/token dùng chung state,
không double-decrement. Lost terminal, heartbeat muộn không khôi phục authority.
Equality ở lease/hard deadline là expired. Heartbeat luôn bị chặn bởi hard deadline.
Candidate renewal được lấy trước validation old lease; regression stall qua E
không cho phép lấy sample muộn để tạo budget mới. Backend trả ErrFenced cũng
đặt writer vào Lost terminal, không dispatch tiếp physical writes.

KeepAlive không làm goroutine execution deterministic. Tests ordering dùng
barriers/channels và Virtual Clock; không dùng sleep để tạo race window.

## Write integration và authority

`NewWriterRegistry` dùng cùng WarmGate và incarnation. `Register` nhận fence từ
trusted integration đã được backend admit; **không ghi Writers table**, không
mint lease, không cấp quyền từ recovery epoch. Backend vẫn kiểm fence/auth/quota.

`write.BindCoordination` trả consumer adapters trên các ports hiện có:

- canonical metadata: RegisterUploadingPack, MarkPackWarm, insert/location CAS;
- immutable manifest staging và reservation;
- physical PutPack/PutSidecar; giữ optional ReadSidecar verification extension;
- CommitObject/DeleteObject và original GetCommitOutcome;
- ReuseGuard: đọc canonical row, pin exact generation, đọc lại row/revision.

StorePublisher và PrepareObject thật dùng các adapters này; không viết pipeline
hoặc MetadataStore thứ hai. Fence gồm LeaseID/ServiceIncarnation/Epoch giữ nguyên
qua mọi bước. Đổi incarnation/fence trên request bị reject trước gọi backend.

Mỗi side effect có `Writer.Do`: check local lease/request deadline và fence,
dispatch đồng bộ; local closure cùng mutex với effect. Không giữ lock này qua
GET. Registry hiện serialize các writer effects, kể cả I/O, nên đây không phải
thiết kế throughput cho nhiều node. Adapter kẹt có thể chặn heartbeat/closure;
không dùng timeout để tự nhả protection. Low-level adapter vẫn yêu cầu caller
quản lý lifetime. Luồng khuyến nghị là `write.BeginWriteSession`, quản lý
registration/heartbeat/work token xuyên prepare → publish → quiescence.
Source: `internal/engine/write/session.go`.

Session lấy quyền write từ `MetadataStore.Authorize`, bind exact object/bucket
incarnation, credential, OpID và WriterFence; không nhận boolean quyền từ caller.
Backend tiếp tục reauthorize trong publication transaction. Test
WriteSessionReauthorizesAtPublication revoke grant sau prepare và xác nhận không
có head/outbox được publish. R1 fence không thay history/epoch của R2 event.

Một registration chỉ có một session thực thi. Duplicate session bị reject mà
không đóng owner đang chạy. Copied session/work token dùng chung private state.
Prepare dùng protected ports của session, giữ prepared descriptor riêng và trả
owned descriptor; caller sửa PageIndex không làm đổi head được publish.

Close cancel và đóng local fence trước, chờ method/body/CPU/I/O trả về, join heartbeat, Finish work
rồi mới release. Test dùng body reader cố tình bỏ qua cancellation và Close qua
bản sao handle: pin/guard vẫn giữ tới khi reader được cho phép trả về. Reconcile
sau Close dùng context mới, đọc retained original outcome và không retry publish.

Writer admission giữ resolving guard đến release và pins cho các generations
reuse. Đây là interlock bảo thủ một process; không đủ làm MARK/sweep eligibility.

## Outcome và failpoints

Hooks Before/After được inject, chạy ngoài mutex; không có global failpoint state.
Tên stage gồm register, register_pack, pack, sidecar, warm, insert/location,
manifest, reserve và commit/op_id. Tests pause trước pack/sidecar/manifest và
trước dispatch/sau publication. Không cần wall-clock sleeps.

| Tình huống | Kết quả an toàn |
|---|---|
| Fence đóng trước callback dispatch | Callback không chạy; ErrFenced |
| Call dispatch hợp lệ trước local lease expiry | Được settle dưới local serialization; outcome không bị xóa bởi expiry |
| Registration lost ACK | Guard còn; retry cùng fence/deadline lấy cùng registration, không nhân đôi |
| ErrUnavailable/cancellation/I/O error sau dispatch | Giữ unknown; không tự retry/rebase/rollback/release |
| Effect xong nhưng After hook chưa trả | Release trả ErrWorkPending, còn protection |
| After hook làm mất ACK | ErrUnavailable, giữ original operation và pins |
| GetCommitOutcome có committed result cùng OpID | Reconcile original result; có thể release sau quiescence |
| GetCommitOutcome trả ErrNotFound | Vẫn unknown, không trở thành no-commit proof |
| Unknown prepare/S3 outcome | Giữ guard/pins; chưa có API tùy tiện clear unknown |
| Adapter hoặc After hook panic | Panic vẫn propagate; mutex được nhả, giữ unknown/guard; không giả rollback |

Unknown của prepare/S3 hoặc proved-not-committed chưa có recovery adapter trong
skeleton này. Fail closed có thể giữ registration lâu; W04 phải cung cấp durable
reconciliation, retention và bounded terminal-history cleanup. Không có TTL tự
động biến unknown thành safe cleanup.

## LP và giới hạn Q6

- Acquire/Pin/Retire/read work/release: mutex WarmGate, **local LP**.
- Bind của WarmGate kiểm observed head nhưng không đồng transaction với Badger
  head CAS. Immutable pages còn được giữ và destructive lifecycle bị chặn; không
  tuyên bố đây là atomic root binding distributed Q2.
- Register/dispatch/local fence closure: mutex WriterRegistry. Callback backend
  vẫn có LP riêng; wrapper không thêm shared transaction condition.
- Badger/fake hiện kiểm wall expiry trong mutation transaction. Skeleton giữ
  predicate đó; không sửa storage để giả lập proof Q6. Test callback settle sau E
  chỉ chứng minh local model; không chứng minh backend commit-after-E support.
- Chưa có durable shared closure/incarnation/CAS, UTC lease-bound attestation,
  grace takeover hoặc no-overlapping-authority proof giữa processes. Thuộc
  W04-PHONG-03 phối hợp adapter của Hải.

Không dùng recovery epoch làm execution authority. Không thay EvaluateRead,
ValidateUTCSample hay protection/restore semantics. Warm-only path không cần
thêm một evaluator hoặc tự tạo trusted UTC proof.

## Read/write races qua adapters thực

`TestWarmR1ReadOverlapsManagedOverwriteAndDelete` chạy hai timelines có barrier:

1. Reader authorize/bind/pin H1, emit verified prefix rồi tạm dừng trong emitter.
2. Managed writer dùng cùng Badger, S3 adapter và WarmGate: prepare + publish H2
   hoặc publish tombstone revision 2; Close không được nhả pin riêng của reader.
3. Request mới thấy H2 hoặc ErrNotFound kể cả khi cache đã chứa bytes H1.
4. Reader cũ tiếp tục đúng body H1; case DELETE dùng logical Range xuyên chunks.
5. Chỉ sau reader quiesce mới có thể retire local generation H1. Physical delete
   vẫn bị chặn. Resource admission và heartbeat timers đều được cleanup.

Chỉ SDK service của S3 được mô phỏng bằng harness hiện có; writer, immutable pack,
checksum/decode, canonical metadata, LRU và WarmReader là implementation thực.
Không dùng `handoffGuard` boolean của test handoff cũ cho evidence R1 mới.

Two-worker writer tests chuẩn bị hai operations trên cùng head revision, ép cả
hai thứ tự CAS, xác nhận winner/loser và không có loser outbox/hidden rebase.
Một timeline khác dừng worker A trước dispatch, đóng fence A, để B commit rồi
thả A: A bị ErrFenced. Các tests này là local integration evidence, không là
distributed fencing/transaction proof.

## Destructive lifecycle gate

`ProtectPackStore` chặn DeletePack/ChangeStorageClass vô điều kiện. App dev
Bootstrap và writer integration trả protected port. Put/Head/RangeGet/Restore
và sidecar verification vẫn giữ contracts tương ứng. Restore acceptance không
được coi là readable; Warm reader tiếp tục reject Cold/Archive.

Runtime phải giữ raw adapter bên trong wiring và chỉ expose protected port tới
consumer. Caller cố tình giữ raw backend có thể bypass wrapper; đây không là IAM
hoặc distributed barrier. Local Retire thành công cũng không mở quyền delete.
Chưa triển khai GC/tiering jobs, compaction hoặc physical deletion coordinator.

## Validation và review gates

```text
go test -count=20 ./internal/coordination/... ./internal/engine/write/... ./internal/readpath/... ./internal/app/...
go test ./...
go vet ./...
go test -race ./internal/coordination/... ./internal/engine/write/... ./internal/readpath/... ./internal/app/...
git diff --check
```

Kiểm gofmt và whitespace cả files untracked. Race chỉ PASS nếu thật sự chạy;
CI được mở rộng từ `./internal/engine` thành `./internal/engine/...` để bao phủ
writer adapter. Không coi tests Fake/Badger hoặc local mutex là distributed proof.

Kết quả local ngày 28/10/2026 trên Go 1.27.2:

| Kiểm tra | Kết quả |
|---|---|
| Các packages coordination, engine/write, readpath, app — `-count=20` | PASS |
| `go test ./...` | PASS |
| `go vet ./...` | PASS |
| gofmt, whitespace tất cả file thay đổi, Markdown local links | PASS |
| `git diff --check` | PASS |
| Race detector local | NOT VERIFIED — GCC mingw32: `64-bit mode not compiled in` |

Tests Badger/localhost chạy ngoài sandbox Windows vì sandbox chặn rename
MANIFEST và loopback. Không chạy AWS live hoặc remote CI cho thay đổi này.

Các handoff cần Hưng/Hải và consumers review:

1. API skeleton và scope W03 so với durable coordinator W04.
2. Source authoritative WriterFence; bootstrap fixture không là runtime lease API.
3. Shared closure + metadata LP, Q2 root binding, exact revision pin transaction.
4. Unknown/no-commit reconciliation và giới hạn registry/history retained trong RAM.
5. Runtime factory chọn NewR1Gate, giữ KeepAlive và expose protected PackStore.
6. Consumer HTTP abort sau verified prefix, không phát error body thứ hai.
