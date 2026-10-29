# W04-PHONG-02 — Policy persistence và tiering/revival eligibility

Trạng thái: implementation chờ review, API extension là PROPOSAL, chưa Frozen.
Task/AC lấy từ nội dung Miles do Phong cung cấp trong phiên làm việc. Không tự
đánh dấu checklist hoặc Done trên Miles.

Baseline `0e60828193a1f436408685df2d0b6ed82fcd34e7`, branch
`codex/W04-PHONG-02-policy-eligibility`.

## Checklist và phạm vi

| Mục | Implementation/evidence |
| --- | --- |
| C1: SystemConfig/cache policy, DDB revision, cùng evaluator thật/ảo | `internal/config/integration/bucket.go`, optional `storage.BucketPolicyStore`, native Badger/DDB adapters; `TestPolicyPersistence` / `TestLocalPolicyPersistence` chạy cùng contract suite và parity từ snapshot đã persist |
| C2: Warm STANDARD, Cold IR mặc định/GLACIER override, live ratio/age/protection/retention | `policy.EvaluateTiering`; `TestTieringSelectionBoundaries`, `TestTieringRealVirtualParityAndNoImmediatePUTTiering`; pack writer vẫn tạo STANDARD |
| C3: revival/reuse | `policy.WriteReuseCandidate` dùng chung evaluator và publisher; `TestWritePipelineRevivesArchiveAndEveryRestoredFromBody`, `TestWritePublisherIRCanAttemptExactReuseWithoutRestore`; backend parity kiểm restore boundary −1ns/equality/+1ns |
| AC: validation/backend; không tier ngay sau PUT; không đổi identity theo bucket | Complete candidate được validate trước mutation và tại backend bucket; protection thiếu thì body revival; bucket policy không chứa tiering/chunking/compression/domain, không mutate Bucket/ChunkKey |

Nguồn: [Clock/Policy](../contracts/clock-and-policy.md) §4–7,
[Architecture](../dedup-storage-gateway-architecture.md) §5/7/19.1,
[Engine](../contracts/chunking-and-commit.md),
[R1/Cache](../contracts/r1-and-cache.md),
[R2](../contracts/r2-recovery.md).

“Cold revival” phải đọc theo storage class: archive GLACIER và mọi Restored được
viết Warm từ client body. Cold GLACIER_IR vẫn đọc/reuse trực tiếp nếu đủ protection
và exact-generation pin. Không sửa ngoại lệ IR đã chốt trong W1 để diễn giải câu
checklist thành revive mọi IR. GET/Copy payload dùng `EvaluateRead`, Restored cần
expiry lower bound ≥ `D_hi + 120s`; equality hợp lệ. Không phát RestoreObject cho IR.

## Persistence và handoff API

- Giữ `MetadataStore.GetPolicy/PutPolicy` cho global revision và backend hiện có.
- Thêm optional `BucketPolicyStore`, không mở rộng mandatory MetadataStore hoặc
  tạo dịch vụ metadata thứ hai. Candidate tái sử dụng `storage.PolicySnapshot`.
- `GetBucketPolicy(ctx, bucket, bucketID, revision)`: zero chọn current; positive
  chọn đúng immutable history. Tên bucket phải còn active, ID phải khớp. Caller
  phải authorize trước disclosure; read helper không tự tạo authorization.
- `PutBucketPolicy`: owner/system admin mới được sửa; grantee write không đủ.
  Validate complete cache-only document, writer/incarnation, admitted mutation,
  expected revision, no-op/CAS trong cùng native transaction.
- `CreateBucketPolicy` bootstrap explicit với expected0 khi chưa có record;
  caller lựa chọn defaults ở creation/migration path. Không tự tạo record trên
  GET, không fallback defaults/current khi thiếu history. Việc gọi helper tại
  Admin/create-bucket consumer do Chiến tích hợp; không mở endpoint mới ở task này.
- Global G và bucket B độc lập, không tăng revision của nhau. `LoadGlobal` rồi
  `LoadBucket` pin cặp G/B; không hứa hai revisions tạo snapshot toàn cluster.
  Request đang pin giữ nguyên policy dù control plane cập nhật.
- Candidate Revision=0; backend cấp next revision. No-op vẫn kiểm CAS/auth/fence,
  không thêm history/event, nhưng giữ operation outcome phục vụ retry.
- Chuẩn hóa complete cache document trước idempotency digest; digest giữ expected
  revision, bucket incarnation và writer. Không tự retry/rebase proposal stale.
- Current pointer, history, typed payload, producer outcome và outbox nằm trong
  một transaction. Lost ACK là unknown outcome; retry cùng admitted mutation hoặc
  đọc `ProducerStore.GetMutation`, không suy rollback từ error/current giống nhau.

### Namespace cần consumer review

Không tạo table/GSI mới. Trên SystemConfig:

```text
bucket-cache/<hex UTF-8 bucket_id>/current
bucket-cache/<hex UTF-8 bucket_id>/history/<16-digit hex revision>
```

R2 `SystemConfig` entity identity là `bucket-cache/<hex UTF-8 bucket_id>`; validator
chỉ nhận namespace này hoặc `global-policy`. Reuse event v1/typed Projection và
producer exporter hiện có, không tạo format R2 cạnh tranh. Tests export payload
thật; chưa tuyên bố recovery installation trên AWS đã được kiểm chứng. History
không có TTL/job cleanup; retention roots/watermark vẫn do R2 sở hữu.

`NoOp` producer được bổ sung HistoryID/LogicalOpID/Epoch trong retained result để
retry cùng mutation không bị `Replay` từ chối vì thiếu binding. Không tạo change
event cho no-op.

