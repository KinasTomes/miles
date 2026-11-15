# Miles

**Multi-tier Intelligent Less-waste Efficient Storage** — cổng lưu trữ tương thích một tập API S3, viết bằng Go. Miles lưu dữ liệu trong các pack trên Amazon S3, quản lý metadata bằng BadgerDB hoặc DynamoDB, và kết hợp deduplication, nén, cache cùng phân tầng lưu trữ.

Mục tiêu là đánh giá đánh đổi giữa dung lượng, chi phí và hiệu năng cho workload backup có dữ liệu lặp. Kết quả benchmark được ghi theo từng môi trường, dataset và phiên bản; không mặc định Miles luôn rẻ hoặc nhanh hơn Direct S3.

## Tính năng

| Nhóm | Khả năng |
|---|---|
| S3 API | Bucket operations; PUT, GET, HEAD, Range, LIST, DELETE; multipart; CopyObject; presigned URL; RestoreObject; SigV4 và kiểm tra checksum |
| Lưu trữ | FastCDC, SHA-256, dedup theo domain, zstd, immutable packs và manifest |
| Phân quyền | Chủ bucket, grant read/write độc lập, credential/token riêng; quota theo byte logical |
| Cache và lifecycle | Cache theo node; GC/compaction; GLACIER_IR và GLACIER Flexible Retrieval; restore bất đồng bộ và revival |
| Độ bền và vận hành | R1 leases/fencing/read pins; R2 outbox/journal/sealed cuts/checkpoint/rebuild; Admin API, `dsgctl`, readiness và metrics |
| Đánh giá | Dataset generator, Direct-S3 baseline, benchmark Cloud/Edge và cost simulator |

Đây là dự án đang nghiệm thu tích hợp. Có implementation và các bộ kiểm thử cho những nhóm trên; việc có endpoint hoặc CI xanh không đồng nghĩa toàn bộ ma trận AWS đã đạt. Phạm vi chính xác xem [API](docs/dedup-storage-gateway-api.md), [hướng dẫn sử dụng](docs/api-usage-guide.md) và [tiêu chí nghiệm thu](docs/acceptance/definition-of-done.md).

## Hai profile triển khai

| | Cloud A | Edge B |
|---|---|---|
| Đường đi client | Client cùng AWS Region → ALB HTTPS → Gateway cluster | Client → Gateway trên VM/local hoặc EC2; không bắt buộc ALB |
| Backend | S3 và DynamoDB qua VPC endpoints | S3 và DynamoDB qua HTTPS; on-prem thường qua Internet |
| Danh tính AWS của gateway | EC2 instance profile | IAM Roles Anywhere; credential provider phải được cấu hình theo môi trường |
| Metadata nhiều node | DynamoDB dùng chung | DynamoDB dùng chung cho 1–2 node |

Hai profile dùng cùng core và binary tương ứng kiến trúc CPU; khác cấu hình, mạng và credential provider. Chế độ `local` dùng BadgerDB + S3 giả lập cho phát triển một node, không thay bằng chứng Cloud/Edge thật. Edge chạy trên EC2 cũng không đại diện cho hiệu năng WAN của một máy on-prem.

## Build và thử local

Yêu cầu Go **1.27.2** theo `go.mod`, GNU Make và Python 3.12+. Docker Engine/Compose và AWS CLI v2 cần cho bộ fixture local; không cần đăng nhập AWS để chạy bộ fixture này.

```bash
make build
./bin/miles -version
./bin/dsgctl -help
make test-local
```

`make test-local` tạo backend/process tạm, kiểm tra các luồng local rồi tự teardown; nó không để lại một gateway để dùng lâu dài và không triển khai AWS.

Để chạy tương tác, làm theo [local runtime](docs/development/local-runtime.md): mở Moto bằng Compose, lấy port loopback, tạo bucket pack vật lý rồi cấu hình `configs/local.example.json`. Credential client được tạo trong `local_data_path/credentials.json`, khác credential dummy của Moto. Không chạy hai process dùng cùng Badger directory.

