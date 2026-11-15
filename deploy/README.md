# Cloud / Edge deployment

Cloud và Edge dùng **cùng release Miles/dsgctl**, với cấu hình, mạng và credential
provider phù hợp từng profile. Shared runtime đã nối DynamoDB, S3, R1/R2,
S3/Admin listeners và jobs; cấu hình đầy đủ nằm trong
[shared runtime](../docs/development/shared-runtime.md).

Phân biệt hai loại config: `configs/shared-*.example.json` mô tả runtime đầy đủ;
`configs/cloud.example.json` và `configs/edge.example.json` là skeleton mà installer
stage ban đầu. Chỉ đặt `mode=runtime` không làm một skeleton trở thành gateway
đã provision. Thiếu backend/history/secret/policy hoặc clock proof vẫn phải
unready/fail startup; không sửa readiness để vượt health check.

## Chọn đường triển khai

| Nhu cầu | Điểm bắt đầu |
|---|---|
| Một installation dev/test mới, Cloud và Edge cùng trên AWS | [Hướng dẫn installation từng bước](../docs/testing/aws-personal-account-guide.md) |
| Chỉ demo Cloud, dùng S3/DynamoDB thật | [Chuẩn bị Cloud demo](../docs/planning/cloud-demo-deployment.md); chưa áp dụng cấu hình mẫu trực tiếp |
| Cloud fleet/ASG đã có shared backend | [Cloud fleet](cloud/README.md) |
| Edge trên VM hoặc máy local có backend AWS | [Edge deployment](edge/README.md) |
| Fixture phát triển không dùng AWS | [Local runtime](../docs/development/local-runtime.md) |

## Trình tự installation trên AWS

Đường này triển khai **hai Cloud node và một Edge EC2**, không phải chỉ Cloud.
CLI hiện tại là `infra/terraform/cloud/scripts/deploy-installation.py`; từng
subcommand độc lập, không tự chạy toàn bộ lifecycle. Lấy command wrapper, file
JSON private, prerequisites và login từ [installation guide](../docs/testing/aws-personal-account-guide.md).

| Bước | Lệnh trong wrapper `deploy` | Điều kiện để đi tiếp |
|---|---|---|
| Chuẩn bị offline | `prepare` | Source commit sạch; config/namespace và private directory mới; chưa tạo AWS resource |
| IAM/backend cấp sẵn (`existing`) | `verify-bootstrap` | Đúng role/account, tags, boundary, state bucket/key; không plan/apply `operator` hoặc `state` |
| IAM/backend mới (`new`) | `plan operator`, `apply operator`; `plan state`, `apply state` | IAM plan được review; apply operator bằng phiên IAM, workload bằng deploy role |
| Shared storage và network | `plan foundation`, `apply foundation` | Saved plan đúng installation; backend rỗng hoặc đúng lineage của lượt đang resume |
| Provision và publish | `provision` | Runtime dừng; bootstrap lần đầu; giữ token/intent nếu kết quả chưa xác định; pin release SHA và S3 VersionId |
| Bật runtime | `plan active`, `apply active` | IAM/AMI/certificate/network và paid window đã kiểm; plan chỉ chứa resource được phép |
| Smoke | `verify` | Hai Cloud target healthy; Cloud/Edge PUT→GET SHA-256 khớp qua TLS thật |
| Dừng/thu hồi | `plan paused` hoặc quy trình full teardown | Quiesce/drain đúng invocation; giữ state/evidence; native inventory xác nhận phần đã xóa hoặc còn giữ |

Mỗi `apply` cần `--plan-sha256` của saved plan tương ứng vừa review; không dùng
hash mẫu hoặc plan của bước trước. `prepare` không cần phiên AWS. Các bước còn
lại có thể tạo chi phí hoặc thay đổi tài nguyên theo lệnh; không lấy config mẫu
làm quyền tự apply. Chi tiết pause, unprotect, destroy và `closeout` nằm trong
installation guide và [full teardown](../infra/terraform/cloud/full-teardown.md).

Với account dùng chung, chọn installation/backend/role theo quyền đã cấp; không
adopt state của lượt cũ hoặc đổi tag tài nguyên người khác. Trên máy SSH, in đầy
đủ URL `aws login --remote` để mở trên máy cá nhân. Giữ riêng
`iam-login-code.txt` cho phiên IAM và `cli-login-code.txt` cho deploy role;
file `0600`, không gửi code qua chat/log và xóa nội dung sau khi dùng.

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
Installer chỉ stage, không provision backend hoặc restart service. Nó từ chối config `mode=dev/local` hay
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