## Tiering evaluator và protection

`EvaluateTiering` là pure function trong package policy, dùng qua cùng immutable
Context của runtime và model. Không đọc host clock/S3/DDB, không execute transition.

Inputs: pinned global policy, request monotonic budget, UTC proof bound đúng Now
và horizon, exact PackInfo/state revision, unique-live stored bytes/total bytes,
metadata/authority và observation `SafeToTransition`.

`SafeToTransition` phải do caller chứng minh dưới R1/R2, bao reader/writer/upload
và recovery roots. Đây là fact cho evaluator, không phải concrete authorization
hoặc pin token. Unknown không Eligible; evaluator không mint authority.

- Chỉ chọn Warm STANDARD/IA, policy enabled. Default destination GLACIER_IR,
  global override GLACIER. Cold không tự promote hoặc compact.
- Age tính UTC 24h từ LastAccess; age equality đủ. `cold_after_days=0` chỉ bỏ age.
- Live/total dùng exact decimal và nhân chéo big.Int; zero/unknown total không
  eligible, không dùng refcount nhân số logical occurrences.
- Dùng UTC **lower bound** để so age/protection/retention, không cho uncertainty
  làm hết protection sớm. Missing proof/timestamp cần thiết fail closed.
- Không tier trước ProtectedUntil/MinRetentionUntil. IA thiếu retention thì defer.
  Eligible không cấp quyền xóa hoặc tự thiết lập Cold retention; executor W06 phải
  ghi retention 90 ngày theo class/nguồn và revalidate R1/CAS khi thực sự chuyển.
- New pack luôn Warm STANDARD với protection caller cung cấp. Reuse chỉ được
  attempt nếu durable protection đã phủ `StorePublisher.ProtectedUntil`; thiếu
  port atomic extension thì revive từ body, không ghi timestamp giả. Đây là
  fallback bảo thủ, có write amplification; tối ưu atomic max do lifecycle/R1
  downstream review. Không giảm protection đã tồn tại.

## Test evidence và giới hạn

Native backend suite bao phủ bootstrap, current/history, independent revisions,
hai proposal concurrent, stale no-op, no-op retry, owner/admin/grantee, incarnation,
stale fence, thiếu admission, reject malformed/duplicate/unknown/incomplete fields,
owned output, fault trước commit và lost ACK, export producer payload. Real/Virtual
dùng domain riêng và cùng controlled UTC source evidence, không sleep để tạo race.

Revival tests dùng Pipeline + production pack writer/verifier, canonical CAS và
Fake PackStore; chứng minh source RangeGet/Restore không được gọi khi revival,
identity/domain giữ nguyên, generation/location revision đổi, old pack không bị
retire/xóa. PUT/UploadPart ở đây dùng chung body preparation; không tuyên bố đã
triển khai toàn bộ endpoint UploadPart hoặc CopyObject.

### Validation đã chạy

| Kiểm tra | Kết quả |
| --- | --- |
| `go test -count=20 ./internal/policy/... ./internal/config/... ./internal/engine/write/...` | PASS trên Windows Go 1.27.2 |
| Policy persistence Badger và DynamoDB Local, lặp 5 lần | PASS; không SKIP native DDB |
| Linux `go test ./...` | PASS; `MILES_DDB_LOCAL_REQUIRED=1` |
| Linux `go vet ./...` | PASS |
| Linux `go test -race -count=1 ./internal/policy/... ./internal/config/... ./internal/engine/write/... ./internal/metadata/badger ./internal/metadata/dynamodb ./internal/storage/...` | PASS |
| Race backend policy sau khi bổ sung native snapshot round-trip | PASS |
| gofmt, whitespace gồm untracked files, Markdown local links, `git diff --check` | PASS |

Linux dùng image `golang:1.27.2` digest
`sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8`,
source mount read-only; DDB Local dùng image digest đã pin trong
[compose](../../deploy/local/compose.yaml), project riêng `miles-w04-policy-contract`.

Full suite Windows FAIL tại tests yêu cầu POSIX permission `0600/0700` và directory
sync ở CLI/adminclient/app/benchmark/secretfile. Không sửa các checks đó; toàn bộ
suite tương ứng PASS trên Linux. Race Windows NOT VERIFIED do lỗi GCC/system DLL;
chỉ kết quả race Linux ở trên được ghi PASS. Không có GitHub CI run cho code chưa
push này. Toolchain Windows đầy đủ lấy từ go.dev, đã kiểm SHA-256 trước giải nén,
không sửa go.mod/go.sum hoặc ghi đè bộ Go cũ thiếu stdlib.

Chưa có live AWS, production UTC attestation, distributed R1 proof, lifecycle job,
Admin HTTP/CLI wiring hoặc full simulator integration trong thay đổi này. Các phần
đó vẫn thuộc consumer/downstream; không dùng parity test làm distributed proof.

## Review/handoff

- Hải: optional port, SystemConfig namespace, transaction/outbox projection và
  durable protection extension sau này. DDB vẫn dùng injected WriterAuthority /
  ProducerAuthority hiện có; fixture authority không chứng minh distributed R1.
- Chiến: authorize trước load, strict merge, expected bucket ID/revision,
  WithMutation và producer admission; map errors theo Admin contract đã có.
- Hưng: cùng evaluator cho model; review safety-facts adapter và fallback revival
  khi chưa thể persist extension. Chưa freeze API trước consumer review.
- W05-PHONG-02/W06-PHONG-01: pack discovery/statistics, lifecycle scheduling,
  atomic Transitioning gate, actual restore/transition/retention và reconciliation.
