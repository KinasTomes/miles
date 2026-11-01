# CompleteMultipartUpload validation

Task **W04-CHIEN-03**, ngày 01/11/2026, từ `origin/main` `98ad195`. Hard dependencies
W04-CHIEN-02, W04-HAI-01, W04-HAI-02 và W04-PHONG-03 đã Done trên backlog. Acceptance:
large multipart upload/read-back đúng; lost-response retry không tái tạo object hoặc
double quota; kill tests lõi pass. Hợp đồng: [API §2.6, §2.7, §4.3, §4.3.1](../dedup-storage-gateway-api.md),
[multipart metadata handoff](../contracts/multipart-metadata-handoff.md).

| Checklist | Implementation / proof |
|---|---|
| Input digest trước freeze; hủy publisher rồi quiesce; snapshot pages bất biến | `completion.freeze` ghi page kind 3 rồi `FreezeCompletion`; `closePublishers` (`CancelPartPublisher`, kiểm `Publishers = ProvisionalBytes = 0`); `snapshot` ghi page kind 4 từ part head hiện hành. `TestCompleteCancelsOpenPublisher`, `TestRecoverUploadResumesFrozenInput` (resume từ input kind 3 đã freeze) |
| Commit head/quota/outbox/Completed receipt cùng transaction; fence/lease/quyền trước publish | `WriteSession.PrepareCompletion` + `WriteSession.Commit` với `MultipartAttempt`, projection Completing, receipt 7 ngày; `RenewCompletion` (kiểm lại quyền initiator) trước prepare và commit. `TestCompleteLargeUploadReadsBackOnce`, `TestCompleteQuotaExceededKeepsReservation`, `TestCompleteDeniedCommit` |
| CRC full combine, SHA/CRC composite, gap/order/size, retry identical/different, receipt trước Active expiry | `TestCompleteChecksumModes`, `TestCompleteValidationReopensActive`, `TestCompleteLargeUploadReadsBackOnce`; thứ tự/digest qua router và SDK: `TestSDKComplete` |
| Takeover sau lease/grace; unknown outcome giữ pins/reservation, đọc op_id/receipt; không TTL mù Completing | `TestCompleteKillPointsTakeOverOnce`, `TestCompleteLostCommitResponse`, `TestRecoverUploadResumesFrozenInput`, `TestRecoverExpiredCompletingAborts` |

## Kiểm thử tự động

Service trên Badger, pack adapter SDK (S3 trong bộ nhớ), write session R1 và read path
thật (`internal/object`); router + handler + AWS SDK Go v2 trên service giả
(`internal/multipart`).

- 5 MiB + 5 MiB + 3 KiB, CRC64NVME FULL_OBJECT: ETag `MD5(part MD5)-3`, checksum bằng
  CRC64NVME của cả object (combine theo raw length, không đọc lại byte), `PartCount` 3;
  GET trả đúng byte, LastModified là lúc Create; quota `Used = size`, `Reserved = Staged =
  ActiveUploads = 0`.
- Retry cùng input: receipt, cùng revision, quota không đổi; PUT đè key rồi retry: vẫn
  receipt revision 1, GET trả bản PUT, `Used` = size bản PUT. Input khác → `ErrConflict`
  (router trả `InvalidRequest` trước service). Sau 48 giờ (quá `expires_at` 24 giờ) vẫn
  trả receipt; sau 7 ngày `ErrNotFound`.
- Checksum mode: CRC32C FULL_OBJECT chọn part 1, 3 (bỏ part 2), CRC32 FULL_OBJECT,
  SHA256/CRC32/SHA1 COMPOSITE (`Base64(H(digest part nối nhau))-3`). Object đọc lại đúng;
  part không chọn vẫn tiêu hết staging, chỉ size được chọn vào `Used`.
- Sau freeze, lần lượt: part chưa upload và ETag khác → `InvalidPart`, checksum part khác →
  `BadDigest`, part không cuối < 5 MiB → `EntityTooSmall`, `x-amz-mp-object-size` khác →
  `InvalidRequest`, checksum object khác → `BadDigest`. Mỗi lần upload về Active, quota
  không đổi, `completion_op_id` giữ nguyên; lần hợp lệ sau đó commit với chính op đó.
- Quota: head 6 MiB lúc Create (r = 0), xóa head, PUT 12 MiB khác, limit 16 MiB → Complete
  `QuotaExceeded`, upload Active giữ reservation; xóa object kia rồi retry thành công.
- Publisher đã đăng ký trước freeze, chặn ở body: Complete hủy nó (`Cancelled`), part 3
  không publish; UploadPart đó trả lỗi khi body tiếp tục; quota không còn provisional.
- Kill (hook writer R1, tất định): sau freeze (prepare không dispatch), sau prepare (ack
  mất), trước transaction. Mỗi điểm: upload Completing, quota giữ nguyên reservation; retry
  trong lease → `ErrConflict` (`OperationAborted`); sau lease + grace (66 giây clock
  metadata) retry takeover cùng op, commit một lần, GET đúng, quota trừ một lần.
