# Miles

**Multi-tier Intelligent Less-waste Efficient Storage** — cổng lưu trữ tương thích một tập API S3, tối ưu lưu trữ bằng chống trùng lặp, nén, cache và phân tầng.

## Trạng thái

Dự án đã có local process bootstrap: startup config Cloud/Edge, explicit dev fixtures, private health/readiness/metrics và graceful signal shutdown ([hướng dẫn](docs/development/local-runtime.md)). Có hai entrypoint, lệnh build/check và CI. `internal/api/s3` đã có bảng route/operation, bảng lỗi XML và fixture trong `tests/fixtures/s3`. `transport.json` giữ request thật của AWS CLI 2.36.41, boto3 1.43.114 và curl (SigV4, presigned, aws-chunked, trailer, checksum) cùng biến thể lỗi, sinh bằng `scripts/s3capture`. `s3.Router` (http.Handler) xác thực SigV4 header/presigned qua interface `Credentials`, phân quyền bucket qua `Access` (snapshot quyền tối đa 5 giây của `internal/auth`) rồi giao request cho handler theo operation; `s3.Payload` giải aws-chunked, kiểm chữ ký chunk/trailer, độ dài, payload SHA-256, Content-MD5 và checksum trên byte logical; mọi case của `transport.json` chạy qua Router. `compat.json` pin ma trận tương thích trước freeze (Unicode/path encoding keys, empty PUT, Range open/suffix/416, HEAD không body, parser XML) với fuzz SigV4/aws-chunked/trailer seeded từ request client đã pin ([suite](docs/testing/s3-compat-suite.md), [ADR-0001](docs/adr/0001-s3-compat-freeze-decisions.md)). Router đã gắn vào `cmd/miles` khi cấu hình `s3_address` trong dev/local mode với ba handler bucket P0 (CreateBucket/HeadBucket/ListBuckets của W03-CHIEN-01, pin trong `buckets.json`); request hợp lệ khác nhận `NotImplemented`. Handler object P0 (PutObject/GetObject/HeadObject kèm Range của W03-CHIEN-02; DeleteObject tombstone và ListObjectsV2 với continuation token ký HMAC của W03-CHIEN-03; service trong `internal/object`) và handler multipart CreateMultipartUpload/UploadPart/ListParts/AbortMultipartUpload (`internal/multipart`, [kiểm thử](docs/testing/multipart-part-validation.md)) cùng CompleteMultipartUpload (freeze, snapshot, receipt, takeover; [kiểm thử](docs/testing/multipart-complete-validation.md)) chạy trong local/benchmark mode. Bucket P0 đã có transaction `CreateBucket` trên MetadataStore (fake + Badger): bucket_id CSPRNG mỗi lần tạo, owner/gán domain bất biến lấy lúc tạo, tạo lại cùng tên không kế thừa grant; ListBuckets chỉ trả bucket owner/grant, system admin không có data access ngầm. Dev provisioning cấp một cặp SigV4 test credential theo format API v2 từ CSPRNG mỗi process, gắn user dev, không log secret (local mode lưu credential riêng với permission 0600 và giữ qua restart). Ma trận S3 của `authorization.json` chạy qua Router trên fake metadata store; `multipart.json` chạy qua `internal/multipart` (input chuẩn hóa, kiểm part head, bảng state của Complete); phần Admin của `authorization.json` chạy qua Admin server thật trên Badger, `quota.json` vẫn chỉ kiểm bằng hàm tham chiếu trong test. `internal/api/admin` phục vụ route Admin danh tính/credential/bearer/grant/policy (API v2 6.2–6.5: users/usage, bucket metadata/grants, policy toàn hệ thống và cache bucket, credentials, admin-tokens) với bearer snapshot ≤5 giây, CAS revision, cursor ký MAC và secret chỉ trả lúc tạo ([kiểm thử](docs/testing/admin-identity-validation.md)). `tests/fixtures/format` giữ golden pack/footer/sidecar và manifest/part/completion pages v1 ([hợp đồng](docs/contracts/pack-and-pages.md)), cũng chỉ được kiểm bằng codec tham chiếu trong test. `tests/fixtures/engine` giữ vectors FastCDC v1, chọn codec, quyết định dedup và action transaction commit ([hợp đồng](docs/contracts/chunking-and-commit.md)), kiểm bằng hàm tham chiếu trong test. Object P0 (PUT/GET/HEAD/Range, DELETE, LIST) đã có handler và service với test trên Badger, pack adapter và read path thật; multipart đủ năm operation P1, còn sweeper `recover_uploads` định kỳ, tiering và recovery chưa được triển khai. Chưa có benchmark hoặc kiểm thử end-to-end storage gateway.

