# Multipart Create, UploadPart, ListParts và Abort validation

Task **W04-CHIEN-02**, ngày 30/10/2026, từ `origin/main` `92deb23`. Hard dependencies
W03-HUNG-05, W04-HAI-01 và W04-HAI-02 đã Done trên backlog. Acceptance: part API pass
với SDK đã pin; gửi đủ part không được coi là object completed. Hợp đồng:
[API §2.2.3, §2.5–2.7, §4.3](../dedup-storage-gateway-api.md),
[multipart metadata handoff](../contracts/multipart-metadata-handoff.md).

| Checklist | Implementation / proof |
|---|---|
| Part body pipeline, part replacement CAS, checksum/raw_length/ETag, published/provisional quota | `object.Service.UploadPart` + `WriteSession.PreparePart` (page kind 2, tập pack generation chính xác). `TestUploadPartStagesVerifiedPartWithoutCompleting`, `TestUploadPartParallelAndReplacement`, `TestUploadPartBadBodyCancelsPublisher` trên Badger, pack adapter SDK và write session thật |
| Upload binding, size/part caps, active expiry, Abort/purge không xóa dữ liệu còn dùng | `internal/multipart` handlers qua `s3.Router`: `TestBindingRejectsBeforeState`, `TestStateTable`, `TestLostPublishAnswersWinningState`; `TestUploadPartQuotaFailsBeforeBody`, `TestAbortReleasesUploadAndKeepsSharedData`, `TestAbortClosesOpenPublisher` |
| Test parallel parts, replace cùng PartNumber, wrong initiator/bucket/key, full staging, aborted upload | Các test trên; AWS SDK Go v2 parse XML/headers (`TestSDKCreatePartListAbort`, `TestCreateChecksumDefaults`, `TestDefaultUploadStoresCRC64NVMEOfAnyPartChecksum`); smoke AWS CLI 2.36.41 và boto3 trên binary thật bên dưới |

## Kiểm thử tự động

- Part đã publish: ETag = MD5 hex, size = raw length, checksum CRC64NVME Base64 khớp
  body, revision 1; page kind 2 đọc được; publisher `Published` giữ pack đã retain;
  `GetObject` vẫn `ErrNotFound`; quota `Reserved = Staged = size`, `Used = 0`.
- Bốn part song song đều publish. Thay part 2 bằng body nhỏ hơn: revision 2, staged chỉ
  còn size mới. Thay part 3 đồng thời một cách tất định (publisher A đăng ký trước, bị
  chặn ở body; B publish xong; A tiếp tục): A thua CAS, đăng ký lại và publish trên
  revision của B, part cuối là của A (revision 3); không còn publisher mở.
- Part lớn hơn quota trả `ErrQuotaExceeded` trước khi đọc body, part > 5 GiB trả
  `ErrPartTooLarge` trước admission; không pack nào được ghi, counters giữ nguyên. Body
  lỗi checksum hoặc thiếu byte không publish và trả provisional bytes.
- Abort bởi owner (không phải initiator) của upload có part dedup lên chunk của object
  khác: Aborted, `Reserved = Staged = ActiveUploads = 0`, `Used` giữ size object;
  object vẫn GET đúng byte. UploadPart/ListParts sau đó bị từ chối; Abort lặp lại không
  đổi accounting. Publisher mở do request chết được Abort resolve qua `ResolvePartPublisher`
  rồi Aborted.
- Router + handler + SDK: Create SHA256 → COMPOSITE, metadata HTTP lưu; part CRC32 trên
  upload SHA256 → `InvalidRequest`; part SHA256 gửi hoặc không gửi checksum đều lưu SHA256
  đúng; ListParts phân trang (`max-parts`, `part-number-marker`, `NextPartNumberMarker`,
  `IsTruncated`, Initiator/Owner, checksum algorithm/type); Abort 204.
- Binding: ID không có, sai format, khác key, khác bucket incarnation → `NoSuchUpload`;
  owner/admin UploadPart thay initiator → `AccessDenied`, không tới service.
- Bảng state 4.3.1 cho UploadPart/ListParts/Abort ở Active, Active hết hạn, Completing,
  Completed còn/hết receipt, Aborting, Aborted. Publish thua freeze → `OperationAborted`,
  thua abort → `NoSuchUpload`, state vẫn Active → 503.
- Create: mặc định CRC64NVME/FULL_OBJECT, CRC32C → COMPOSITE, CRC32 FULL_OBJECT giữ,
  SHA1 FULL_OBJECT → `InvalidRequest`, key 1025 byte → `KeyTooLongError`.

Kết quả: `gofmt`, `go vet ./...` sạch; `go test ./...` pass trên Linux (Go 1.27.2,
WSL); `go test -race -count=1` pass cho `internal/object`, `internal/multipart`,
`internal/engine/write`, `internal/api/s3`, `internal/app`, `cmd/miles`; Python
`scripts/tests` pass.

## Smoke tiến trình thật (local mode)

Binary Linux, Moto 5.2.5 loopback làm pack backend, `mode=local` với `s3_address`.
Client: AWS CLI 2.36.41 (bản pin) và boto3 1.43.122 không awscrt.

1. `create-multipart-upload` mặc định; `upload-part` 1 (6 MiB) và 2 (1 MiB) song song:
   ETag bằng `md5sum`, response có `ChecksumCRC64NVME`; `list-parts` trả hai part với
   size, ETag, checksum, Initiator `dev`, `CRC64NVME`/`FULL_OBJECT`.
2. Thay part 2 bằng 512 KiB: ETag mới; `--max-parts 1 --part-number-marker 1` chỉ trả
   part 2, không truncated. `head-object` key đích 404; `complete-multipart-upload` 501
   `NotImplemented`.
3. Upload id trên key khác / id không có → `NoSuchUpload`. Abort 204; sau đó ListParts,
   UploadPart, Abort đều `NoSuchUpload`.
4. Upload `--checksum-algorithm SHA256`: part SHA256 trả đúng `openssl dgst -sha256`,
   part CRC32 → `InvalidRequest`; ListParts `SHA256`/`COMPOSITE`.
5. PUT object 300 KB, upload part cùng nội dung, abort: object tải về `cmp` khớp.
6. `aws s3 cp` 20 MiB: mọi part tải lên, dừng ở `NotImplemented` của Complete, object
   không tồn tại. boto3: `create_multipart_upload` mặc định + `upload_part` CRC32 mặc
   định thành công (`ChecksumCRC32` echo), `list_parts`, abort; `upload_file` 12 MiB
   dừng ở Complete.

## Giới hạn và bàn giao

- CompleteMultipartUpload (freeze, snapshot, prepare, commit, receipt, takeover) thuộc
  W04-CHIEN-03; part head đã có ETag hex, checksum Base64 và size cho `Assemble`.
- Local runtime chỉ admit ghi của user `dev`; quyền grantee/owner/admin được chứng minh
  qua Router trong test, không qua object service local.
- Part head không lưu thời gian publish nên ListParts không trả `LastModified`.
- ListParts của store yêu cầu write khi caller không phải admin: initiator chỉ còn grant
  read nhận `AccessDenied` dù bảng 2.2.3 cho phép (owner metadata).
- Publish chưa rõ kết quả giữ publisher mở; sweeper recovery tự động (`recover_uploads`)
  chưa có, Abort sau đó resolve được. Không chạy DynamoDB Local cho đường HTTP này; adapter
  DynamoDB đã pass contract multipart của W04-HAI-02.
