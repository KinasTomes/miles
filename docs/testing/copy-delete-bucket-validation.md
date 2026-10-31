# CopyObject, DeleteObjects và bucket P1 validation

Kiểm chứng CopyObject cùng miền, DeleteObjects, DeleteBucket và GetBucketLocation.
Yêu cầu: single-copy ≤ 5 GiB, bucket/delete operations đúng hợp đồng; copy khác miền
trả lỗi rõ thay vì ghi tham chiếu chéo miền.
Hợp đồng: [API §2.2.3, §4.2, §4.5, §4.6](../dedup-storage-gateway-api.md),
[kiến trúc §5.3](../dedup-storage-gateway-architecture.md).

| Yêu cầu | Implementation / proof |
|---|---|
| CopyObject cùng miền, MetadataDirective COPY/REPLACE/self-copy, giữ opaque ETag/checksum nguồn, nguồn được bảo vệ trước khi resolve | `object.Service.CopyObject` + `readpath.WarmReader.BindSource` + `WriteSession.PrepareCopy`/`write.CopyManifest`; handler `internal/api/s3/copy.go`. `TestCopySameDomainRepeatsRefsWithoutPayload`, `TestCopyToItself`, `TestCopySourceHeadIsProtectedAgainstRetirement`, `TestCopyObjectSourceAndDirective`, `TestCopyObjectMetadataReadsBack` |
| DeleteObjects XML per-key, DeleteBucket empty guard, GetBucketLocation | `internal/api/s3/delete_objects.go`, `bucket.go`; `storage.MetadataStore.DeleteBucket` qua control mutation `Buckets`/`entity_delete`. `TestDeleteObjectsPerKeyResults`, `TestDeleteObjectsRejectsBeforeAnyDelete`, `TestDeleteBucketFreesTheName`, contract `s3-delete-bucket-owner-incarnation-and-recreate` (Badger + DynamoDB), `TestGetBucketLocation` |
| Read nguồn/write đích, quota owner đích, threshold−1/=/+1, UploadPartCopy 501 | `TestCopyObjectNeedsSourceReadAndDestinationWrite`, dòng CopyObject của `authorization.json`; `TestCopySameDomainRepeatsRefsWithoutPayload` (grantee copy, owner trả quota), `TestCopyQuotaExceededKeepsDestination`; `TestCopyRejectsBeforeAdmission` (5 GiB + 1 / đúng 5 GiB); dòng `UploadPartCopy is P2` của `routes.json`; smoke AWS CLI bên dưới |

## Kiểm thử tự động

- Service trên Badger, pack adapter SDK và read path thật: copy 100 KiB (nhiều chunk) bởi
  grantee write-only không ghi pack mới, manifest mới lặp đúng refs nguồn, ETag/ETagKind/
  checksum/metadata bằng nguồn, quota `Used` của owner tăng đúng size; ghi đè nguồn sau
  copy không đổi byte của bản copy. Self-copy COPY bị từ chối trước admission, REPLACE ra
  revision và manifest mới, cùng ETag/nội dung, metadata mới, quota không đổi. Copy vượt
  quota owner trả `QuotaExceeded`, head đích và ledger giữ nguyên.
- Bảo vệ nguồn: hook ngay sau lookup head nguồn gọi `WarmGate.Retire` cho pack nguồn; với
  GET (đối chứng) và CopyObject đều bị `ErrConflict`, request hoàn tất và bản copy đọc đúng
  byte nguồn.
- Trước admission (head nguồn thay qua metadata của reader): tombstone `NoSuchKey`, 5 GiB + 1
  `InvalidRequest`, đúng 5 GiB qua giới hạn copy rồi dừng ở quota, miền khác
  `NotImplemented`, đổi checksum algorithm `InvalidRequest`, cùng algorithm thành công.
- Metadata: REPLACE lưu Content-Language và mặc định `application/octet-stream` khi thiếu
  Content-Type; HEAD và GET của bản REPLACE trả header mới, bản COPY trả header nguồn.
- Handler qua `s3.Router` trên fake metadata: parse `x-amz-copy-source` (dấu `/` đầu, decode
  một lần, thiếu key, `versionId`, encode hỏng, bucket nguồn không có), directive, thứ tự
  quyền (writer không read nguồn, reader không write đích đều `AccessDenied` trước service),
  mapping lỗi service. DeleteObjects: per-key Deleted/Error, Quiet, lỗi toàn request trước
  tombstone đầu tiên (thiếu digest, Content-MD5 sai, 0/1001 key, key rỗng/dài, VersionId,
  điều kiện ETag, XML hỏng, reader), checksum CRC32 thay Content-MD5. DeleteBucket: reader/
  writer/user lạ `AccessDenied`, owner và system admin 204, lần hai `NoSuchBucket`, tạo lại
  cùng tên ra `bucket_id` mới và grant cũ không còn hiệu lực; mapping lỗi store.
