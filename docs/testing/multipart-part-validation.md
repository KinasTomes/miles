# Multipart Create, UploadPart, ListParts và Abort validation

Task **W04-CHIEN-02**, ngày 30/10/2026, từ `origin/main` `92deb23`. Hard dependencies
W03-HUNG-05, W04-HAI-01 và W04-HAI-02 đã Done trên backlog. Acceptance: part API pass
với SDK đã pin; gửi đủ part không được coi là object completed. Hợp đồng:
[API §2.2.3, §2.5–2.7, §4.3](../dedup-storage-gateway-api.md),
[multipart metadata handoff](../contracts/multipart-metadata-handoff.md).

| Checklist | Implementation / proof |
|---|---|
| Part body pipeline, part replacement CAS, checksum/raw_length/ETag, published/provisional quota | `object.Service.UploadPart` + `WriteSession.PreparePart` (page kind 2, tập pack generation chính xác); retain/publish qua `WriteSession.Do` (writer R1 được quản lý). `TestUploadPartStagesVerifiedPartWithoutCompleting`, `TestUploadPartParallelAndReplacement`, `TestUploadPartBadBodyCancelsPublisher`, `TestUploadPartStopsWhenWriterAuthorityCloses` trên Badger, pack adapter SDK và write session thật |
| Upload binding, size/part caps, active expiry, Abort/purge không xóa dữ liệu còn dùng | `internal/multipart` handlers qua `s3.Router`: `TestBindingRejectsBeforeState` (gồm Completed khác key: 0 lần đọc receipt), `TestStateTable`, `TestLostPublishAnswersWinningState`; `TestUploadPartQuotaFailsBeforeBody`, `TestAbortReleasesUploadAndKeepsSharedData`, `TestAbortClosesOpenPublisher` |
| Test parallel parts, replace cùng PartNumber, wrong initiator/bucket/key, full staging, aborted upload | Các test trên; quyền ListParts/GetPart theo §2.2.3 ở contract chung Badger + DynamoDB Local (`list-parts-read-initiator-get-part-write-initiator`); AWS SDK Go v2 parse XML/headers (`TestSDKCreatePartListAbort`, `TestCreateChecksumDefaults`, `TestPartChecksumMustMatchUpload`); smoke AWS CLI 2.36.41 và boto3 trên binary thật bên dưới |

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
- Writer R1 bị `Invalidate` sau khi retain pack, trước khi dispatch publish (hook
  `Before` của registry, tất định): UploadPart trả `ErrFenced`, không part nào publish,
  publisher được hủy và staged về 0.
- Abort bởi owner (không phải initiator) của upload có part dedup lên chunk của object
  khác: Aborted, `Reserved = Staged = ActiveUploads = 0`, `Used` giữ size object;
  object vẫn GET đúng byte. UploadPart/ListParts sau đó bị từ chối; Abort lặp lại không
  đổi accounting. Publisher mở do request chết được Abort resolve qua `ResolvePartPublisher`
  rồi Aborted.
- Router + handler + SDK: Create SHA256 → COMPOSITE, metadata HTTP lưu; part CRC32 trên
  upload SHA256, CRC64NVME chọn rõ hoặc mặc định → `InvalidRequest`, không tới service;
  part không gửi checksum trên upload CRC32 lưu CRC32 do server tính; ListParts phân trang
  (`max-parts`, `part-number-marker`, `NextPartNumberMarker`, `IsTruncated`,
  Initiator/Owner, checksum algorithm/type); Abort 204.
- Binding: ID không có, sai format, khác key, khác bucket incarnation, upload Completed
  của key khác (receipt đọc lỗi) → `NoSuchUpload`, không đọc receipt; owner/admin
  UploadPart thay initiator → `AccessDenied`, không tới service. Đúng key thì receipt
  mới được đọc.
- Quyền store (Badger và DynamoDB Local): initiator chỉ grant write → `GetPart` được,
  ListParts `AccessDenied`; owner, admin ListParts được; đổi grant thành chỉ read →
  ListParts được, `GetPart` `AccessDenied`.
- Bảng state 4.3.1 cho UploadPart/ListParts/Abort ở Active, Active hết hạn, Completing,
  Completed còn/hết receipt, Aborting, Aborted. Publish thua freeze → `OperationAborted`,
  thua abort → `NoSuchUpload`, state vẫn Active → 503.
- Create: mặc định CRC64NVME/FULL_OBJECT, CRC32C → COMPOSITE, CRC32 FULL_OBJECT giữ,
  SHA1 FULL_OBJECT → `InvalidRequest`, key 1025 byte → `KeyTooLongError`.

Kết quả: `gofmt`, `go vet ./...` sạch; `go test ./...` pass trên Linux (Go 1.27.2,
WSL); `go test -race -count=1` pass cho `internal/object`, `internal/multipart`,
`internal/engine/write`, `internal/api/s3`, `internal/app`, `cmd/miles`,
`internal/metadata/badger`, `internal/storage/...`; contract DynamoDB
(`TestLocalMultipartContract`) pass với DynamoDB Local 3.3.2 và
`MILES_DDB_LOCAL_REQUIRED=1`; Python `scripts/tests` pass.

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
   không tồn tại. boto3: upload mặc định và upload chọn rõ CRC64NVME nhận part CRC32 →
   `InvalidRequest`; Create `ChecksumAlgorithm=CRC32` (COMPOSITE) + `upload_part` mặc
   định thành công, `ChecksumCRC32` trả về và có trong `list_parts`; abort; `upload_file`
   12 MiB dừng ở Complete (lần chạy trước).

## Giới hạn và bàn giao

- CompleteMultipartUpload (freeze, snapshot, prepare, commit, receipt, takeover) thuộc
  W04-CHIEN-03; part head đã có ETag hex, checksum Base64 và size cho `Assemble`.
- Local runtime chỉ admit ghi của user `dev`; quyền grantee/owner/admin được chứng minh
  qua Router trong test, không qua object service local.
- Part head không lưu thời gian publish nên ListParts không trả `LastModified`.
- boto3 không awscrt phải chọn algorithm nó gửi được (ví dụ CRC32) lúc Create; part
  CRC32 trên upload CRC64NVME bị từ chối như S3.
- Publish chưa rõ kết quả giữ publisher mở; sweeper recovery tự động (`recover_uploads`)
  chưa có, Abort sau đó resolve được. Đường HTTP chỉ chạy trên Badger; DynamoDB pass
  contract multipart, gồm quyền ListParts/GetPart.
