# Auth, debug headers và hướng dẫn sử dụng API — Validation

Phạm vi: gate `x-dsg-debug`, các header vật lý `x-dsg-*`, phân quyền owner/grant và tách biệt credential, guide đầy đủ cho client/grants/presigned/multipart/copy-limit/restore, actual API error schemas và bảng đối chiếu API coverage P0/P1/`dsgctl`/P2 trên hai profile Cloud và Edge.

Tài liệu chính: [Hướng dẫn sử dụng đầy đủ và đối chiếu API](../api-usage-guide.md) (đã đồng bộ với [API v2](../dedup-storage-gateway-api.md) và [README](../../README.md)).

## Checklist hiện thực và kiểm thử

| Yêu cầu | Hiện thực và kiểm chứng |
|---|---|
| Gate `x-dsg-debug` và 7 header `x-dsg-*` | `s3.Router` yêu cầu đồng thời `X-Dsg-Debug: 1` đơn trị trong `SignedHeaders`, `system_admin=true`, `debug_allowed=true` và authorization hợp lệ trên bucket (`owner` hoặc `read`/`write` grant). `write.Pipeline`/`PreparedObject` tính `DupChunks`, `RevivedChunks`, `StoredBytes`; `readpath.WarmReader` chỉ quét `cache.Has` khi `WarmRequest.Debug == true` (non-debug GET bỏ qua hoàn toàn phép probe `Has`), tính `CacheStatus` (`hit`/`miss`/`partial`) không làm biến đổi stats/LRU và trả `ColdRestoreError` (`requested`/`in-progress`); `s3.Objects`, `multipart.Handlers` và `s3.Router` chỉ xuất `x-dsg-*` khi `Caller.Debug == true`. |
| Không lộ thông tin vật lý cho user thường / thiếu quyền | `s3.TestDebugHeadersGateAndPhysicalCounts`, `s3.TestDebugHeadersDeniedAndNeverLeakedToRegularUsers`, `readpath.TestWarmReaderPreflightCacheStatus`, `multipart.TestUploadPartDebugHeadersGate`, `admin.TestAdminProvisionedDebugGateAndGrantSeparation`: kiểm tra owner thường, grantee thường, `system_admin` thiếu `debug_allowed`, `debug_allowed` thiếu `system_admin`, header không ký / giá trị `0` / trùng giá trị, non-debug GET không gọi `Cache.Has`, admin+debug không có grant / thiếu quyền read hoặc write (`403 AccessDenied`), presigned query `?x-dsg-debug=1` (`501 NotImplemented`), presigned header, bearer token gửi lên S3, và revoke grant trong `≤ 5s`. |
| Hướng dẫn đầy đủ + actual error schemas | [docs/api-usage-guide.md](../api-usage-guide.md) tổng hợp hướng dẫn client, grants, presigned, multipart, copy-limit (5 GiB, `multipart_threshold=5GB`, `s3api copy-object`, download+multipart), Cold/RestoreObject, cùng toàn bộ 33 S3 XML error codes (`internal/api/s3/errors.go`) và 22 Admin JSON error codes + schema thực tế (`faults`, `stats`, `layout`, `packs`, `jobs`, `cache`, `nodes`, `cost/report`). |
| Đối chiếu API coverage & hai profile Cloud/Edge | `tests/integration.TestMandatoryS3AndAdminCoverageChecklist` và `TestBothProfilesRejectSharedCredentialsAndValidateStartup`: kiểm mọi operation S3 P0 (8) + P1 (12), P2/out-of-scope (`501 NotImplemented`), `GET /admin/v1/cost/report` (`401`/`403`/`200`/`400`), bare 404/405 cho route/method lạ. `cmd/miles.TestGuideEndToEndBothProfilesAndCredentialSeparation`: chạy full luồng guide trên cả hai profile `cloud` và `edge` (kể cả `cost/report`) với tách biệt cứng DSG SigV4, Admin Bearer và backend credential. `cmd/dsgctl.TestCLIRequiresTokenOnlyForSupportedReadCommands`: xác nhận `cost report` được nối và `rebuild-index`, `sim run` trả `ErrUnsupported`. |

## Lệnh xác minh

Chạy trên Linux (`wsl -d Ubuntu`, Go 1.27.2 `linux/amd64`):

```sh
make check
go test -race -count=1 ./internal/cache ./internal/engine/write ./internal/readpath ./internal/object ./internal/api/s3 ./internal/multipart ./internal/api/admin ./internal/auth ./cmd/dsgctl ./cmd/miles ./tests/integration
```

## Giới hạn
- CLI `dsgctl` đã nối `cost report` tới `GET /admin/v1/cost/report`; mô phỏng offline chi tiết nằm tại `cmd/costsim`, còn `rebuild-index` là workflow thư viện guarded trong `internal/recovery` chưa mở trên `dsgctl`.
- Kiểm thử end-to-end hai profile trong `cmd/miles` dùng `mode=local` (Badger) và emulator HTTP S3 PackStore nội bộ; không thay thế nghiệm thu triển khai AWS thật với ALB, Roles Anywhere, Chrony hoặc 20 bảng DynamoDB.
