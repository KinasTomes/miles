# Miles

**Multi-tier Intelligent Less-waste Efficient Storage** — cổng lưu trữ tương thích một tập API S3, tối ưu lưu trữ bằng chống trùng lặp, nén, cache và phân tầng.

## Trạng thái

Dự án đã có local process bootstrap: startup config Cloud/Edge, explicit dev fixtures, private health/readiness/metrics và graceful signal shutdown ([hướng dẫn](docs/development/local-runtime.md)). Có hai entrypoint, lệnh build/check và CI. `internal/api/s3` đã có bảng route/operation, bảng lỗi XML và fixture trong `tests/fixtures/s3`. `transport.json` giữ request thật của AWS CLI 2.36.41, boto3 1.43.114 và curl (SigV4, presigned, aws-chunked, trailer, checksum) cùng biến thể lỗi, sinh bằng `scripts/s3capture`. `s3.Router` (http.Handler) xác thực SigV4 header/presigned qua interface `Credentials`, phân quyền bucket qua `Access` (snapshot quyền tối đa 5 giây của `internal/auth`) rồi giao request cho handler theo operation; `s3.Payload` giải aws-chunked, kiểm chữ ký chunk/trailer, độ dài, payload SHA-256, Content-MD5 và checksum trên byte logical; mọi case của `transport.json` chạy qua Router. Router chưa được gắn vào `cmd/miles` và chưa có handler operation nào (request hợp lệ nhận `NotImplemented`). Ma trận S3 của `authorization.json` chạy qua Router trên fake metadata store; `multipart.json` chạy qua `internal/multipart` (input chuẩn hóa, kiểm part head, bảng state của Complete); phần Admin của `authorization.json` và `quota.json` vẫn chỉ kiểm bằng hàm tham chiếu trong test. `internal/api/admin` giữ format bearer, cursor phân trang và precondition revision của Admin API (API v2 6.5), chưa có route. `tests/fixtures/format` giữ golden pack/footer/sidecar và manifest/part/completion pages v1 ([hợp đồng](docs/contracts/pack-and-pages.md)), cũng chỉ được kiểm bằng codec tham chiếu trong test. `tests/fixtures/engine` giữ vectors FastCDC v1, chọn codec, quyết định dedup và action transaction commit ([hợp đồng](docs/contracts/chunking-and-commit.md)), kiểm bằng hàm tham chiếu trong test. Handler S3, dedup, multipart, tiering và recovery chưa được triển khai. Chưa có benchmark hoặc kiểm thử end-to-end storage gateway.

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

`make check` kiểm tra format, chạy `go vet`, `go test`, Python fixture-boundary tests và build hai binary. `go test` hiện chạy fixture route/lỗi/transport/quyền/quota của `internal/api/s3`, snapshot quyền của `internal/auth`, Complete/state của `internal/multipart`, vector bearer/cursor của `internal/api/admin`, golden định dạng pack/pages của `internal/pack` và `internal/manifest`, vectors chunking/nén/commit của `internal/engine`. Chạy `make fmt` để format code.

`make run-dev` mở health server tại127.0.0.1:9100 với in-memory fixtures và simulated dev readiness. Chạy không config mặc định runtime mode, health200/ready503 tới khi real storage/Clock adapters được wiring. S3/Admin ports8080/9000, TLS và command nghiệp vụ chưa triển khai. Xem [startup, health, shutdown và local fixtures](docs/development/local-runtime.md) để biết giới hạn và bàn giao.

Không commit credential, `.env`, dữ liệu local hoặc Terraform state. Đặt cấu hình riêng ở `configs/*.local.*` và dùng secret provider phù hợp khi triển khai AWS.

## Nhóm

| Thành viên | MSSV | Vai trò |
|---|---|---|
| Trịnh Quang Hưng | 23021587 | Nhóm trưởng |
| Nguyễn Minh Chiến | 23021491 | Thành viên |
| Tô Ngọc Hải | 23021543 | Thành viên |
| Đinh Tuấn Phong | 24020266 | Thành viên |
