# Miles

**Multi-tier Intelligent Less-waste Efficient Storage** — cổng lưu trữ tương thích một tập API S3, tối ưu lưu trữ bằng chống trùng lặp, nén, cache và phân tầng.

## Trạng thái

Dự án đang ở bước khởi tạo. Có cấu trúc module, hai entrypoint, lệnh build/check và CI. `internal/api/s3` đã có bảng route/operation, bảng lỗi XML và fixture trong `tests/fixtures/s3`. `transport.json` giữ request thật của AWS CLI 2.36.41, boto3 1.43.114 và curl (SigV4, presigned, aws-chunked, trailer, checksum) cùng biến thể lỗi, sinh bằng `scripts/s3capture`; parser SigV4/aws-chunked chưa có. Handler S3, dedup, multipart, tiering và recovery chưa được triển khai. Chưa có kết quả benchmark hoặc kiểm thử tính năng.

Gateway và `dsgctl` dùng **Go 1.27.2** trong một module `github.com/KinasTomes/miles`. Backend dự kiến: AWS SDK for Go v2, BadgerDB cho dev một node, DynamoDB cho metadata dùng chung, S3 cho pack, FastCDC và zstd. Các dependency sẽ được chọn/pin khi triển khai module tương ứng; skeleton hiện chỉ dùng thư viện chuẩn Go.

## Tài liệu thiết kế

- [Kiến trúc v4](docs/dedup-storage-gateway-architecture.md).
- [API v2](docs/dedup-storage-gateway-api.md).

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

Cài Go 1.27.2 và GNU Make. Phiên bản được pin trong `go.mod`; CI đọc cùng file.

```bash
make build
./bin/miles -version
./bin/dsgctl -version
make check
```

`make check` kiểm tra format, chạy `go vet`, `go test` và build hai binary. `go test` hiện chạy fixture route/lỗi/transport của `internal/api/s3`. Chạy `make fmt` để format code.

Chưa có server/port lắng nghe hoặc command nghiệp vụ trong skeleton. Các cổng 8080/9000/9100 và TLS của Cloud/Edge sẽ được triển khai theo API/kiến trúc.

Không commit credential, `.env`, dữ liệu local hoặc Terraform state. Đặt cấu hình riêng ở `configs/*.local.*` và dùng secret provider phù hợp khi triển khai AWS.

## Nhóm

| Thành viên | MSSV | Vai trò |
|---|---|---|
| Trịnh Quang Hưng | 23021587 | Nhóm trưởng |
| Nguyễn Minh Chiến | 23021491 | Thành viên |
| Tô Ngọc Hải | 23021543 | Thành viên |
| Đinh Tuấn Phong | 24020266 | Thành viên |
