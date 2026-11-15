# Chuẩn bị demo Cloud với S3 thật

Phạm vi hiện tại là **chuẩn bị offline**, chưa tạo hoặc bật lại tài nguyên AWS.
Chỉ dựng sau khi lượt cleanup trước đã kết thúc và cấu hình, saved plan, thời
gian giữ môi trường cùng envelope của đợt demo đã được duyệt. Không tái sử dụng
token, backend lineage hoặc private deployment state của lượt đã destroy.

Mục tiêu là có ứng dụng Miles chạy được qua HTTPS và đối chiếu dữ liệu trên
S3 Console. Đây không phải một đợt benchmark hoặc nghiệm thu lại toàn ma trận.

## Cấu hình đích

| Thành phần | Cấu hình demo |
|---|---|
| Gateway | Cloud profile, hai node ASG ở hai AZ, cùng source/release |
| Client | Máy operator đã có, được allowlist tới ALB; không tạo EC2 client benchmark |
| Front door | ALB HTTPS, giữ signed Host/path; client xác minh certificate/CA |
| Metadata | DynamoDB dùng chung, 20 bảng theo installation |
| Payload | Bucket S3 versioned và mã hóa theo cấu hình runtime; pack và sidecar thật |
| Quản trị | Admin loopback qua SSH/SSM tunnel đã xác minh; không public Admin/metrics |
| Loại khỏi demo này | Edge VM, acceptance client/telemetry nodes, Direct-S3 baseline, Cold transition/restore và fault injection |

Giữ Cloud architecture hiện có, không đổi thành Badger một node hoặc proxy local
rồi gọi đó là Cloud. Hành vi Edge và Cold được trình bày từ tài liệu/evidence
đã review, không cần tạo thêm tài nguyên chỉ để có giao diện demo.

## Một JSON để chuẩn bị inputs

Copy [cloud-demo.example.json](../../infra/terraform/cloud/environments/dev/cloud-demo.example.json)
ra thư mục private. Account/ARN/AMI/IP và ngân sách trong example là **fixture**,
không phải tài nguyên đã cấp hoặc giá AWS dự kiến. Thay theo bàn giao thực tế:

- Account, Region/AZ, AMI/type/architecture và CIDR của máy thực sự chạy client.
- Installation/environment/owner riêng; IAM path, boundary và deploy role được
  phép quản lý chính installation đó. Không đổi tên rồi dùng role cũ nếu scope
  hoặc tags không khớp.
- `bootstrap_mode=existing` cho IAM/backend đã cấp. Nếu cần bootstrap mới, chọn
  `new` theo [installation guide](../testing/aws-personal-account-guide.md);
  renderer không tự cấp quyền AWS.
- Với backend cấp sẵn khác quy ước, điền `operator.state_bucket` và `state_key`
  theo bàn giao. Không trỏ vào state workload vừa teardown hoặc còn đang cleanup.
- `operator.utc_reference` khớp chrony của operator. Chốt envelope và thời hạn
  thực tế; các số mẫu không phải approval, timer tự tắt AWS hoặc hard billing cap.

Chạy từ repo có source đã pin; chỉ dùng renderer offline:

```bash
umask 077
MILES_DEMO_PRIVATE="$HOME/.local/state/miles/cloud-demo-preparation"
mkdir -p "$MILES_DEMO_PRIVATE"
chmod 700 "$MILES_DEMO_PRIVATE"
cp infra/terraform/cloud/environments/dev/cloud-demo.example.json \
  "$MILES_DEMO_PRIVATE/installation.json"
# Điền/kiểm installation.json trước. Dùng output directory mới mỗi lần render.
python3 -B infra/terraform/cloud/scripts/render-installation.py \
  --config "$MILES_DEMO_PRIVATE/installation.json" \
  --out "$MILES_DEMO_PRIVATE/rendered"
```

Renderer tạo private Terraform inputs/backend/IAM proposals, không gọi AWS.
`paused=true` giữ fleet chưa phục vụ; chưa có release version/SHA, certificate
ARN hay shared bootstrap. Output này chưa đủ để apply một demo đang hoạt động.

## Ranh giới giữa Terraform và bộ dựng installation

Terraform hỗ trợ Cloud-only qua `enable_edge=false`, `enable_acceptance_nodes=false`,
`enable_asg=true` và `enable_alb=true`. Sample JSON trên dành cho renderer và
đường [Cloud fleet](../../deploy/cloud/README.md).

