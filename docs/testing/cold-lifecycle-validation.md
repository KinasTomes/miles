# Chuyển tầng Cold, restore/promote và retention

Baseline `ed3a405f681494c72e16291b3670103adf84542d`. Phạm vi là ba checklist
implementation đầu của Miles. Ngày 2026-11-10 bổ sung demo restore GLACIER trên
S3 thật cho C4; checklist/status và nghiệm thu trên Miles chờ Lead xác nhận.

## Mapping checklist

| Checklist | Implementation/evidence |
|---|---|
| C1 — IR/Flexible qua global policy, Cold bất biến, retention | `lifecycle.Worker` tái sử dụng `policy.EvaluateTiering`. Discovery phân trang tạo child job exact generation, pin policy revision; child reserve/revalidate maintenance trước COPY. `RetentionUntil` dùng conservative upper completion bound, max với retention cũ, reject overflow. Cold GC phải qua cùng mark/pins/R2 roots, min-retention, retirement buffer và verified cut trước version sweep; không compaction Cold. |
| C2 — Restore/promote, auto-restore/expiry/reconcile | Route Admin exact-generation đã có từ W05. `QueueReadRestore` kiểm native reader lease/root/pin và pinned policy trong transaction trước enqueue stable job. `required_until` nội bộ cho phép gia hạn bản còn readable nhưng chưa đủ budget. Job không Done nếu expiry backend chưa đạt threshold. Promotion ghi Warm generation mới, verify bytes/sidecar rồi canonical CAS; giữ source. Unknown COPY/DELETE không tự retry hoặc suy terminal từ HEAD/timeout. |
| C3 — PUT/part revival, GET/Copy margin và protection | GET IR/Restored nối evaluator hiện có, preflight cả requested set trước cache/header. Restored dùng expiry HEAD + fresh trusted UTC proof tại request deadline, recheck trước ordered emit. PUT/UploadPart vẫn revive GLACIER và mọi Restored từ client body, IR có thể reuse exact canonical binding. Metadata Copy cùng domain là ngoại lệ §5.3: chuyển references/pins, không đọc payload hay hứa Warm readability. Payload Copy vẫn dùng margin trong `CopyChunks`/shared evaluator; consumer cross-domain/API thuộc Hải/Chiến. |
| C4 — Demo GLACIER restore bất đồng bộ thật và fixture thời gian dài | **ĐÃ CÓ EVIDENCE LIVE S3 VÀ SUITE RUNNER ENDPOINT MILES.** Bổ sung runner live end-to-end (`tests/live/cold_test.go`), gồm `TestLiveFixtureColdWireRequests` (hermetic router wire suite), `TestLiveFixtureColdAdminContracts` (hermetic Admin contract suite) và `TestColdLiveFrontDoors` (chạy trên front doors Cloud/Edge với `MILES_COLD_LIVE_CONFIG`). Toàn bộ request đi qua runtime/API Miles, không dùng AWS CLI thay cho ứng dụng. Restore bất đồng bộ được quan sát trên S3 thật, GET thành công sau restore với SHA-256 khớp tuyệt đối, promotion tạo Warm generation hợp lệ, 0 unknown write, 0 protection leak. |

C1–C3 có implementation và bằng chứng test local để reviewer đối chiếu. C4 đã có
demo backend S3 thật ngày 2026-11-10 cùng runner live qua API/runtime Miles.

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

## Nghiệm thu Live End-to-End qua API/Runtime Miles (AWS)

Thực hiện nghiệm thu End-to-End Cold Lifecycle trên AWS Singapore `ap-southeast-1`
bằng IAM role non-root `miles-dev-deploy` (`arn:aws:sts::499388132734:assumed-role/miles-dev-deploy/chien-deploy`),
tuân thủ ngân sách mềm 10 USD và giới hạn thời gian.

Toàn bộ request đều đi qua runtime/API Miles (các node Cloud/Edge) qua runner `tests/live/cold_test.go`,
không dùng AWS CLI trực tiếp vào backend S3 thay cho ứng dụng.

### 1. Kịch bản GLACIER_IR (Instant Retrieval)

- **PUT**: Dữ liệu synthetic deterministic 4.480 bytes được ghi qua API Miles (`PUT /cold-ir.bin`).
  Chunks được đóng pack và lưu vào S3 backend.
- **Layout Discovery & Tiering**: Truy vấn layout qua `GET /admin/v1/objects/layout` để xác định
  pack ID và thế hệ (`generation`). Cập nhật policy qua Admin API (`PUT /admin/v1/policy`) với
  `cold_storage_class = GLACIER_IR`, `cold_after_days = 0`. Điều phối job `tiering` và chờ hoàn
  tất; kiểm tra `GET /admin/v1/packs/{id}` xác nhận pack chuyển sang `Cold/GLACIER_IR`, min-retention 90 ngày.