- Mất response sau transaction: outcome op được đối chiếu, Complete trả head revision 1,
  không commit lại; retry trả receipt; quota một lần.
- `RecoverUpload` (đầu vào `recover_uploads`): trong grace (62 giây) `ErrConflict`, sau
  66 giây resume input kind 3 đã freeze và commit. Completing quá 25 giờ vẫn Completing
  (không TTL); recovery không resume upload hết hạn: attempt bị đóng, upload Aborted,
  `Reserved = Staged = ActiveUploads = 0` một lần; recovery lặp lại không đổi counters.
- Commit bị từ chối quyền (hook trả `ErrAccessDenied`): attempt của client → Active,
  quota giữ; attempt takeover → Aborted, release một lần.
- Router + SDK: Active tới service với input đã chuẩn hóa (ETag bỏ ngoặc, chữ thường),
  `UserID`/credential của initiator; owner khác initiator → `AccessDenied`; Completing
  trong lease hoặc grace → `OperationAborted`, khác input → `InvalidRequest`, quá grace →
  service; Completed cùng input → service (replay), khác input → `InvalidRequest`, receipt
  hết → `NoSuchUpload`; Aborted → `NoSuchUpload`. Lỗi service: `InvalidPart`,
  `QuotaExceeded`, `ServiceUnavailable`. SDK parse `CompleteMultipartUploadResult`
  (Location, Bucket, Key, ETag, `ChecksumCRC64NVME`, `ChecksumType`).

Kết quả: `gofmt`, `go vet ./...` sạch; `go test ./...` pass trên Linux (Go 1.27.2,
WSL); Python `scripts/tests` pass. Trên Windows các package `internal/object`,
`internal/multipart`, `internal/engine/write`, `internal/metadata/badger`,
`internal/api/s3` pass; `internal/app`, `cmd/miles`, `cmd/dsgctl` chỉ fail vì kiểm quyền
file 0600/0700 của Windows như trước thay đổi này.

## Smoke tiến trình thật (local mode)

Binary Linux (WSL), Moto 5.2.5 loopback làm pack backend, `mode=local` với `s3_address`.
Client: AWS CLI 2.36.41 (bản pin, Windows) và boto3 1.43.122 không awscrt.

1. `aws s3 cp` 20 MiB: Create CRC64NVME, ba part, Complete không gửi checksum. `head-object
   --checksum-mode ENABLED`: `ETag "…-3"`, `ChecksumCRC64NVME 2wMI9Q8o2V4=`, `FULL_OBJECT`,
   bằng CRC64NVME tính độc lập trên file; `aws s3 cp` tải về `cmp` khớp.
2. `s3api` SHA256: part 5 MiB + 777 byte. Complete có part 3 chưa upload → `InvalidPart`;
   Complete đúng → `ChecksumSHA256 …=-2`, `COMPOSITE`, bằng SHA-256 của hai digest part tính
   độc lập; retry cùng input trả cùng ETag/checksum; input khác → `InvalidRequest`;
   `list-parts` → `NoSuchUpload`; abort → `OperationAborted`; `get-object` đúng byte.
3. boto3 `upload_file` 12 MiB (chunk 5 MiB, mặc định CRC32 COMPOSITE): `ChecksumCRC32
   …==-3`, `download_file` khớp SHA-256.

## Giới hạn và bàn giao

- Lịch sweeper `recover_uploads` mỗi 60 giây (job, lease job, quét Completing) là
  W05-CHIEN-02; task này giao `object.Service.RecoverUpload` cho một upload. Admission của
  recovery dùng credential lưu trong upload: credential bị thu hồi thì local authority từ
  chối admit, upload giữ Completing cho recovery authority sau này.
- `ClaimCompletion` (W04-HAI-02) từ chối takeover khi quá `expires_at`, nên Completing quá
  hạn được Abort (bước 4) thay vì resume như dòng Completing của bảng 4.3.1 ("kể cả quá
  `expires_at`"). Cần chốt hợp đồng nếu muốn resume.
- Lease gia hạn giữa các bước, không theo timer; một bước (prepare upload rất lớn) quá 60
  giây mất fence cho takeover. Takeover luôn snapshot/prepare lại (part không đổi khi
  Completing), không dùng lại manifest đã prepare của attempt cũ; manifest đó chờ GC.
- Quyền bị thu hồi thật được chứng minh ở contract store (commit/renew kiểm initiator); test
  service mô phỏng từ chối bằng hook writer. Đường HTTP chỉ chạy trên Badger; DynamoDB đã
  pass contract multipart của W04-HAI-02, chưa chạy qua service này.
- Local authority cấp lease mới cho mỗi lần admit thay vì một lease cố định theo op, để
  retry/takeover admit lại completion op; không lease nào bị dùng lại.
