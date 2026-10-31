# CopyObject, DeleteObjects và bucket P1 validation

Task **W04-CHIEN-04**, ngày 31/10/2026, từ `origin/main` `92deb23`. Hard dependencies
W03-CHIEN-03 và W04-HAI-01 đã Done trên backlog. Acceptance: single-copy ≤ 5 GiB và
bucket/delete operations pass; copy khác miền có contract, hoàn thiện ở W06-CHIEN-02.
Hợp đồng: [API §2.2.3, §4.2, §4.5, §4.6](../dedup-storage-gateway-api.md),
[kiến trúc §5.3](../dedup-storage-gateway-architecture.md).

| Checklist | Implementation / proof |
|---|---|
| CopyObject cùng miền, MetadataDirective COPY/REPLACE/self-copy, giữ opaque ETag/checksum nguồn | `object.Service.CopyObject` + `WriteSession.PrepareCopy`/`write.CopyManifest`; handler `internal/api/s3/copy.go`. `TestCopySameDomainRepeatsRefsWithoutPayload`, `TestCopyToItself`, `TestCopyObjectSourceAndDirective` |
| DeleteObjects XML per-key, DeleteBucket empty guard, GetBucketLocation | `internal/api/s3/delete_objects.go`, `bucket.go`; `storage.MetadataStore.DeleteBucket` qua control mutation `Buckets`/`entity_delete`. `TestDeleteObjectsPerKeyResults`, `TestDeleteObjectsRejectsBeforeAnyDelete`, `TestDeleteBucketFreesTheName`, contract `s3-delete-bucket-owner-incarnation-and-recreate` (Badger + DynamoDB), `TestGetBucketLocation` |
| Read nguồn/write đích, quota owner đích, threshold−1/=/+1, UploadPartCopy 501 | `TestCopyObjectNeedsSourceReadAndDestinationWrite`, dòng CopyObject của `authorization.json`; `TestCopySameDomainRepeatsRefsWithoutPayload` (grantee copy, owner trả quota), `TestCopyQuotaExceededKeepsDestination`; `TestCopyRejectsBeforeAdmission` (5 GiB + 1 / đúng 5 GiB); dòng `UploadPartCopy is P2` của `routes.json`; smoke AWS CLI bên dưới |

## Kiểm thử tự động

- Service trên Badger, pack adapter SDK và read path thật: copy 100 KiB (nhiều chunk) bởi
  grantee write-only không ghi pack mới, manifest mới lặp đúng refs nguồn, ETag/ETagKind/
  checksum/metadata bằng nguồn, quota `Used` của owner tăng đúng size; ghi đè nguồn sau
  copy không đổi byte của bản copy. Self-copy COPY bị từ chối trước admission, REPLACE ra
  revision và manifest mới, cùng ETag/nội dung, metadata mới, quota không đổi. Copy vượt
  quota owner trả `QuotaExceeded`, head đích và ledger giữ nguyên.
- Trước admission (head nguồn thay bằng stub): tombstone `NoSuchKey`, 5 GiB + 1
  `InvalidRequest`, đúng 5 GiB qua giới hạn copy rồi dừng ở quota, miền khác
  `NotImplemented`, đổi checksum algorithm `InvalidRequest`, cùng algorithm thành công.
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
`internal/object`, `internal/engine/write`, `internal/metadata/badger`, `internal/app`,
`cmd/miles`. Contract DynamoDB chạy với Moto 5.2.5 server loopback
(`MILES_DDB_LOCAL_ENDPOINT`); CI chạy lại với DynamoDB Local.

## Smoke binary thật

`bin/miles` build Linux, local mode (Badger + pack trên Moto 5.2.5), AWS CLI 2.36.41,
region `ap-southeast-1`, path-style:

| Bước | Kết quả |
|---|---|
| `get-bucket-location` | `{"LocationConstraint":"ap-southeast-1"}` |
| `copy-object demo/a → b` (3 MiB) | ETag = MD5 nguồn, Content-Type/metadata giữ, byte tải về bằng nguồn, số object pack trên Moto 8 → 8 |
| `--metadata-directive REPLACE` | Content-Type/metadata mới, ETag nguồn giữ |
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

- Copy khác miền trả `NotImplemented` (W06-CHIEN-02); nguồn ở pack không Warm/IR trả
  `InvalidObjectState` (copy metadata nguồn Cold là W06).
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
