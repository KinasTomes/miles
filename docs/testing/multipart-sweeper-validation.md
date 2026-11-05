# Multipart sweeper, takeover và quota adversarial validation

Ngày 04/11/2026, từ `origin/main` `1f78d9a`. Phụ thuộc đã Done trên backlog ngày
04/11/2026: Complete snapshot/receipt/takeover, GC Warm/compaction, Jobs/Nodes/drain và
checkpoint/rebuild. Acceptance: không kẹt Completing vĩnh viễn, không release
reservation chưa rõ outcome, receipt không tái tạo head đã xóa. Hợp đồng:
[API §2.6, §4.3, §4.3.1, §13.3](../dedup-storage-gateway-api.md),
[cluster operations](../development/cluster-operations.md).

| Checklist | Implementation / proof |
|---|---|
| Ghép `recover_uploads`/`purge_uploads` scheduler với completion lease/recovery APIs | Executor `uploadJobs` (`internal/app/upload_jobs.go`) chạy mỗi 60 giây trong runtime Cloud/Edge: purge chỉ đóng Active hết hạn/Aborting, recover chỉ takeover Completing sau lease + 5 giây grace qua `object.Service.RecoverUpload`. Bổ sung: recover để Aborting cho purge; lỗi không retry được của một upload thành `deferred` thay vì chặn cả lượt quét. `TestRecoverSweepSettlesAroundStuckUploads`; native DynamoDB Local `nativeUploadJobs` (Completing quá `expires_at`), `nativeSweepDuringCut` |
| Kill sau freeze/prepare/commit mất response; retry cùng input sau Active expiry trong receipt 7 ngày; digest khác bị từ chối | `TestCompleteKillPointsTakeOverOnce`, `TestCompleteLostCommitResponse`, `TestCompleteLostResponseAfterSlowCleanup`, `TestRecoverExpiredCompletingResumes`; mới: `TestCompleteReceiptKeepsDeletedObjectDeleted`, `TestTakeoverRejectsDifferentInput` |
| Part publish bị freeze hủy, Abort vs Complete, staging cap, quyền bị thu hồi trước publish | `TestCompleteCancelsOpenPublisher`, contract `sixteen-upload-owner-bound`; mới: `TestAbortAndCompleteEitherOrder`, `TestAbortCompleteLoserAnswersWinningState`, `TestUploadPartDeniedAtPublishReleasesStaging`, contract `abort-and-complete-in-either-order`, `staging-cap`, `revoked-initiator-before-publish` (Badger + DynamoDB Local) |

## Hành vi sweeper

- Kết quả chưa rõ (`ErrUnavailable`), mất fence/admission (`ErrFenced`, recovery cut), CAS
  conflict và context bị hủy giữ pending step; step chạy lại cùng checkpoint như trước.
- Lỗi khác của một upload (ví dụ input kind 3 đã freeze bị hỏng → `ErrInvalid`) tăng
  `deferred`, quét tiếp. Upload giữ nguyên state, roots và reservation; lượt định kỳ sau
  thử lại. Job kết thúc `failed` với `error_code=uploads_deferred`, progress có `deferred`.
- `recover_uploads` gặp Aborting (takeover bị thu hồi quyền, còn publisher mở) tính
  `skipped`; `purge_uploads` resolve publisher rồi Aborted, release một lần.

## Kiểm thử tự động

- Sweep step với store/service giả: thứ tự Completing hỏng input → bị thu hồi quyền →
  phục hồi được: cả ba được gọi, `completed=1`, `skipped=1`, `deferred=1`,
  `uploads_deferred`. Completing unknown outcome đứng đầu: step trả lỗi, không settled,
  upload sau không bị đụng. Completing còn lease 4 giây trước grace: không gọi recovery.