Gateway và `dsgctl` dùng **Go 1.27.2** trong một module `github.com/KinasTomes/miles`. Backend dự kiến: AWS SDK for Go v2, BadgerDB cho dev một node, DynamoDB cho metadata dùng chung, S3 cho pack, FastCDC và zstd. Các dependency sẽ được chọn/pin khi triển khai module tương ứng; bootstrap runtime dùng thư viện chuẩn Go; module đã có JSON Schema validator cho pricing.

## Tài liệu thiết kế

- [Kiến trúc v4](docs/dedup-storage-gateway-architecture.md).
- [API v2](docs/dedup-storage-gateway-api.md).
- [Kế hoạch môi trường AWS, quyền GitHub và tích hợp nhóm](docs/planning/aws-environment-and-integration-plan.md).

Hai tài liệu là hợp đồng thiết kế của Cloud/Edge. Báo cáo học phần trong `report/` chỉ giữ local, đã được `.gitignore` loại khỏi Git.

## Cấu trúc

```text
cmd/
  miles/                  Entry point gateway
  dsgctl/                 Entry point CLI quản trị
internal/
  api/s3/, api/admin/     HTTP API
  auth/, config/, clock/  Quyền, cấu hình, clock thật/mô phỏng
  engine/                 Chunking, hash, lookup, nén
  pack/, manifest/        Định dạng pack/sidecar và manifest
  metadata/               MetadataStore, adapter BadgerDB/DynamoDB
  storage/s3/             PackStore backend S3
  cache/, multipart/      Cache LRU và upload nhiều part
  coordination/           R1: leases, fencing, gate, read pins
  lifecycle/              Tiering, restore, revival, GC/compaction
  recovery/               R2: outbox, journal, seal, checkpoint, rebuild
  jobs/, observability/   Job/node/drain, health/metrics
  cost/, simulator/       Mô hình chi phí và thời gian ảo
infra/terraform/cloud/    EC2/ASG/ALB/VPC endpoints và dịch vụ AWS
deploy/edge/             VM/local, TLS và credential provider
configs/                  Ví dụ cấu hình sẽ bổ sung cùng parser
tests/                   Contract/integration tests và fixtures
benchmarks/               Dataset/harness sẽ bổ sung
scripts/                  Công cụ phát triển; s3capture sinh fixture client
docs/                    Hai tài liệu kiến trúc/API
```

Các thư mục `.gitkeep` là vị trí dành sẵn cho module, chưa chứa implementation. Adapter và interface chi tiết tuân theo hai tài liệu, không xem cấu trúc thư mục là API đã đóng băng.

## Phát triển

Cài Go 1.27.2, GNU Make và Python3. Phiên bản được pin trong `go.mod`; CI đọc cùng file.

```bash
make build
./bin/miles -version
./bin/dsgctl -version
make check
make run-dev
# Docker/Compose + AWS CLI v2: disposable local backend/process fixtures
make test-local
```

`make check` kiểm tra format, chạy `go vet`, `go test`, Python fixture-boundary tests và build hai binary. `go test` hiện chạy fixture route/lỗi/transport/quyền/quota/bucket của `internal/api/s3`, snapshot quyền của `internal/auth`, Complete/state của `internal/multipart`, vector bearer/cursor của `internal/api/admin`, golden định dạng pack/pages của `internal/pack` và `internal/manifest`, vectors chunking/nén/commit của `internal/engine` và contract CreateBucket trên fake lẫn Badger của `internal/storage/contracttest`. `make fuzz` chạy fuzz SigV4/aws-chunked/trailer/XML của parser S3 trong thời gian bounded (mặc định 30 s mỗi target). Chạy `make fmt` để format code.

`make run-dev` mở health server tại127.0.0.1:9100 với in-memory fixtures và simulated dev readiness. Chạy không config mặc định runtime mode, health200/ready503 tới khi real storage/Clock adapters được wiring. Chế độ local trong `configs/local.example.json` nối Badger bền vững, S3 loopback và S3 listener bucket P0 tại8080. `admin_address` mở Admin API loopback trong local/benchmark; `dsgctl admin-token bootstrap` cấp system token offline, rồi `dsgctl` quản lý user/usage/grant/policy/bucket-policy/credentials/admin-token qua bearer. Route stats/layout/packs/jobs/nodes và production TLS/runtime thuộc task sau. Xem [startup, health, shutdown và local fixtures](docs/development/local-runtime.md) để biết giới hạn và bàn giao.

Không commit credential, `.env`, dữ liệu local hoặc Terraform state. Đặt cấu hình riêng ở `configs/*.local.*` và dùng secret provider phù hợp khi triển khai AWS.

## Nhóm

| Thành viên | MSSV | Vai trò |
|---|---|---|
| Trịnh Quang Hưng | 23021587 | Nhóm trưởng |
| Nguyễn Minh Chiến | 23021491 | Thành viên |
| Tô Ngọc Hải | 23021543 | Thành viên |
| Đinh Tuấn Phong | 24020266 | Thành viên |