- Contract `RunProducers` (Badger và DynamoDB): DeleteBucket thiếu mutation context
  `ErrFenced`; user không phải owner `ErrAccessDenied`; `bucket_id` khác `ErrBucketNotFound`;
  owner xóa ra event `Buckets`/`entity_delete` revision 2, GetBucket/Authorize không thấy
  bucket; xóa lại `ErrBucketNotFound`; tạo lại cùng tên ra incarnation revision 3 của user
  khác; system admin xóa được. Guard namespace còn object vẫn `ErrConflict`
  (`ErrBucketNotEmpty`) ở case `nonempty-delete` có sẵn.

Kết quả trên source hiện tại: `gofmt`, `go vet ./...` sạch; `go test -count=1 ./...` pass
trên Linux (Go 1.27.2, WSL); `go test -race -count=1` pass cho `internal/api/s3`,
`internal/object`, `internal/readpath`, `internal/engine/write`, `internal/multipart`,
`internal/metadata/badger`, `internal/app`, `cmd/miles`. Contract producer DynamoDB (gồm
DeleteBucket) chạy với Moto 5.2.5 loopback (`MILES_DDB_LOCAL_ENDPOINT`); toàn bộ suite
DynamoDB chạy trong CI với DynamoDB Local.

## Smoke binary thật

`bin/miles` build Linux, local mode (Badger + pack trên Moto 5.2.5), AWS CLI 2.36.41,
region `ap-southeast-1`, path-style:

| Bước | Kết quả |
|---|---|
| `get-bucket-location` | `{"LocationConstraint":"ap-southeast-1"}` |
| `copy-object demo/a → b` (3 MiB) | ETag = MD5 nguồn, Content-Type/Content-Language/metadata giữ, byte tải về bằng nguồn, số object pack trên Moto không đổi |
| `--metadata-directive REPLACE --content-language vi-VN` (không Content-Type) | HEAD/GET: `application/octet-stream`, `vi-VN`, metadata mới, ETag nguồn giữ |
| self-copy COPY / REPLACE | `InvalidRequest` / 200, ETag giữ, Content-Type mới |
| ghi đè nguồn sau copy | bản copy giữ byte cũ |
| nguồn không có key / bucket; `--checksum-algorithm SHA256` | `NoSuchKey` / `NoSuchBucket`; `InvalidRequest` |
| `upload-part-copy` | `NotImplemented` |
| 8 MiB − 1 B | `aws s3 cp s3://demo/t s3://demo/t.cli` dùng CopyObject, byte bằng nguồn; `s3api copy-object` OK |
| 8 MiB và 8 MiB + 1 B | `s3api copy-object` OK; `aws s3 cp` dừng ở GetObjectTagging `NotImplemented` |
| `aws s3 mv` | CopyObject + DeleteObject, nguồn 404 |
| `delete-objects` (2 key + key không có) | 3 Deleted, không Error; Quiet: body rỗng; VersionId `NotImplemented` |
| `delete-bucket` còn object | `BucketNotEmpty` |
| `s3 rm --recursive`, `s3 rb` | 204; sau cửa sổ snapshot 5 s HeadBucket 404, tạo lại cùng tên ra bucket rỗng |

## Giới hạn

- Copy khác miền trả `NotImplemented`: chưa có đường đọc/verify chunk nguồn rồi ghi lại
  trong miền đích. Nguồn ở pack không Warm/IR trả `InvalidObjectState`: copy metadata hiện
  chỉ nhận nguồn đọc được ngay.
- Grant của incarnation đã xóa không bị dọn vật lý; chúng gắn `bucket_id` cũ nên không có
  hiệu lực. Chưa có journal cleanup theo `bucket_id`.
- Trong ≤ 5 giây sau DeleteBucket, node còn snapshot quyền có thể trả HeadBucket 200 hoặc
  CreateBucket `BucketAlreadyOwnedByYou` cho tên vừa xóa (cửa sổ §2.2.3); write vào
  incarnation cũ luôn bị transaction từ chối.
- Badger/DynamoDB chạy DeleteBucket chỉ dưới mutation context tin cậy (local/benchmark
  admit như CreateBucket); thiếu context trả `ServiceUnavailable`. Readiness của mode `dev`
  dò credential fixture thay vì bucket probe, vì user dev nay xóa được bucket đó.
- DeleteObjects chạy tuần tự từng key; 1000 key là 1000 transaction.
- Không chạy AWS thật; không đo throughput.