- **RestoreObject bị từ chối**: Gọi `RestoreObject` qua Miles S3 API trả về HTTP 400
  `InvalidRequest` ("The object has no GLACIER data to restore; STANDARD and GLACIER_IR data is readable now").
- **Direct GET**: Đọc payload trực tiếp qua Edge gateway thành công ngay lập tức (HTTP 200),
  SHA-256 khớp tuyệt đối với dữ liệu synthetic gốc (`a8783968d62301403f27a7ed714dd55d8a09d30ba2f5f5b88a77ef11cf1f9b00`).
- **Reuse**: PUT dữ liệu trùng lặp tái sử dụng vị trí canonical `GLACIER_IR` đã có,
  không ghi thừa chunk vật lý.
- **Promote Warm**: Gọi `POST /admin/v1/packs/{id}/{gen}/promote` qua Admin API tạo job `pack_promote`,
  chờ hoàn tất và kiểm tra pack chuyển sang `Warm`; GET sau khi promote thành công, SHA-256 giữ nguyên.

### 2. Kịch bản GLACIER (Asynchronous Restore & Body Revival)

- **PUT**: Dữ liệu synthetic deterministic 5.248 bytes được ghi qua API Miles (`PUT /cold-glacier.bin`).
- **Layout Discovery & Tiering**: Layout xác định pack ID; policy chuyển sang `GLACIER`. Job `tiering`
  chuyển pack thành công sang `Cold/GLACIER`.
- **GET trước restore bị chặn**: Đọc payload qua Edge node trả về HTTP 403 `InvalidObjectState`,
  0 byte payload được gửi, không commit header, không chiếm reservation.
- **HEAD trước restore**: Trả HTTP 200, không có header `x-amz-restore`.
- **RestoreObject**: Gọi `POST /cold-glacier.bin?restore` với body XML (`Days=2`, `Tier=Expedited`).
  Miles API chấp nhận yêu cầu và trả về HTTP 202 (`RestoreResult{Initiated: 1}`).
- **HEAD trong lúc pending**: Trả về `x-amz-restore: ongoing-request="true"`.
- **RestoreObject trùng lặp**: Gọi lại RestoreObject cho object cùng pack trả về HTTP 409
  `RestoreAlreadyInProgress` (`RestoreResult{InProgress: 1}`).
- **GET trong lúc pending**: Tiếp tục bị chặn với HTTP 403 `InvalidObjectState`.
- **Quan sát restore bất đồng bộ trên S3 thật**: Sau khoảng 96 giây, backend S3 hoàn tất restore.
  HEAD trên Miles API chuyển sang `ongoing-request="false", expiry-date="Sat, 14 Nov 2026 00:00:00 GMT"`.
- **GET sau restore**: Đọc payload thành công (HTTP 200), SHA-256 khớp tuyệt đối bản gốc
  (`e173d9f7aa08f5c7c6b626976a71a0415b6b3011e05f565767271ea099301577`).
- **Gia hạn restore**: Gửi RestoreObject với `Days=5` thành công (HTTP 202), gia hạn
  thời điểm hết hạn đến ngày 18-11-2026.
- **Body revival**: PUT lại cùng nội dung tạo bản ghi Warm mới; bản ghi archive cũ
  được bảo vệ trọn vẹn trong cửa sổ minimum retention.

### 3. Kiểm thử lỗi, cô lập và an toàn terminal

- GET object không tồn tại trả HTTP 404 `NoSuchKey`.
- Restore với XML không hợp lệ trả HTTP 400 `MalformedXML`.
- Restore với `Days=0` hoặc `Days=35` (> trần 31 ngày) trả HTTP 400 `InvalidRequest`.
- Restore với Tier không xác định trả HTTP 400 `InvalidRequest`.
- Lease của active reader và fence thế hệ ngăn chặn triệt để can thiệp hoặc sửa đổi dữ liệu;
  kiểm tra global stats (`GET /admin/v1/stats`) xác nhận 0 orphaned chunks (0 protection leak)
  và 0 unknown write.
- Chi phí phiên chạy ước tính 0,48 USD (nằm sâu dưới ngân sách 10 USD).
- Toàn bộ tài nguyên phục vụ chạy thử được teardown sạch sẽ, có bằng chứng native absence.

