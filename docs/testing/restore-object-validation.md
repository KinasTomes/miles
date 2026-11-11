# RestoreObject và GET/HEAD archive contracts

Ngày 10/11/2026, từ `origin/main` `1dcc352`, sau khi presigned/ALB compatibility,
lifecycle jobs nền và Cold tiering/restore/promote đã hoàn tất.
Acceptance: restore endpoint cùng read behavior pass thật/dev fixture cả IR và
Flexible; quyền read là điều kiện bắt buộc. Hợp đồng: [API §2.2.3, §4.2, §4.4](../dedup-storage-gateway-api.md#44-cold-và-restoreobject).

## Mapping checklist

| Checklist | Implementation/evidence |
|---|---|
| C1 — RestoreObject, restore headers/status, InvalidObjectState | `s3.Objects.RestoreObject` parse `RestoreRequest`, validate Days 1–30/Tier, map 202/409 `RestoreAlreadyInProgress`/200/400. `object.Service.RestoreObject` → `readpath.WarmReader.Restore` (cùng admission/view/pin như GET). HEAD trả `x-amz-restore` từ `WarmReader.ArchiveState`. GET archive chưa restore giữ 403 `InvalidObjectState` trước header. |
| C2 — IR đọc ngay; GLACIER chưa restore 403 + auto-restore theo policy; không giữ request | IR: GET đọc ngay, RestoreObject 400 không gọi backend restore. GLACIER: GET trả `ErrCold` ngay sau pre-flight, auto-restore và RestoreObject dùng chung một job `pack_restore` mỗi generation; request không chờ restore. |
| C3 — GET/Copy Restored deadline+120s, renewal, expired; không cần `aws --debug` | Restored chỉ đọc khi backend expiry ≥ hard deadline + 120 giây (đường đọc Cold có sẵn, giữ nguyên). Copy cùng domain chia pack với nguồn nên GET bản copy theo cùng biên. RestoreObject gia hạn bản Restored ngắn hơn Days và restore lại bản đã hết hạn. Header `x-amz-restore` là header S3 chuẩn, không cần debug. |

## Hành vi

- Quyền: Router kiểm read (`authorization.json` dòng RestoreObject); service kiểm lại
  read/owner tại admission như GET. Writer-only bị `AccessDenied` trước service.
- Pack GLACIER duy nhất của head được HEAD backend trước khi enqueue bất kỳ job
  nào: `Ref`, `Size`, `Class` phải khớp record đã pin và state (metadata lẫn
  backend) thuộc Cold/Restoring/Restored, lệch → `ErrConflict`, không job nào.
  Sau đó `Restoring` → in-progress; `Restored` với expiry ≥ upper UTC + Days →
  không làm gì; Cold, Restored ngắn hơn hoặc đã hết hạn → `QueueReadRestore` với
  Days/Tier của request.
- Days/Tier dùng chung `storage.RestoreRequest.Validate` ở HTTP, readpath và
  `lifecycle.Params`; readpath từ chối trước admission/HEAD/job hook.
- `required_until` của job = hard deadline + 120 giây như GET, nên job hoàn tất
  khi bản restore phủ được GET tại thời điểm request.
- Khác biệt S3 đã ghi ở API §4.4: Days > 30 bị từ chối; IR/Warm-only trả
  `InvalidRequest` 400; gia hạn bất đồng bộ trả 202; SELECT/OutputLocation 501.

## Regression tests

- `readpath.TestRestoreClassifiesArchivePacksWithoutPayload`: IR, GLACIER Cold
  (job mới/đã queue), backend restoring, Restored đủ Days, Restored ngắn hơn Days
  (renewal), Restored hết hạn; hook nhận đúng view/pin/revision/`required`
  (= deadline + 120 giây)/Days/Tier; không GET payload, admission sạch.
- `readpath.TestRestoreRejectsBeforeSideEffects`: Days 0/31, Tier rỗng/sai/sai
  hoa thường (không HEAD backend), thiếu UTC proof, runtime không có restore
  port, credential không có quyền: không enqueue.
- `readpath.TestRestoreRejectsPhysicalMismatch`: backend size +1, generation
  khác, class khác, state không phải archive → `ErrConflict`, không hook, không
  GET payload (review PR #78, fail trước khi sửa trừ case class).
- `readpath.TestArchiveStateReportsBackendRestore`: IR, chưa request, restoring,
  restored (expiry), expired.
- `object.TestRestoreHeaderAggregatesArchivePacks`: tổng hợp nhiều pack, không bịa
  `ongoing-request="true"`, expiry nhỏ nhất.
- `s3.TestRestoreObjectStatus`, `TestRestoreObjectValidatesBeforeService`,
  `TestRestoreObjectDefaultsAndPermission`, `TestHeadObjectRestoreHeader`.
- `dynamodb.TestLocalReadRestoreAtomicAdmissionAndDedup` (RestoreObject chia job
  với auto-restore, `created`), `TestLocalExplicitRestoreUsesRequestParameters`.
- `app.TestNativeColdCloudEdgeFlows` (DynamoDB Local + Moto, ba runtime
  Cloud/Cloud/Edge, lifecycle worker thật): IR → RestoreObject 400, HEAD không
  header, GET ngay; GLACIER → HEAD không header, RestoreObject 202, lần hai trên
  bản copy cùng pack 409, GET 403 không chờ, job xong → HEAD
  `ongoing-request="false"` cho cả nguồn và bản copy, GET cả hai, RestoreObject
  Days=1 → 200, Days=5 → 202 renewal, expiry HEAD đổi, GET tiếp tục đọc được.
- `cmd/miles.TestPresignedLocalProfiles`: AWS SDK Go v2 `RestoreObject` qua
  SigV4/Router thật ở local Cloud/Edge: body SDK parse đúng, Warm → 400.

Boundary equality/+1 ns/expired của GET Restored vẫn do
`TestFlexibleRestoreTimelineWithVirtualClock` và `TestRestoredReadBackendExpiryDeadlineAndUTC`
([cold lifecycle](cold-lifecycle-validation.md)) kiểm; Copy payload margin do `write.TestCopyRestoredExpiry`.

## Validation

Ngày 2026-11-10, Go 1.27.2 linux/amd64 trong WSL Ubuntu, compose project riêng
từ `deploy/local/compose.yaml` (DynamoDB Local/Moto pinned, loopback). Không
dùng credential hoặc tài nguyên AWS.

- `gofmt`, `go vet ./...`: PASS.
- `go test -count=1 ./...` với `MILES_DDB_LOCAL_REQUIRED=1`,
  `MILES_RECOVERY_S3_REQUIRED=1`: PASS.
- `TestNativeColdCloudEdgeFlows`, `TestPresignedLocalProfiles`: PASS.
- Smoke `miles` local mode + Moto + AWS CLI 2.39.3: `head-object` không có
  `Restore`; `restore-object` Warm → `InvalidRequest`, `Days=31` →
  `InvalidRequest`, key thiếu → `NoSuchKey`; `get-object` byte khớp.

## Giới hạn

- Chưa chạy RestoreObject qua endpoint Miles trên AWS thật; Moto completion là
  emulator evidence. Expiry thực sau Days và billing restore không được đo.
- HEAD đọc page/chunk/pack metadata và HEAD backend mỗi pack GLACIER của object
  (không pin): thêm I/O cho mọi HEAD; chưa có benchmark overhead.
- Local mode (Badger) không có lifecycle nên không có pack GLACIER; restore chỉ
  hoạt động trong shared Cloud/Edge runtime.
- Cross-domain CopyObject chưa có trong thay đổi này.
