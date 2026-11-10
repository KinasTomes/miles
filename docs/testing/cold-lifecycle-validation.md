# W06-PHONG-01 — Tiering, restore/promote và Cold retention

Baseline `ed3a405f681494c72e16291b3670103adf84542d`, branch
`codex/W06-PHONG-01-tiering-restore`. Phạm vi lượt này là ba checklist
implementation đầu của Miles. Ngày 2026-11-10 bổ sung demo restore GLACIER trên
S3 thật cho C4; checklist/status và nghiệm thu trên Miles chờ Lead xác nhận.

## Mapping checklist

| Checklist | Implementation/evidence |
|---|---|
| C1 — IR/Flexible qua global policy, Cold bất biến, retention | `lifecycle.Worker` tái sử dụng `policy.EvaluateTiering`. Discovery phân trang tạo child job exact generation, pin policy revision; child reserve/revalidate maintenance trước COPY. `RetentionUntil` dùng conservative upper completion bound, max với retention cũ, reject overflow. Cold GC phải qua cùng mark/pins/R2 roots, min-retention, retirement buffer và verified cut trước version sweep; không compaction Cold. |
| C2 — Restore/promote, auto-restore/expiry/reconcile | Route Admin exact-generation đã có từ W05. `QueueReadRestore` kiểm native reader lease/root/pin và pinned policy trong transaction trước enqueue stable job. `required_until` nội bộ cho phép gia hạn bản còn readable nhưng chưa đủ budget. Job không Done nếu expiry backend chưa đạt threshold. Promotion ghi Warm generation mới, verify bytes/sidecar rồi canonical CAS; giữ source. Unknown COPY/DELETE không tự retry hoặc suy terminal từ HEAD/timeout. |
| C3 — PUT/part revival, GET/Copy margin và protection | GET IR/Restored nối evaluator hiện có, preflight cả requested set trước cache/header. Restored dùng expiry HEAD + fresh trusted UTC proof tại request deadline, recheck trước ordered emit. PUT/UploadPart vẫn revive GLACIER và mọi Restored từ client body, IR có thể reuse exact canonical binding. Metadata Copy cùng domain là ngoại lệ §5.3: chuyển references/pins, không đọc payload hay hứa Warm readability. Payload Copy vẫn dùng margin trong `CopyChunks`/shared evaluator; consumer cross-domain/API thuộc Hải/Chiến. |
| C4 — Demo GLACIER restore bất đồng bộ thật và fixture thời gian dài | **ĐÃ CÓ EVIDENCE LIVE S3, CHỜ REVIEW.** Ngày 2026-11-10, profile AssumeRole có MFA chạy tại Sydney: upload GLACIER, GET trước/trong pending bị từ chối, HEAD chuyển từ pending sang completed có expiry, download 54 bytes với SHA-256 và nội dung khớp. Fixture Virtual Clock/Moto đã PASS. Demo live này dùng AWS CLI với object synthetic; chưa chạy endpoint Miles trên AWS. |

C1–C3 có implementation và bằng chứng test local để reviewer đối chiếu. C4 đã có
demo backend S3 thật và fixture thời gian dài. Lead/reviewer xác nhận phạm vi
nghiệm thu, gồm việc có yêu cầu thêm demo qua endpoint Miles trên AWS hay không.

## Demo GLACIER thật ngày 2026-11-10

Profile `phong-deploy` dùng `aws login` của chính operator. Profile `phong-miles`
AssumeRole vào `miles-dev-deploy` bằng thiết bị MFA đã đăng ký. Kiểm tra quyền
upload/download một file STANDARD trong prefix test của bucket do admin cấp:
PASS, nội dung và SHA-256 khớp. MFA code và credential không nằm trong evidence.

Sau preflight, upload một object synthetic GLACIER riêng ở `ap-southeast-2`,
SSE-S3. Restore dùng `Expedited`, `Days=1`. Các mốc UTC dưới đây là thời điểm
operator quan sát response; không phải timestamp linearization của backend.