- Native (DynamoDB Local, ba node runtime, scheduler thật): upload Completing (freeze
  bằng fixture R1) bị lùi `expires_at` 1 giờ. `purge_uploads` succeeded, upload vẫn
  Completing; `recover_uploads` trong lease succeeded với `completed=0`, quota y nguyên;
  lùi `lease_until` rồi `recover_uploads` → Completed, `Used` tăng đúng size,
  `Reserved = Staged = ActiveUploads = 0`.
- Receipt sau DELETE: Complete 5 MiB + 1 KiB, owner DELETE object. Retry cùng input trả
  head revision 1 cùng ETag, GET vẫn `ErrNotFound`, quota không đổi; input khác
  `ErrConflict`. Lặp lại sau 48 giờ (quá `expires_at`). Sau 7 ngày `ErrNotFound`, key vẫn
  không tồn tại.
- Takeover quá `expires_at` 25 giờ với input khác → `ErrInvalid`; upload giữ Completing,
  cùng fence/revision, quota không đổi; input đã freeze takeover cùng op, quota một lần.
- Abort/Complete: Complete đã freeze (kill ở prepare) thì Abort của owner →
  `ErrConflict`, upload Completing và quota giữ; takeover sau grace commit, Abort sau đó
  vẫn `ErrConflict`. Abort trước thì Complete trên record đọc trước Abort → `ErrConflict`,
  upload Aborted, quota release một lần, GET `ErrNotFound`. Router + SDK: Complete thua
  Abort → `NoSuchUpload`; Abort thua Completing/Completed → `OperationAborted`.
- Contract store (Badger, DynamoDB Local): freeze rồi Abort → `ErrConflict`, ledger
  Active; Abort rồi freeze bằng revision cũ → `ErrConflict`, revision mới → `ErrFenced`;
  Abort lặp lại không release lần hai.
- Staging cap: quota seed `Staged = logical_limit + 100 GiB − 4` (staging của upload
  khác), limit logical 1 MiB còn đủ. Register publisher 5 byte → `ErrQuotaExceeded`,
  quota/upload không đổi; 4 byte được, `Staged` chạm đúng cap.
- Thu hồi quyền: owner đổi grant initiator thành chỉ read sau khi publisher đăng ký
  (provisional 4 byte). `PublishPart` và `RegisterPartPublisher` → `ErrAccessDenied`,
  quota không đổi; cancel publisher trả `Reserved = Staged = 0`, upload Active không còn
  publisher, không có part head. Service: `PublishPart` bị từ chối (hook) → UploadPart
  `ErrAccessDenied`, publisher bị hủy, staging về 0, ListParts rỗng.

## Kết quả

Linux (WSL), Go 1.27.2. DynamoDB Local 3.3.0 và Moto 5.1.15 theo
`deploy/local/compose.yaml` (loopback), `MILES_DDB_LOCAL_REQUIRED=1`,
`MILES_RECOVERY_S3_REQUIRED=1`:

- `gofmt -l cmd internal configs/pricing deploy/edge` rỗng, `go vet ./...` sạch.
- `go test -count=1 ./...` pass, gồm native runtime/contract DynamoDB.
- `go test -race -count=1` pass cho `internal/object`, `internal/multipart`,
  `internal/app`, `internal/metadata/badger`, `internal/metadata/dynamodb`,
  `internal/storage/...`.
- `python3 -B -m unittest discover -s scripts/tests`: 32 test OK; `go build ./cmd/...` OK.

## Giới hạn và bàn giao

- Chưa chạy trên AWS thật (DynamoDB, ALB, Edge); bằng chứng native là DynamoDB Local.
- Mode local/benchmark một node không có job runner: Completing ở đó chỉ resume khi
  client retry sau lease + grace.
- Kill là fault inject tất định qua hook writer R1, không phải kill process.
- Thu hồi quyền thật được chứng minh ở contract store; test service mô phỏng bằng hook.
- Upload `deferred` (input hỏng) không tự sửa và không có alarm riêng; operator thấy qua
  job `uploads_deferred` và progress `deferred`.
