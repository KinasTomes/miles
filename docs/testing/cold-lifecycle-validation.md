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
| C4 — Demo GLACIER restore bất đồng bộ thật và fixture thời gian dài | Demo backend S3 thật ngày 2026-11-10 (AWS CLI, không qua Miles). Runner live qua front door Miles (`tests/live/cold_test.go`) đã có; **nghiệm thu AWS end-to-end qua Miles Cloud + Edge CHƯA ĐẠT**, xem mục nghiệm thu live bên dưới. |

C1–C3 có implementation và bằng chứng test local để reviewer đối chiếu. C4 có
demo backend S3 thật ngày 2026-11-10; nghiệm thu qua API/runtime Miles trên AWS
đang chờ một lượt chạy được duyệt trên installation riêng.

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

## Nghiệm thu live end-to-end qua API/runtime Miles (AWS)

**Trạng thái: CHƯA ĐẠT (NOT RUN).** Lượt chạy ngày 2026-11-13 bị hủy làm bằng
chứng: runtime bật cờ bỏ qua `ProtectedUntil`, script reset R1 gate và xóa job
chưa terminal trực tiếp trong DynamoDB, Cloud là process local thay vì đường
ALB/EC2, và Edge chạy bằng credential của deploy role. Artifact của lượt đó đã
bị gỡ; code bypass đã được xóa, fencing R1/lifecycle trở lại hợp đồng gốc.

Lần deploy ngày 2026-11-14 (installation riêng, runtime release cùng code
production với nhánh này) dừng trước khi chạy test: ALB và Edge EC2 được tạo,
nhưng Cloud ASG bị từ chối vì deploy role thiếu `ec2:RunInstances` trên
`launch-template` của installation. Edge không lên được: credential helper của
Roles Anywhere báo `could not parse PEM data`, process Miles chưa từng start (không
request, không lease). Session đã pause bằng saved plan; datastore, state và IAM
được giữ nguyên. Backend còn tồn đọng từ lượt 2026-11-13: R1 gate
`active=2/resolving=2`, lease và lifecycle action không có job (do script cũ
reset gate và xóa job). Các tồn đọng này cần xử lý qua đường reconcile được
review, không sửa trực tiếp.

### Runner

`TestColdLiveFrontDoors` chỉ chạy khi có `MILES_COLD_LIVE_CONFIG` và
`MILES_RUN_COMMIT` (SHA đầy đủ của commit sạch đang test); thiếu một trong hai là
SKIP/FAIL, không phải PASS. Trước mọi request front door:

- Đọc inventory private (`installation_file` trỏ tới `installation.json` theo
  [hướng dẫn installation](aws-personal-account-guide.md)). Không có installation
  mặc định; tên bảng `<installation>-<Logical>` và bucket
  `<installation>-data-<account>-<region>` suy ra đúng như Terraform.
- `ValidateApprovedInventoryAndSchema` (fail-closed): STS phải là assumed-role
  session của `operator.deploy_role_arn` đúng account (root, IAM user, role dùng
  chung hoặc runtime role đều dừng); bucket versioning `Enabled` với
  `ExpectedBucketOwner`; 20 bảng `ACTIVE`, đúng tên/ARN/key schema/kiểu và đủ tag
  `Project`, `Installation`, `Environment`, `Owner` theo inventory.
- Admin chỉ nhận địa chỉ IP loopback literal (không nhận hostname).

Mỗi endpoint: ghi `cluster/nodes` làm topology thực tế, PUT payload IR và
GLACIER, bật tiering cho class tương ứng rồi tạo job `tiering` exact generation
pin revision đó, sau đó tắt tiering global ở revision mới. Job giữ revision đã
pin; periodic discovery (vẫn bật) chỉ thấy policy tắt nên không tạo job cạnh
tranh. Trong cửa sổ `ProtectedUntil`, job phải được worker claim, chưa terminal
và generation vẫn `Warm` (protection hoãn chuyển tầng). Runner chờ tới hết
`ProtectedUntil` thật (cộng 1 phút cho UTC bound, khoảng 45–50 phút, dùng chung
cho mọi endpoint), rồi chính các job đó phải `succeeded` với generation `Cold`,
đúng class và min-retention ≥ 89 ngày. Sau đó:

