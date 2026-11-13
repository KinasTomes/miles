# Native evidence và các module benchmark độc lập

**Chỉ có implementation và kiểm thử offline. Chưa chạy AWS hoặc duyệt ngân sách.**
Cách gọi từng bước: [runbook](../planning/full-aws-benchmark-execution-plan.md).
Không cần process controller; `full-benchmark.json` không phải luồng bắt buộc.

## Input/output và ranh giới

| Module | Input bắt buộc | Output / điều kiện thất bại |
|---|---|---|
| `run.py approve` | run/source/config + tài liệu của đúng action đã review | Receipt SHA-256, hết hạn 45 phút; sửa file thì invalid; không dispatch mutation |
| `direct-plan` / `direct-provision` | Exact bucket ARN theo hash run, IAM/cost/plan approval, AWS deploy role | Intent trước CreateBucket, ACK riêng, cấu hình versioning/SSE-KMS/public block/TLS/tags; resource cũ hoặc lost ACK không tự adopt/retry |
| `trial_state.prepare` | Terraform fleet, Chunks base table, Admin cluster/cache endpoints | Domain rỗng qua consistent scan; purge job và ACK đủ từng node; sample sau purge xác nhận memory/disk rỗng |
| `coldbench --native-config --aws-credentials-file` | Private binding 0600, AWS STS session riêng, manifest | Snapshot current + noncurrent versions trước/sau, retained physical bytes, class và generation của các pack được object tham chiếu |
| `recoverybench --native-config --aws-credentials-file` | Cùng binding, trusted Chrony của client, reviewed lease-revoke/checkpoint-cut | Probe lease/fence, cut intent, native seal, checkpoint/replay images, seed objects và full GET SHA-256 |
| `direct-cleanup-plan` / `direct-cleanup` | Ownership receipt, stop proof, client quiescence, approved exact versions | Retain response từng batch, reject partial delete/new version, confirm native absence; không restore IAM trước khi direct resource đã giải quyết |
| `benchmark-budget --inputs` | 20-case matrix, quantities, đủ rates/source/date | 60 slots, 24 pairs, itemized gross estimate; thiếu giá → UNKNOWN, không cho approval benchmark |

Binding private: account, assumed role, Region, physical/logical bucket, KMS key,
20 table names, Project/Environment tags, run ID, slot, source, history,
incarnation và stop_at. Không commit file này hoặc AWS credentials. Provider
không dùng DSG key để gọi DynamoDB, không đọc ambient AWS profile/endpoint/proxy.
STS phải khớp đúng account/role; session có expiry đủ cho stop_at. Source/table/
history lấy từ run và Terraform inventory đã freeze, không suy từ tên case.

## Recovery: điều đã được chứng minh và giới hạn

Injector acquire một **control lease mới của trial**, ghi identity trước dispatch,
đóng lease rồi yêu cầu native `CheckR1` và `BeginR1Action` đều trả `ErrFenced`.
`ErrUnavailable` không phải proof fencing, không cho release như thành công.
Không thu hồi writer của runtime, không kill VM, không trực tiếp update raw DDB
rows hoặc tua clock. Scope R1 là `lease-revoke`, R2 là `checkpoint-cut`.

Cut dùng `recovery.Worker.CloseAndSeal`, cùng adapter DynamoDB SharedAuthority
và Chrony như runtime. Worker của operator dùng supervisor để renew lease bình
thường mỗi 15 giây, join heartbeat trước khi release; không kéo dài lifetime
để né fencing. Gate/history/incarnation phải khớp; timeout hoặc unknown
mutation giữ native intent cho reconciliation. Không force mở gate. Sau cut,
verifier đọc epoch Sealed và kiểm checksum, contiguous chain, membership và mọi
dependency qua `VerifyCut`. Epoch tăng một mình không đủ.

`recovery.VerifyReplay` build hai lần **từ cùng genesis**, không lấy checkpoint
ở target làm base của lần thứ hai vì sẽ bỏ qua replay. Nó so root canonical và
namespace/quota/refcount records được dựng lại; image phải có các object seed
đúng bucket/key/size. GET sau đó kiểm toàn bộ SHA-256 từ manifest. Không cài image
đè live tables; đây không phải claim đã thử node crash, metadata destruction,
PITR hoặc boot một installation recovered.

## Cold: physical inventory và phạm vi verdict

Tái sử dụng `storage/s3.VersionInventory`: list mọi retained payload version,
không chỉ latest object. ExpectedBucketOwner đi kèm mọi page. Inventory phải
complete, không có duplicate version hoặc pending multipart bytes chưa tính;
hai vòng list giống nhau mới chấp nhận snapshot. Không có transactional S3
snapshot: nếu namespace thay đổi giữa các vòng thì dừng và quiesce/reconcile.