**Không chạy `deploy-installation.py provision/plan active` với giả định nó giữ
Cloud-only.** Bộ dựng hiện sinh `active.tfvars.json` với `enable_edge=true`, và
`verify` của nó kiểm cả Cloud lẫn Edge. `prepare` cũng sinh foundation inputs
tắt serving flags; nó không phải bản active Cloud-only của JSON này.

Trước thực thi, operator chọn đường Terraform fleet với private inputs/backend
và identity/plan gates hiện có, hoặc bổ sung profile selection vào bộ dựng kèm
tests cho provision, verify và cleanup. Không sửa riêng một generated flag để
bỏ qua các kiểm tra và không tuyên bố bộ dựng một-file đã hỗ trợ Cloud-only.
Đây là điểm tích hợp còn cần chốt trước apply, không ngăn chuẩn bị tài liệu/config.

## Các bước khi được giao triển khai

1. **Chốt source và ownership:** commit sạch đã review; cleanup lượt cũ hoàn
   tất hoặc có disposition rõ; backend/key và inventory riêng. Không lấy code
   chưa commit hoặc binary của benchmark cũ làm release mới.
2. **Identity/IAM:** dùng deploy role đúng account/installation, kiểm quyền
   state/lock, runtime boundary và quyền fleet. Template policy không chứng
   minh effective permission; chưa xác định thì không apply. Login SSH dùng
   URL đầy đủ và code file theo [deployment guide](../../deploy/README.md).
3. **Foundation/bootstrap:** tạo storage/network theo saved plan; runtime dừng
   khi bootstrap user/token/history/keys lần đầu. Kết quả chưa xác định giữ
   intent/token để reconcile, không reset metadata hoặc bootstrap lại.
4. **Release/TLS/fleet:** pin source, archive SHA-256 và S3 VersionId; điền ACM
   ARN, runtime references và client routing. Review plan Cloud-only, gồm cả
   IAM/network/retained storage, trước khi bật hai node.
5. **Ứng dụng:** hai target healthy, private readiness tốt; client gọi S3 API
   qua ALB bằng Miles key. Upload một file synthetic, HEAD/LIST, download và
   đối chiếu SHA-256. Upload cùng bytes vào key thứ hai để quan sát dedup.
6. **Đối chiếu S3:** system admin dùng `dsgctl layout <bucket> <key>` và stats
   để tìm pack/generation; mở đúng bucket/pack/sidecar trên Console. Kiểm trước
   quyền Console/IAM/KMS; không nới bucket thành public để xem dữ liệu.
7. **Bàn giao và thu hồi:** ghi source/config/endpoints, cách truy cập credential
   riêng tư và hạn giữ môi trường. Chốt pause hay destroy; drain và xác nhận
   native inventory theo [full teardown](../../infra/terraform/cloud/full-teardown.md).

Bucket/key của client Miles là namespace logical. S3 Console hiển thị pack và
sidecar, không phải một bản file người dùng dưới đúng tên key. Ảnh Console chỉ
minh họa backend; PUT→GET hash match mới chứng minh nội dung qua gateway.
Physical bytes còn gồm journal, sidecar và retained data: không yêu cầu tổng
dung lượng bucket đứng yên sau upload trùng.

Không tắt checksum/TLS/fencing/protection hoặc periodic recovery để có demo
PASS. Dùng dữ liệu synthetic và giữ secrets/token/presigned URLs khỏi ảnh chụp.

## Điều kiện để bàn giao demo đang hoạt động

- Hai Cloud target healthy và chạy đúng binary/config; client truy cập được
  HTTPS bằng hostname/CA thực tế, không dùng `--no-verify-ssl`.
- PUT→GET SHA-256 khớp; LIST/HEAD trả đúng object; stats/layout của Admin đối
  chiếu được với S3. User thường không được cấp quyền vật lý hoặc system token.
- Có endpoint, CA nếu cần, credential Miles được bàn giao private và lệnh sử
  dụng từ [README](../../README.md)/[usage guide](../api-usage-guide.md).
- Đã ghi thời điểm tắt/cleanup và phần storage/state cần giữ. Chuẩn bị offline,
  plan pass hoặc CI xanh chưa được ghi là một demo AWS đã chạy thành công.