- GLACIER_IR: RestoreObject 400, GET trực tiếp khớp SHA-256, reuse, promote exact
  generation; layout canonical chuyển sang generation mới `Warm`, source giữ
  `Cold` cùng retention.
- GLACIER: GET/HEAD trước restore, RestoreObject 202 rồi 409, HEAD pending, GET
  403 khi pending, chờ restore S3 thật, GET khớp SHA-256, metadata `Restored`
  với `restored_until`; gia hạn 202 phải đưa generation về `Restored` với expiry
  muộn hơn (không để job restore dở); body revival phải bind chunk sang pack
  `Warm` mới trong layout, archive giữ class và retention.
- Stats của node Admin: `physical_complete` true, `orphaned_chunks_total` = 0,
  `cold_dead` > 0. Write counter chỉ đếm trên từng node (ALB có thể route PUT sang
  Cloud node khác), nên revival được kiểm qua layout chứ không qua counter. Các
  số liệu này bổ sung, không thay thế trạng thái job và bản ghi generation.

Result file (mới, 0600) ghi commit, installation, tên deploy role, thời điểm
bắt đầu/kết thúc, mọi observation S3 và Admin event (job, pack generation,
protection/retention, restore, stats).

### Chạy

Cloud (ASG sau ALB) và Edge (EC2) đều deploy bằng `deploy-installation.py`.
`scripts/run_live_acceptance.py --config installation.json --private DIR --result FILE`
từ chối chạy khi worktree bẩn hoặc result file đã tồn tại. Script lấy node từ
inventory của installation, mở tunnel Admin SSH-over-SSM (host key pin qua SSM)
tới một Cloud node và Edge node trên `127.0.0.1`, sinh live config private
(Cloud qua DNS ALB, Edge qua IP public, CA test của installation), chỉ đưa
credential deploy role cho process test. Script không sửa R1, không xóa job,
không dừng process nào ngoài tunnel của nó và không ghi vào repository.

Điều kiện trước khi chạy: Lead duyệt run window/ngân sách và Terraform plan;
workload Cloud/Edge của installation riêng đã deploy. Sau khi chạy: drain, dọn
tài nguyên do lượt chạy tạo, chụp inventory native baseline/final, tách riêng tài
nguyên được giữ lại (archive còn min-retention, state). Raw result và inventory
giữ private; chỉ commit bản tóm tắt đã sanitize kèm checksum.

Suite: `TestColdLiveFrontDoors`, `TestLiveFixtureColdWireRequests`,
`TestLiveFixtureColdAdminContracts` (`tests/live/cold_test.go`);
`TestPreflight*`, `TestLivePreflightApprovedInventoryAndSchema` (opt-in qua
`MILES_INSTALLATION_JSON`, `internal/metadata/dynamodb/preflight_test.go`).

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
2. C4 đã có demo backend S3 thật, pending/completed và checksum, kèm fixture thời
   gian dài. Nghiệm thu qua endpoint Miles trên AWS chưa đạt; runner và điều kiện
   chạy ở mục nghiệm thu live. Không đo billing hoặc retention thực 90 ngày và
   không coi Moto completion là bằng chứng live AWS.
3. Consumer S3 RestoreObject/HEAD và cross-domain CopyObject trong
   `internal/api/s3` và `internal/object` cần Chiến review theo API và
   [pack contract](../contracts/pack-and-pages.md). Revival/promotion và physical
   accounting trong `internal/engine/write`, `internal/lifecycle` cần Hải review
   theo [engine contract](../contracts/chunking-and-commit.md). Payload Copy vẫn
   giữ margin +120s theo contract.
4. API mới là implementation handoff cần consumer review; không tuyên bố Frozen,
   distributed correctness đầy đủ hoặc task Done.