| Mốc UTC | Bằng chứng |
|---|---|
| 04:31:18 | HEAD có `StorageClass=GLACIER`, 54 bytes; chưa có restore header |
| 04:31:21 | GET trước restore trả `InvalidObjectState` |
| 04:31:24 | Restore request được AWS chấp nhận |
| 04:31:27 | HEAD có `ongoing-request="true"` |
| 04:31:30 | GET trong lúc pending tiếp tục trả `InvalidObjectState` |
| 04:32:19 | HEAD vẫn pending |
| 04:32:52 | HEAD có `ongoing-request="false"`, expiry `Thu, 12 Nov 2026 00:00:00 GMT` |
| 04:32:55 | GET thành công, 54 bytes; nội dung và SHA-256 khớp bản gốc |

SHA-256 fixture và bản tải lại đều là
`7efe6fc643ee93ae0643c579db0b1c8136897a81117f79ba48666f0ae4d2fc17`.
ETag và storage class GLACIER giữ nguyên qua các response. Upload dùng
`If-None-Match: *`, download dùng `If-Match` theo ETag đã ghi nhận. Response
không có VersionId; đây là binding theo unique key/ETag, không phải bằng chứng
immutable object version hoặc production PackRef generation.

Evidence đã loại identity/session/account/bucket/key khỏi phần public và giữ
fingerprint target để đối chiếu với bản ghi riêng tư. Payload fixture chỉ có dữ
liệu synthetic. [Kết quả và timeline](evidence/cold-glacier-live/result.json),
[checksum manifest](evidence/cold-glacier-live/sha256sums.txt).

Theo lựa chọn của Phong, giữ hai object synthetic để reviewer đối chiếu. Đây là
demo backend S3 thật bằng AWS CLI. Live endpoint Miles, pack verifier trên AWS,
expiry thực sau một ngày và retention thực 90 ngày chưa được chạy trong demo này.
Các boundary margin/expiry/protection/retention tiếp tục có evidence local riêng.

### Lần thử Free Tier trước đó

Lần thử ngày 2026-11-07 ở selected Region `ap-southeast-2` đã xác minh đúng
project đích, `Free Tier / Active`, tạo bucket test riêng và xác nhận Block Public
Access. AWS từ chối upload file synthetic với `AccessDenied`, nêu rõ explicit
deny trong SCP. Bucket được kiểm tra sau lỗi và có 0 object. Do upload thất bại,
chuỗi restore chưa bắt đầu.