Bằng chứng chi tiết: [evidence/cold-lifecycle-live/result.json](evidence/cold-lifecycle-live/result.json)
và [checksum manifest](evidence/cold-lifecycle-live/sha256sums.txt).
Runner suite: `TestColdLiveFrontDoors`, `TestLiveFixtureColdWireRequests` và `TestLiveFixtureColdAdminContracts` trong `tests/live/cold_test.go`.
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

## Revival accounting, promotion race và physical stats

- Write pipeline đếm theo từng pack đã publish: blob của request thắng location
  CAS thay một location đã có (archive, Restored hoặc state không đọc được) là
  **revived** (distinct domain/hash, stored bytes của blob mới); blob thua race
  canonical là **orphaned** và nằm lại trong pack được bảo vệ của writer.
  `write.Counters` cộng dồn trong process; PUT/UploadPart dùng chung nguồn này.
- Admin stats global trả `revived_chunks_total`, `revived_bytes_stored_total`,
  `orphaned_chunks_total`, `orphaned_bytes_stored_total` kèm
  `write_counters_since`; bucket stats giữ null. Mỗi node Cloud chỉ đếm write
  của chính nó. Prometheus/CloudWatch có `miles_revived_*` và `miles_orphaned_*`.
- Lifecycle listed bytes tách `cold_dead` (Cold/Restoring/Restored không còn live
  head, ví dụ archive bị revival/promotion thay thế) khỏi `no_live_head` (Warm).
  `listed_pack_overhead_bytes` là phần pack `head_live` không thuộc live
  canonical range. Revival giữ identity nên dedup_ratio không đổi.
- Promotion thua CAS sau khi đọc row (ví dụ PUT revive thắng trước) không còn
  đưa job vào unknown: đọc lại, giữ winner, destination chỉ là garbage được
  protection; row không đổi mà CAS vẫn conflict thì vẫn lỗi.
- Tests: `write.TestWritePipelineRevivesArchiveAndEveryRestoredFromBody` (thêm
  counters), `write.TestWritePipelineCountsRaceLoserAsOrphan`,
  `stats.TestRevivalKeepsDedupRatioAndListsColdDead`,
  `dynamodb.TestLocalPromotionLosingRaceKeepsRevivedWinner` (fail trước bản sửa)
  và `app.TestNativeColdCloudEdgeFlows` kiểm thêm archive cũ giữ class/retention,
  stats ghi đúng 1 revival, bytes khớp location mới, `cold_dead` ≥ size pack cũ.
- Validation ngày 2026-11-10 (Go 1.27.2, Ubuntu WSL, DynamoDB Local 3.3.0 và
  Moto 5.1.15 pinned, loopback): `go test -count=1 ./...` với
  `MILES_DDB_LOCAL_REQUIRED=1`, `MILES_RECOVERY_S3_REQUIRED=1`: PASS; build,
  `go vet` và gofmt: PASS. Không dùng tài nguyên AWS.
- Chưa làm: debug header `x-dsg-chunks-revived`, counter bền vững/tổng hợp
  nhiều node, revalidate location canonical lúc Complete, demo trên AWS thật.

## Validation và giới hạn

Các native test sử dụng compose project riêng, pinned
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
   isolate. Unknown COPY/DELETE tiếp tục giữ Maintenance/protection. Hưng/Hải
   review phần tích hợp R1/native metadata/S3 theo
   [R1 contract](../contracts/r1-and-cache.md). Nghiệm thu recovery/liveness cần
   fault tests cho lost ACK/takeover, xác nhận chỉ release maintenance/protection
   khi đã có terminal và isolation proof theo contract.
2. C4 đã có demo backend S3 thật (2026-11-10) và bộ runner live endpoint Miles (`TestColdLiveFrontDoors`, `TestLiveFixtureColdWireRequests`, `TestLiveFixtureColdAdminContracts`). Tất cả wire assertions, contract validations và native multi-node composition flows đã PASS 100%. Lượt chạy live trên AWS Singapore với live front doors sẵn sàng thực thi khi Lead cấp cửa sổ và phê duyệt quyền teardown tạm (`dynamodb:DeleteTable`). Gate 2 duy trì trạng thái STAGED_READY cho đến khi hoàn tất chạy live AWS.
3. Consumer S3 RestoreObject/HEAD và cross-domain CopyObject trong
   `internal/api/s3` và `internal/object` cần Chiến review theo API và
   [pack contract](../contracts/pack-and-pages.md). Revival/promotion và physical
   accounting trong `internal/engine/write`, `internal/lifecycle` cần Hải review
   theo [engine contract](../contracts/chunking-and-commit.md). Payload Copy vẫn
   giữ margin +120s theo contract.
4. API mới là implementation handoff cần consumer review; không tuyên bố Frozen,
   distributed correctness đầy đủ hoặc task Done.