Native metadata resolve bucket → head → manifest → chunk location → pack
generation. Mọi pack được tham chiếu phải hiện diện trong version inventory ở
class yêu cầu; `x-amz-storage-class` của response DSG không tự chứng minh physical
transition đã xong. Inventory tổng hợp theo class/category; whole-installation
retained bytes không bị diễn giải thành dung lượng riêng của tenant hoặc chỉ lấy
logical GET bytes. Pack Warm cũ/Cold cũ/noncurrent vẫn được tính.

`coldbench` có scope `archive-read-restore-and-physical-inventory`. **Restore+GET
không chứng minh PUT/UploadPart revival**. Cờ revival giữ false khi chưa exercise;
case toàn lifecycle bị ghi INCOMPLETE và lệnh benchmark dừng. Việc bổ sung provider
không tự hoàn thành các phép thử expiry/protection-window/R3 còn thiếu.
Standard Flexible restore có thể vượt trial timeout; giữ evidence của lỗi và
snapshot sau lỗi nếu đọc được, không tăng timeout hoặc đổi tier để né approval.

## Cloud telemetry và isolation

Không lấy một scrape qua ALB làm metric cho cả fleet. `trial_state` đối chiếu
per-node cache heartbeat với hai Cloud target healthy và Edge instance ID từ
Terraform. Thiếu node, stale sample, sample duplicate, node đổi incarnation hoặc
counter giảm đều thất bại. Purge job cần terminal succeeded và ACK purged thật
cho mọi node, không phải dry-run. Có hoạt động/drain khác thì không purge.

Mỗi slot dùng domain mới, kiểm Chunks bằng consistent base scan; shared scenario
chỉ chia sẻ một domain giữa hai tenant **trong slot đó**. Prefix/bucket mới không
thay cho domain isolation. Các domain này là namespace benchmark, không sửa
bucket domain đã tạo. Memory/disk reset có proof; Bloom retained phải công bố và
không gọi là full cold-cache reset. Per-node hit/miss/lookups/errors/puts có baseline
và delta; không tự đổi chúng thành Cloud OS/clock telemetry hoặc throughput.
Edge chrony recorder vẫn cung cấp evidence clock của Edge; Cloud clock/OS thật
cần nghiệm thu ở G2/G3, không được khẳng định từ fixture.

## Dự toán và approval

Budget example dùng upper allowances cho dataset/versions/request amplification,
không giả định dedup/nén tiết kiệm trước khi đo. Giá lịch sử giữ `verified=false`;
rate null không thành zero. Model tính 90 ngày IR/Flexible kể cả xóa sớm, IR tối
thiểu 128 KiB/object, metadata Flexible 32 KiB archive + 8 KiB Standard, restore
request/GB/Standard copy và mọi dimension hạ tầng/network/metadata/observability.
Không cộng early-deletion charge lần nữa khi đã tính trọn minimum duration.
Pending deletion phải điền billable quantity theo điều kiện dịch vụ; không giả
định luôn tính thêm một tháng hay luôn miễn phí.

Nguồn điều kiện: [S3 pricing](https://aws.amazon.com/s3/pricing/),
[retrieval options](https://docs.aws.amazon.com/AmazonS3/latest/userguide/restoring-objects-retrieval-options.html).
Đơn giá cần refresh Singapore tại G1, gắn source và ngày. `window_fit` là đối
chiếu thời lượng **ước tính** với paid hours trừ 90 phút cleanup; không phải phép
đo throughput hoặc cam kết restore hoàn thành. Approval ngân sách vẫn do chủ
account quyết định; không được suy từ merge, CI hay credit còn lại.

## Kiểm thử offline

```bash
python3 -B -m unittest scripts.tests.test_acceptance_modules scripts.tests.test_acceptance_benchmark_preflight scripts.tests.test_acceptance_benchmark_case scripts.tests.test_acceptance_process scripts.tests.test_acceptance_kit
go test ./internal/benchmarknative ./internal/coldrunner ./internal/recoveryrunner ./internal/recovery
```

Regression bao phủ invalid/unknown inventory, noncurrent bytes, missing native
proof, unsafe fault scope, stale fence dispatch, corrupt/unsealed replay, budget
thiếu giá/minimum-duration, drift/expiry approval, existing/wrong-owner bucket,
lost Create ACK, changed versions và partial cleanup errors. Chúng là local
fixtures/test seams, **không phải native AWS evidence**. Muốn nghiệm thu phải chạy
đúng scope được duyệt rồi giữ raw receipts trên source đã pin.