`make run-dev` chỉ dùng fixture phát triển; cấu hình mặc định không mở S3 listener. Nó không phải lệnh triển khai gateway Cloud/Edge. Với runtime AWS, bắt đầu từ [shared Cloud/Edge runtime](docs/development/shared-runtime.md) và các cấu hình `configs/shared-cloud.example.json`, `configs/shared-edge.example.json`.

## Dùng gateway bằng AWS CLI

Trước khi chạy, operator cung cấp endpoint S3, Region, credential **Miles SigV4** và CA nếu dùng TLS nội bộ. Các lệnh dưới đây dành cho một bucket thử mới trên gateway đã sẵn sàng.

Trong một terminal client riêng, tạo profile tách khỏi AWS backend:

```bash
umask 077
PRIVATE_DIR="$HOME/.local/state/miles-client-demo"
mkdir -p "$PRIVATE_DIR"
export AWS_CONFIG_FILE="$PRIVATE_DIR/config"
export AWS_SHARED_CREDENTIALS_FILE="$PRIVATE_DIR/credentials"
export AWS_EC2_METADATA_DISABLED=true
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_SECURITY_TOKEN
unset AWS_PROFILE AWS_DEFAULT_PROFILE

# Nhập access key/secret do Miles cấp, Region của installation và output json.
aws configure --profile miles-client
aws configure set s3.addressing_style path --profile miles-client

ENDPOINT="https://miles.example"
# Nếu dùng CA riêng: export AWS_CA_BUNDLE=/absolute/private/miles-ca.pem
BUCKET="miles-demo-$(date -u +%Y%m%d%H%M%S)"
```

Không nhập IAM backend key ở bước này. Không tắt xác minh TLS. Nếu thử local, thay endpoint bằng địa chỉ loopback S3 đã cấu hình.

```bash
aws --profile miles-client --endpoint-url "$ENDPOINT" s3api create-bucket --bucket "$BUCKET"
printf 'Hello from Miles\n' > "$PRIVATE_DIR/hello.txt"
aws --profile miles-client --endpoint-url "$ENDPOINT" s3api put-object \
  --bucket "$BUCKET" --key hello.txt --body "$PRIVATE_DIR/hello.txt"
aws --profile miles-client --endpoint-url "$ENDPOINT" s3api head-object \
  --bucket "$BUCKET" --key hello.txt
aws --profile miles-client --endpoint-url "$ENDPOINT" s3api get-object \
  --bucket "$BUCKET" --key hello.txt "$PRIVATE_DIR/downloaded.txt"
cmp "$PRIVATE_DIR/hello.txt" "$PRIVATE_DIR/downloaded.txt"
sha256sum "$PRIVATE_DIR/hello.txt" "$PRIVATE_DIR/downloaded.txt"
aws --profile miles-client --endpoint-url "$ENDPOINT" s3api list-objects-v2 --bucket "$BUCKET"
```

`cmp` phải exit 0 và hai SHA-256 phải giống nhau. Tên bucket mới được giữ trong biến `BUCKET`; chỉ dọn dữ liệu thử do mình tạo sau khi hoàn tất demo.

Hướng dẫn chi tiết cho multipart, presigned URL, CopyObject, checksum và restore nằm trong [API usage guide](docs/api-usage-guide.md).

## Quản trị và chia sẻ bucket

Operator bootstrap Admin token một lần khi gateway dừng; user thường dùng credential/token riêng. Admin API chỉ truy cập qua mạng quản trị, loopback hoặc SSH tunnel đã xác minh; không mở cổng quản trị ra Internet.

```bash
export DSGCTL_ENDPOINT=http://127.0.0.1:19000
export DSGCTL_TOKEN_FILE=/absolute/private/owner-token
./bin/dsgctl usage alice
./bin/dsgctl bucket show demo
./bin/dsgctl grant ls demo
```

Port `19000` trong ví dụ là đầu local của tunnel do operator mở. Token file phải là regular file mode `0600`. Global flags đứng trước subcommand; command flags đứng sau positional arguments.

