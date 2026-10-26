# Terraform AWS dev Warm

Môi trường dev một node ở Singapore, được chuẩn bị theo W03-HUNG-01. Hạ tầng đã
kiểm trên AWS ngày 25/10/2026 và kết thúc ở pause; DSG chưa triển khai.
Terraform 1.15.2, AWS provider 6.21.0; lockfiles pin
provider cho linux_arm64/linux_amd64. Không thay kiến trúc/API hay triển khai Edge.

Nguồn: kiến trúc §10.1/11/19, API §1/8/13 và
[kế hoạch AWS](../../../docs/planning/aws-environment-and-integration-plan.md).

## Thành phần

- `bootstrap/state`: S3 backend riêng, versioning/encryption/native lockfile,
  prevent_destroy. Account input riêng, không chứa account thật trong Git.
- `bootstrap/operator`: policy template operator MFA, deploy role, runtime boundary;
  bản data/compute/session/teardown đã kiểm trên AWS trong phiên được duyệt ngày 25/10.
- `modules/data`: S3 Standard versioned, SSE-KMS, block public access/ACLs, TLS-only;
  20 bảng DDB on-demand/SSE-KMS/PITR 1 ngày, deletion protection, không TTL/GSI/seed.
  [Mapping vật lý và handoff adapter](metadata-tables.md).
- `modules/compute`: tùy chọn một t4g.small private, IMDSv2, CPU credits standard,
  root disk 8 GiB dùng theo phiên, data EBS gp3 20 GiB giữ lại. Pin AMI AL2023 ARM64
  của Amazon. Không tự format data disk, cài DSG, lưu secret hay mở SSH ra Internet.
- Network: VPC riêng, hai AZ, S3/DDB gateway endpoints. Chọn một NAT cho dev hoặc
  9 interface services (có ssmmessages) × hai AZ. Single NAT không HA và có thể có
  phí cross-AZ. Admin/metrics chỉ private. Session Manager là đường quản trị dev
  không cần bastion/SSH; Cloud/Edge deployment đầy đủ vẫn thuộc các task sau.
- Security/IAM: gateway/jobs instance profiles, KMS/key policy, secret metadata,
  SSM config, log group mã hóa KMS. Không tạo secret value hoặc SystemConfig trong TF.
- ALB vẫn tùy chọn, cần certificate ACM ISSUED cùng account/Region và client CIDRs.
  Bảo toàn Host, TLS 443→8080, health 9100/readyz. Policy deploy hiện chưa cấp quyền
  tạo ALB; phải generate/review policy từ plan có TLS thật trước khi bật nó.

`paused=true` là mặc định an toàn cho **phiên compute/network**, giữ datastore và
keys; vẫn có phí lưu trữ/key nếu apply. `enable_compute=false` mặc định dành cho
foundation plan; cấu hình Warm thật đặt true và giữ nguyên qua mỗi lần nghỉ/bật.
`egress_mode` là lựa chọn khi đang chạy; pause ghi đè thành isolated, tắt ALB và EC2.
Output `aws_api_egress_configured` mô tả route, không chứng minh app/secret/SSM ready.

## Kiểm tra không tạo hạ tầng

```bash
terraform fmt -check -recursive infra/terraform/cloud
terraform -chdir=infra/terraform/cloud/bootstrap/state init -backend=false -input=false -lockfile=readonly
terraform -chdir=infra/terraform/cloud/bootstrap/state validate
terraform -chdir=infra/terraform/cloud/environments/dev init -backend=false -input=false -lockfile=readonly
terraform -chdir=infra/terraform/cloud/environments/dev validate
terraform -chdir=infra/terraform/cloud/environments/dev test
python3 -B -m unittest discover -s infra/terraform/cloud/scripts/tests -v
```

Tất cả Terraform tests dùng mock AWS. Các ca vòng đời dùng mock apply/cleanup để
kiểm giữ data qua đổi graph; không gọi AWS. Certificate/AMI/ARN trong test là fixture,
không là bằng chứng certificate đã được cấp hoặc image thật có sẵn.

Plan thật **read-only** cần CLI đã đăng nhập; script tạo temporary empty-state local
copy và tự dọn, không apply, chỉ in address/action/count:

```bash
python3 infra/terraform/cloud/scripts/plan-local.py --mode nat
python3 infra/terraform/cloud/scripts/plan-local.py --mode nat --var-file "$DEV_VARS"
python3 infra/terraform/cloud/scripts/plan-local.py --mode nat --paused --var-file "$DEV_VARS"
python3 infra/terraform/cloud/scripts/plan-local.py --stack state
```

Có thể dùng `--policy-plan-json "$PRIVATE_DIR/new-plan.json"` cho IAM Autopilot:
đường dẫn tuyệt đối ngoài checkout, file mới 0600; không ghi đè file/symlink. Full
JSON có configuration nhạy cảm: không commit/upload. `--var-file` dùng file riêng;
CLI account/mode/paused override giá trị tương ứng. Script hỗ trợ aws login bằng
credential export trong subprocess, không in/lưu raw credentials.

Fresh local plan không đọc remote state nên không chứng minh drift/permissions
apply, không dùng làm saved plan triển khai. ARN/account mẫu `123456789012` trong
examples là fixture AWS, không là account triển khai. Không apply bằng root.

## Triển khai và thu hồi

Quy trình đầy đủ ở [dev sessions](dev-sessions.md): IAM review → bootstrap state →
initial persistent plan review → resume plan/gate/apply → smoke test → graceful
stop/unmount → pause plan/gate/apply → kiểm orphan/inventory. Scripts gate chỉ đọc,
không gọi apply. Backend/provider phải dùng cùng role session đã kiểm tra.

Không `terraform destroy` toàn bộ để pause: S3/DDB/EBS/KMS/state có bảo vệ. Full
teardown dataset disposable theo [runbook full teardown](full-teardown.md), cần
data/retention review và quyền cleanup riêng.

Xem [readiness và dự toán](dev-preparation.md). [Inventory](inventory-and-costs.md)
và [validation W2](validation.md) là evidence foundation tại ngày ghi trong tài liệu,
không thay thế kết quả W3 hoặc nghiệm thu AWS thật.

[Kết quả kiểm tra AWS dev](../../../docs/testing/aws-dev-readiness.md)
ghi rõ bằng chứng offline/live và các bước còn thiếu trước Done.