Quy tắc `DenyGlacierStorageClasses` trong SCP Free Tier được AWS công bố chặn
`s3:PutObject` với `GLACIER` và `DEEP_ARCHIVE`. Nguồn đối chiếu là
[managed policies của AWS](https://docs.aws.amazon.com/accounts/latest/reference/scps-and-rcps-for-projects.html).

Việc có thêm credit không thay đổi SCP Free Tier. Lần thử bị chặn chỉ là bằng
chứng blocker của môi trường trước đó. Demo thành công ngày 2026-11-10 dùng
project/bucket đã được admin cấp quyền riêng.

## Authority và ownership

- Không thay `MetadataStore`, `PackInfo`, `CacheKey`, Clock semantics hoặc W1
  contracts; không thêm policy evaluator, global GET lock hay authority boolean.
- `WarmView.SharedFence` chỉ xuất identity cho native validation; fence value
  không tự là proof. Local-only view không dùng được đường enqueue shared.
- `QueueReadRestore` kiểm lease/root revision/exact pin trong cùng transaction
  với job; không impersonate system admin. Read permission đã pin tại admission;
  không reauthorize từng chunk. Không lookup current head để hủy old bound view
  chỉ vì overwrite/delete sau admission.
- GET thiếu restore luôn trả `ErrCold` trước headers/cache/payload. Enqueue không
  phải acceptance của S3 và không phải readiness. Một generation/revision chia sẻ
  stable job; unknown enqueue được đối chiếu lại cùng identity. Failed/unknown
  physical job không được GET tự clear/retry. Lỗi enqueue/time proof có diagnostic
  riêng, không thay archive error bằng một response success.
- `RequestUTC` chỉ bridge từ trusted `R1TimeSource` đã bao uncertainty qua Horizon;
  không dùng wall clock hoặc coi skew 2s là proof. Widen interval qua thời gian
  gọi adapter, bind lại monotonic Now, reject stale call/domain/horizon/overflow.
- Worker buffers, cache copies, HEAD transport và verified output giữ reservation
  đến emitter trả về; cancellation phải join workers trước release pins/budget.
  Recheck backend trước ordered emit kể cả khi speculative result đã hoàn tất.

## Discovery và Cold cleanup

- Shared runtime bật discovery mỗi phút qua cluster-owner periodic scheduler
  hiện có. Discovery chỉ đọc một trang 32 packs mỗi step; catalog/job inventories
  có giới hạn, vượt giới hạn trả lỗi, không truncate để cấp quyền chuyển tầng.
- Child IDs ổn định theo parent/generation. Replay và takeover không nhân đôi
  child; native creation fence chặn old worker enqueue sau closure. Discovery
  không có S3 side effect nên có đường reconcile riêng; exception này không áp
  dụng cho exact-pack tiering, restore, promotion hoặc GC đang unknown.
- Protection, active readers/writers/uploads và minimum retention ưu tiên age.
  `cold_after_days=0` chỉ bỏ age gate. Min-retention IR/Flexible là 90 ngày từ
  upper completion bound; không phải AWS billing attestation.
- Cold/Restored không được sửa nội dung hoặc compact. Khi không còn root, GC có
  thể retire canonical records và đóng generation; payload/sidecar vẫn giữ qua
  retirement window 24h+10m và mọi protected cut/anchor. Thời gian một mình không
  cấp quyền DELETE. Chỉ sweep exact immutable object versions đã được bind.

## Regression tests

- `readpath.TestColdReadIRAndArchivePreflight`: IR miss/hit, archive không đọc
  payload, không emit, accounting sạch.
- `TestRestoredReadBackendExpiryDeadlineAndUTC`: expiry boundary và thiếu UTC
  proof; S3 header chỉ có seconds, ns arithmetic được kiểm riêng ở policy suite.
- `TestFlexibleRestoreTimelineWithVirtualClock`: sau khi fixture xác nhận nhận
  yêu cầu, `HEAD` giữ `ongoing-request="true"` trong bốn giờ mô phỏng. Chỉ khi
  `HEAD` báo hoàn tất và cung cấp expiry thì GET mới được đọc payload và điền
  cache. Với request budget 1 phút, cache hit được chấp nhận tại equality của
  `deadline + 120s`, bị chặn sau 1 ns và tiếp tục bị chặn khi hết hạn. Mỗi lần
  bị chặn đều kiểm tra không commit headers, không emit, không chạm cache hay
  đọc payload và không giữ reservation. Bốn giờ là giá trị của fixture.
- `TestColdCacheNeverBypassesExpiryAndAutoRestore`: cache resident không bypass
  pending restore; auto_restore false/true, unique generation, pin còn sống,
  lỗi enqueue vẫn là archive error và có diagnostic.
- `TestRestoredReadRechecksBeforeOrderedEmit`: expiry/state mất hiệu lực sau
  prefix phải abort, không emit thêm chunk, reservation được thu hồi.
- `coordination.TestRequestUTCConservativeBridge`: pre-epoch UTC, source latency,
  age equality 5s và interval không thu hẹp; missing/domain/horizon reject.
- `lifecycle.TestMinimumRetentionIsIndependentOfPolicyAge`: IR/Flexible 90 ngày,
  Standard-IA 30 ngày, max-preserve, year-9999 overflow và unsupported class.
- Native DDB: `TestLocalReadRestoreAtomicAdmissionAndDedup`,
  `TestLocalRestoreExtensionWaitsForRequiredExpiry`,
  `TestLocalColdGCRetentionAndRoots`,
  `TestLocalRetiredColdSweepRequiresEveryProtection`,
  `TestLocalColdCompactionNeverReadsOrMutatesPayload`,
  `TestLocalTieringDiscoveryQueuesOnlyEligibleChildren`.
- `app.TestNativeColdCloudEdgeFlows`: native DynamoDB, production SDK/Moto,
  ba runtime Cloud/Cloud/Edge; explicit tiering theo policy, IR GET miss/hit,
  IR reuse, GLACIER Cold/Restored PUT và UploadPart→Complete→GET, metadata Copy
  Cold cùng domain và promotion sang generation Warm mới.
- Existing lifecycle tests vẫn kiểm lost ACK, one-shot send, exact terminal and
  isolation proof, state/fence closure, canceled restore và promotion retries.

## Validation và giới hạn

Các native test sử dụng compose project riêng `miles-w06-phong-contract`, pinned
DynamoDB Local/Moto từ `deploy/local/compose.yaml`, Go 1.27.2 Linux, source mount
read-only và endpoints loopback explicit. Không dùng credential hoặc tài nguyên
AWS. Test forwarding chỉ ở container để nối hai disposable services.

Kết quả validation local Linux ngày 2026-11-04:

- `go test -count=1 -timeout=10m ./...`: PASS; native DynamoDB và runtime
  DDB/Moto bắt buộc qua `MILES_DDB_LOCAL_REQUIRED=1` và
  `MILES_RECOVERY_S3_REQUIRED=1`. Các suite live AWS opt-in không được bật.
- `go vet ./...`: PASS.
- `go test -count=10 ./internal/readpath ./internal/coordination
  ./internal/lifecycle`: PASS.
- `go test -race -count=1 -timeout=8m` trên readpath, coordination, engine/write,
  lifecycle, metadata/dynamodb và app, lọc các regression Cold/Restored/UTC/
  retention/discovery/native Cloud–Edge/Copy: PASS. Đây không phải race suite
  toàn bộ repository.
- Sau khi tăng độ mạnh hai assertions (existing-but-unpinned generation và
  cache counters không đổi khi preflight reject), chạy lại hai regression đó
  với `-race`: PASS.
- gofmt và whitespace checks, gồm file mới chưa tracked: PASS.

Kết quả kiểm tra lại ngày 2026-11-07:

- `TestFlexibleRestoreTimelineWithVirtualClock`: PASS với `-count=20` và
  PASS với `-race -count=1` trên Go 1.27.2 trong Ubuntu WSL.
- `go test -count=1 ./...` và `go vet ./...`: PASS. Các test yêu cầu endpoint
  bên ngoài được kiểm riêng ở bước sau.
- `TestNativeColdCloudEdgeFlows` và nhóm regression DynamoDB về restore
  admission/extension, tiering, promotion, retention, GC và compaction: PASS
  với DynamoDB Local/Moto bắt buộc trong compose project riêng. Các container
  fixture đã được dọn sau khi chạy.
- Trước khi mở PR, tích hợp `origin/main` tại `6b3a654` và chạy lại full Go
  tests/vet, build, format check, kiểm tên tài liệu và 50 Python regression
  tests: PASS. Race tests toàn package readpath, coordination, lifecycle và
  engine/write: PASS. Native Cloud/Edge Cold flows cùng nhóm regression
  DynamoDB ở trên: PASS sau merge. Các live AWS suite opt-in vẫn chưa được bật.

Trong validation, regression takeover discovery đã phát hiện việc kiểm nhầm
old fence sau khi native lease slot chuyển epoch. Bản sửa dùng exact next-epoch
admission, vốn chỉ được `AcquireR1` cấp sau Released; test cũng xác nhận old
fence không enqueue child sau closure. Không nới proof cho mutable S3 operations.

HEAD preflight bổ sung trước ordered emit làm tăng số HEAD requests; đây là
trade-off hiện tại để completed speculative bytes không bypass lifecycle/expiry.
Chưa có benchmark overhead hoặc số đo chi phí AWS cho thay đổi này. Không claim
GitHub CI PASS khi chưa push/tạo PR và kiểm tra check runs.

### Gate còn lại

1. Runtime mặc định vẫn không có provider `SharedAuthority.LifecycleTerminal`
   chứng minh unknown mutable request đã terminal **và** worker/request đã bị
   isolate. Unknown COPY/DELETE tiếp tục giữ Maintenance/protection. Hưng/Hải cần
   review/cung cấp operational proof; không thêm force-complete hoặc boolean
   thay proof để che lỗ hổng liveness.
2. C4 đã có demo backend S3 thật, pending/completed và checksum, kèm fixture thời
   gian dài. Demo qua endpoint Miles trên AWS chưa chạy; Lead xác nhận có cần
   bổ sung bằng chứng này trước nghiệm thu. Không đo billing hoặc retention
   thực 90 ngày và không coi Moto completion là bằng chứng live AWS.
3. S3 RestoreObject/HEAD headers và cross-domain CopyObject HTTP consumer thuộc
   W06-CHIEN-01/02; revival/copy/accounting handoff phối hợp W06-HAI-01. Thư viện
   payload Copy không thay bằng metadata-copy exception và vẫn giữ +120s margin.
4. API mới là implementation handoff cần consumer review; không tuyên bố Frozen,
   distributed correctness đầy đủ hoặc task Done.