Chủ bucket cấp/thu hồi grant bằng `grant set`/`grant rm`, dùng `bucket_id` và revision thật vừa đọc. Read/write độc lập; system administrator không có quyền đọc/ghi object ngầm định. Quota và usage của user tính theo byte logical; thống kê vật lý/dedup chỉ hiện ở bề mặt quản trị được phép. Xem [luồng users → credentials → tokens → grants](docs/api-usage-guide.md).

## Triển khai và vận hành

- [Terraform Cloud và cấu hình installation](infra/terraform/cloud/README.md): hạ tầng, namespace riêng, IAM/bootstrap và teardown.
- [Shared runtime](docs/development/shared-runtime.md): provision lần đầu, backend, keys, clock và cấu hình hai profile.
- [Cloud deployment](deploy/cloud/README.md), [Edge deployment](deploy/edge/README.md): release, TLS và service lifecycle; đối chiếu với shared runtime cho wiring hiện hành.
- [Lifecycle/recovery runbook](docs/runbooks/lifecycle-recovery-operations.md): tiering, restore/promote, GC, checkpoint/rebuild, drain và unknown outcomes.
- [AWS acceptance operator guide](docs/testing/focused-aws-operator-guide.md): công cụ cho môi trường nghiệm thu disposable; không phải điều kiện để mọi người dùng upload file.

Client backup theo luồng tạo bucket → upload → đọc lại/kiểm hash → LIST/HEAD; quản trị viên chuẩn bị tài khoản, grant, policy và hạ tầng. GLACIER_IR đọc trực tiếp; GLACIER Flexible cần restore bất đồng bộ trước khi đọc dữ liệu archive. PUT/UploadPart gặp chunk archive hoặc Restored dùng dữ liệu client để revival theo contract.

## Giới hạn và trạng thái kiểm chứng

- Chỉ hỗ trợ tập API được liệt kê trong API v2; không tuyên bố thay thế toàn bộ Amazon S3. `UploadPartCopy`, ACL/bucket policy S3 và các mục P2 chưa được phê duyệt không thuộc đường sử dụng bắt buộc.
- CopyObject không đọc lại khi cùng domain; copy khác domain tuân theo ranh giới domain và khả năng đọc chunk nguồn. High-level client có thể chọn multipart copy, nên làm theo workaround trong usage guide.
- Local Badger có giới hạn riêng về actor ghi và các route vận hành; dùng shared runtime để kiểm thử chia sẻ ghi/nhiều node.
- Các mục tiêu hiệu năng/dedup/cache là mục tiêu đánh giá, không phải cam kết hoặc kết quả đã đạt. Không gộp simulator, local fixtures, pilot cũ và các bản build khác nhau thành một kết quả AWS.
- Full matrix Cloud/Edge và Cold/Recovery cần evidence theo từng slot; các lượt đang chạy chỉ được kết luận sau review.

## Phát triển và tài liệu

```bash
make check                         # format, vet, tests và build
make fuzz FUZZTIME=10s              # fuzz có thời hạn; dùng khi cần kiểm parser
```

| Đường dẫn | Nội dung |
|---|---|
| `cmd/miles`, `cmd/dsgctl` | Gateway và CLI quản trị |
| `internal/` | API, auth, engine, storage/metadata, cache, coordination, lifecycle, recovery và cost |
| `configs/`, `infra/`, `deploy/` | Config mẫu, Terraform, installer và công cụ vận hành |
| `benchmarks/`, `tests/`, `scripts/` | Dataset, harness, baseline, fixtures và kiểm thử |
| `docs/` | Thiết kế, contract, hướng dẫn và bằng chứng kỹ thuật |

Hợp đồng nguồn: [Kiến trúc v4](docs/dedup-storage-gateway-architecture.md), [API v2](docs/dedup-storage-gateway-api.md). Đánh giá: [benchmark và dataset](benchmarks/README.md), [Direct-S3 baseline](benchmarks/baseline.md), [cost simulator](benchmarks/cost-simulator.md). Quy trình: [hướng dẫn đóng góp](CONTRIBUTING.md).

Không commit credential, token, private key, dữ liệu người dùng hay Terraform state. `backlog/` và `report/` là tài liệu local, không thuộc gói source Git.
