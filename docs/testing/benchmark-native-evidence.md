# Native evidence và các module benchmark độc lập

**Implementation và kiểm thử offline; chưa chạy AWS cho thay đổi này.**
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
| `cold-plan` / `cold-step` | Slot đã seed, snapshot binding, exact stage/packs/policy, approval hash | Một phase mỗi lệnh; intent trước write/job/policy CAS; unknown outcome chặn redispatch |
| `cold-status` / `cold-reconcile` | Slot/source cố định, receipt/segment hiện có | Status không dispatch PUT/restore/job; reconcile chỉ công nhận receipt hoàn chỉnh, không gửi lại mutation |

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

`benchmark-case` cho Cold chỉ tạo tenant/bucket, seed Warm, kiểm bytes và native
snapshot. PUT storage-class header không phải công cụ chuyển tầng. Operator chờ
protection hết theo Chrony thật rồi gửi job tiering exact generation, pin policy
revision. Installation phải dành riêng cho acceptance, background tiering và
auto_restore tắt qua Admin policy CAS đã review; không tua UTC hoặc sửa TTL.

`coldbench --phase …` thực hiện độc lập inspect, restore-request, restore-status,
read, put-cold, part-cold, put-restored, part-restored. Bốn probe có chunk set rời
nhau để PUT trước không làm Warm nguồn của UploadPart sau. UploadPart được kiểm
native CAS/Warm **trước Complete**, rồi full GET kiểm SHA-256. Flexible phải tạo
Warm generation mới; IR phải giữ nguyên location. Mỗi ACK cần protection còn ít
nhất 15 phút tính từ trusted lower bound sau ACK. Old Cold payload versions và
sidecar vẫn tồn tại, không rút ngắn minimum retention.

Restore request kiểm `403 InvalidObjectState`, lưu intent/ACK và trả waiting ngay;
status đọc cả metadata lẫn native S3 state/expiry. Không giữ request chờ nhiều giờ.
Session có thể refresh giữa các phase nhưng source/installation/slot/dataset và
paid cleanup deadline không đổi. Lỗi/unknown giữ pending và segment; reconcile
thu đúng receipt, không tự retry PUT/part/job. Policy CAS mất ACK cần operator
đọc policy/job để giải quyết, không tự đoán đã apply hay rollback.

Protection test gửi tiering job vào pack vừa revive, cần checkpoint native
`r1_busy_or_retained`, hủy đúng job và chờ cancelled, xác nhận pack vẫn Warm.
Finish chỉ PASS khi đủ Cold PUT/part, Restored PUT/part, read SHA, retained inventory,
protection và telemetry. IR không gọi restore: read + reuse/protection được kiểm
riêng. Boundary Restored gần expiry được kiểm bằng fixture; không giả thành phép
đo AWS đã đợi expiry thật. Legacy `coldbench` không có `--phase` vẫn chỉ có scope
archive-read-restore-and-physical-inventory, không được dùng thay lifecycle verdict.

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
Edge chrony recorder cung cấp evidence clock của Edge. Foreground Warm/Recovery
và Cold data phases có collector Cloud riêng: metrics trực tiếp health address
cả hai Cloud EC2 và Edge EC2, OS CPU/memory/pressure và Chrony trong cùng sequence/window. Hai target
phải healthy, đúng account/installation/tags/source, boot/PID/executable không đổi.
Release-bundled script chạy qua SSM Command document cố định, pin version + AWS
SHA-256 từ Terraform. Không cấp `AWS-RunShellScript` hoặc tham số shell tự do.
Artifact gzip private có hash/size, tải theo chunk nhỏ để không vượt SSM stdout;
thiếu node/sample, clock fail, đổi fleet, mất sequence hoặc quá giới hạn đều fail.
Collector tối đa 20 phút/segment; thời gian chờ Glacier không phải measurement
window liên tục. Kiểm thử offline không thay xác nhận SSM/SSM Agent trên AWS.

## Spending envelope và approval

Operator chọn khoảng 10–20 USD; config mặc định `max_usd=20`, từ chối lớn hơn 20.
Đã bỏ model dự toán chi tiết và gate bắt buộc điền giá/quantities 60 slots.
`cost-review.json` ghi mode `operator-envelope`, estimate null; không gọi Pricing
API và không coi null là 0. Envelope không phải hard cap billing, không bảo đảm
60 trials hoàn thành trong mức này. Giữ thời hạn paid window, 90 phút cleanup,
review IAM/exact plans và theo dõi chi phí thực tế. Restore và retained Cold
versions còn phát sinh phí kể cả sau cleanup sớm. Chưa có số đo AWS trong PR này.

## Kiểm thử offline

```bash
python3 -B -m unittest scripts.tests.test_acceptance_modules scripts.tests.test_acceptance_benchmark_preflight scripts.tests.test_acceptance_benchmark_case scripts.tests.test_acceptance_process scripts.tests.test_acceptance_kit
go test ./internal/benchmarknative ./internal/coldrunner ./internal/recoveryrunner ./internal/recovery
```

Regression bao phủ invalid/unknown inventory, noncurrent bytes, missing native
proof, unsafe fault scope, stale fence dispatch, corrupt/unsealed replay,
drift/expiry approval, phase prerequisites/unknown outcome, fixed SSM document,
window/process mismatch, existing/wrong-owner bucket,
lost Create ACK, changed versions và partial cleanup errors. Chúng là local
fixtures/test seams, **không phải native AWS evidence**. Muốn nghiệm thu phải chạy
đúng scope được duyệt rồi giữ raw receipts trên source đã pin.
