# Cloud / Edge deployment

W04-HUNG-01 chuẩn bị cấu hình triển khai dùng **cùng release Miles/dsgctl** cho
Cloud và Edge. Không fork engine, không chứa credential, không apply AWS.

**Chưa chạy production:** `mode=runtime` hiện chỉ có health/metrics và trả
`/readyz=503`; chưa compose backend và các listener S3/Admin. Giữ nguyên guard này.
W05-HUNG-01 phải nối runtime thật trước khi bật ASG; nếu bật ngay, ELB health sẽ
thất bại và ASG sẽ liên tục thay node. Xem [handoff](../docs/testing/cloud-edge-deployment-validation.md).

## Release chung

Từ worktree sạch đã commit, Go theo `go.mod`, Python 3.12+, Linux:

```bash
python3 -B deploy/build-release.py --arch arm64 --out "$PRIVATE_DIR/miles-linux-arm64.tar.gz"
```

Chọn `amd64` cho x86_64. Kết quả ghi source commit, toolchain, checksum, kiến trúc;
tar chỉ chứa allowlist, không đóng gói checkout, `.env`, state hay secret. Manifest
ghi SHA-256 từng file; Miles `-version` mang đúng source commit. Hai profile dùng
chính binary đó. Khác kiến trúc cần build lại cùng source.

Đối chiếu SHA-256 archive từ kênh release tin cậy **trước khi giải nén/chạy installer**.
Installer kiểm manifest/kiến trúc, lưu immutable release dưới `/opt/miles/releases/`,
đổi symlink `/opt/miles/current` nguyên tử và giữ config operator trong `/etc/miles`.
Installer chỉ stage, không restart service. Nó từ chối config `mode=dev/local` hay
drain khác 1800 giây. Đường dẫn release/config phải do root quản lý.

Cloud dùng [ASG runbook](cloud/README.md); Edge dùng [Edge runbook](edge/README.md).
Không đặt private key, certificate, token, `.aws` hoặc raw process credential vào Git,
user-data hay Terraform variables/state. `aws-config.example` chỉ chứa provider và
đường dẫn công khai. Edge dùng certificate/private key **riêng cho credential provider**,
không tái sử dụng certificate TLS của Nginx.

## Kiểm tra local / CI

```bash
python3 -B scripts/deployment_smoke.py --out "$PRIVATE_DIR/new-deployment-smoke"
```

Build release native từ source sạch, stage hai profile, so hash binary, chạy process
fixture health/metrics/SIGTERM và Nginx TLS thật với router SigV4 hiện có. Có request
chờ response 65 giây. Cần Docker; fixture tự xóa đúng container của nó. Output nằm
ngoài checkout. Fixture process dùng loopback/port động và drain 2 giây để test;
config triển khai vẫn 1800 giây. Không gọi AWS, không chứng minh R1/R2 hay runtime
production đã sẵn sàng.

Rollback: drain/dừng process theo runbook trước; stage lại archive đã xác minh của
release trước, kiểm config vẫn tương thích, rồi mới start. Cloud pin lại cả SHA-256
và S3 version ID trong launch template, review rolling refresh. Không đổi binary
dưới process đang chạy; không tự rollback dữ liệu/schema.
