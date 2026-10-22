# W02-HUNG-01 — Terraform nền Cloud (dev)

Nguồn: [kiến trúc v4](../../../docs/dedup-storage-gateway-architecture.md) §11/11.1/19, [API v2](../../../docs/dedup-storage-gateway-api.md) §1/8/12. Đây là IaC nền, chưa triển khai Miles, EC2/ASG/bastion, tables hay data bucket. Không chạy workload trên máy làm việc cá nhân.

## Cấu trúc và mặc định

- `bootstrap/state`: stack độc lập cho bucket state riêng, SSE-S3 AES256, versioning, public access block, deny HTTP, `force_destroy=false` và `prevent_destroy`. Không đặt state trong data bucket Miles, không phụ thuộc stack dev để tồn tại.
- `modules/network`: VPC10.0.0.0/16, public10.0.0.0/24 và10.0.1.0/24, private10.0.100.0/24 và10.0.101.0/24 ở hai AZ, IGW/routes/SG. CIDR/AZ qua variables; phải đối chiếu overlap VPN/on-prem trước apply.
- `modules/endpoints`: S3/DDB Gateway Endpoints trên private route tables; Interface Endpoints có private DNS/HTTPS SG khi chọn `interface`.
- `modules/security`: KMS runtime rotation/30day deletion window, Secrets Manager metadata **không có secret version**, SSM String chỉ chứa non-secret bootstrap locations, log group14days.
- `modules/iam`: hai role/profile EC2 gateway và jobs, không key dài hạn; permissions theo inventory. Không tạo deployment role giả khi chưa có trusted principal hợp lệ.
- `modules/alb`: ALB hai public subnets, HTTPS443 → HTTP8080, `/readyz` port9100, Host preservation, idle120s, drain1800s; không rewrite rule, không route9000/metrics ra public.
- `environments/dev`: ghép module, partial S3 backend, variables/examples và lockfile. `account_id` là input bắt buộc, không có default; `123456789012` trong example/test là ID mẫu AWS, không là đích deploy. Terraform **1.15.2**, AWS provider **6.21.0**, lockfile checksum linux_arm64/linux_amd64; pin có chủ đích, không tuyên bố đây là latest.

Mặc định `egress_mode=isolated`, `enable_alb=false`, CIDR client/admin rỗng: không tạo NAT, Interface Endpoints hoặc ALB. Output `aws_api_egress_configured` chỉ cho biết chọn đường mạng cho tám API trong inventory, **không là workload readiness probe** (SSM Session Manager/ECR/external Internet có điều kiện riêng). Đây **không là runtime-ready Cloud**; KMS/SSM/Secrets/CloudWatch chưa có đường từ private subnets. KMS/secret metadata vẫn phát sinh phí nếu apply, nên isolated không có nghĩa toàn stack miễn phí. Secret chưa được populate cũng chặn runtime.

Cập nhật chuẩn bị deploy: [operator/MFA và quyền deploy](bootstrap/operator/README.md),
[kiểm tra role và dự toán dev](dev-preparation.md). Các mục W2 bên dưới mô tả
phạm vi foundation ban đầu; môi trường workload vẫn chưa apply.

## Chạy kiểm tra không tạo tài nguyên

Yêu cầu Terraform đúng version, AWS CLI đã đăng nhập để plan thật. CI dùng mock provider nên không cần AWS credentials.

```bash
terraform fmt -check -recursive infra/terraform/cloud
terraform -chdir=infra/terraform/cloud/bootstrap/state init -backend=false -input=false -lockfile=readonly
terraform -chdir=infra/terraform/cloud/bootstrap/state validate
terraform -chdir=infra/terraform/cloud/environments/dev init -backend=false -input=false -lockfile=readonly
terraform -chdir=infra/terraform/cloud/environments/dev validate
terraform -chdir=infra/terraform/cloud/environments/dev test
python3 infra/terraform/cloud/scripts/plan-local.py --mode isolated
python3 infra/terraform/cloud/scripts/plan-local.py --mode nat
python3 infra/terraform/cloud/scripts/plan-local.py --mode interface
```

`plan-local.py` copy stack vào thư mục temp, đổi backend sang local **chỉ trong bản copy**, lấy account ID từ STS khi chạy và truyền bằng input riêng, export credential tạm vào environment của subprocess (không in/lưu secret), init với lockfile rồi plan thật. JSON xuất ra chỉ address/actions/count; state/plan temp tự dọn. Dùng credential env/profile đã chọn của AWS CLI; không commit output credential, plan JSON đầy đủ hoặc `.terraform/`.

Do chưa có S3 backend thật, đây là **fresh empty-state plan** cho toàn bộ stack, không phải remote-state/drift validation hay bằng chứng apply thành công. Không dùng nó như saved plan triển khai. Không chạy `terraform plan` trực tiếp trong dev trước khi init backend vì backend S3 còn thiếu input/bucket.

Terraform test có tám ca mock, **tất cả command=plan**, gồm mặc định isolated, NAT1/NAT2, eight Interface services, reject ALB thiếu TLS, ALB đúng settings, reject VPC /20 sinh subnet /28 và accept biên /19 sinh /27. [Giới hạn subnet ALB](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/application-load-balancers.html) yêu cầu /27 hoặc lớn hơn và ít nhất 8 IP trống; plan này chưa chứng minh số IP trống tại thời điểm deploy thực tế. Certificate UUID rỗng trong test là fixture giả, không là certificate được cấp hay cấu hình deploy.

## State bootstrap và quy trình triển khai sau review

1. Cung cấp IAM deployment principal/assume-role được cấp quyền phù hợp; không apply bằng root. Đăng nhập/assume deployment role trước, rồi chạy gate bên dưới trước **mỗi** apply (bootstrap và dev). Lượt này chỉ dùng session hiện có cho đọc/plan và không tạo IAM principal thực.

```bash
# DEPLOY_ROLE_ARN lấy từ cấu hình riêng, không ghi ARN thật vào Git.
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
```

Gate gọi STS đọc, reject root/IAM user/sai account/sai role. Phải Pass trên session role dự kiến trước apply; backend/provider phải dùng cùng deployment identity đã kiểm tra, không override sang profile/role khác. Đây là gate vận hành, không phải IAM/SCP chặn root ở cấp account và không thể ngăn operator cố tình bỏ qua. Script không tự gọi apply. Unit tests offline: `python3 -B -m unittest discover -s infra/terraform/cloud/scripts/tests -v`.

2. Operator với quyền phù hợp init/plan/apply **stack bootstrap/state riêng** sau review. Bucket versioned không có expiration tự động. Giữ bootstrap local state trên disk được mã hóa và permission0600 ngoài Git tới khi migrate vào backend được bảo vệ riêng; mất file bootstrap state có thể cần import lại tài nguyên, không tự tạo lại bucket cùng tên.
3. Copy `backend.hcl.example` ra `/tmp/miles-backend.hcl` (không secret), xác nhận tên bucket do bootstrap trả về (thay sample account trong example), rồi init dev:

```bash
terraform -chdir=infra/terraform/cloud/environments/dev init -backend-config=/tmp/miles-backend.hcl -reconfigure
```

4. Backend dùng `encrypt=true`, `use_lockfile=true` (S3 native `.tflock`, không thêm DynamoDB lock table). Quyền tối thiểu **state access** cho deployment principal: ListBucket bound prefix cloud/dev, GetObject/PutObject trên `cloud/dev/terraform.tfstate`; GetObject/PutObject/DeleteObject trên `cloud/dev/terraform.tfstate.tflock`. Không cấp state access cho gateway/jobs. Backend AWS authentication tách provider `assume_role`; nếu dùng role, cấu hình backend `assume_role` tương ứng hoặc export session role trước init. Không nhầm `deploy_role_arn` của provider sẽ tự áp lên backend.
5. Copy tfvars example thành `terraform.tfvars` đã ignore. Xác nhận CIDR/AZ, chọn egress cho runtime, điền certificate ARN và CIDR thực nếu bật ALB, populate runtime secret bằng provisioning ngoài Terraform. Không đưa bearer/SigV4 key vào tfvars/SSM String/state. Invariants SystemConfig/quyền vẫn trong metadata DSG, không do Terraform quản lý.
6. Plan remote state, review inventory/policy/cost, sau đó mới apply theo authorization triển khai thực. Không dùng `-target` để lách TLS/egress constraints. Không deploy app/ASG ở task W2 này.

Permission bootstrap/config chưa được kiểm bằng apply. Deployment role cần quyền quản lý đúng VPC/endpoints/ALB/IAM profiles/KMS/secrets metadata/SSM/log group/bucket state trong account và `iam:PassRole` chỉ roles Miles với `iam:PassedToService=ec2.amazonaws.com` khi launch ở task sau; **không cấp AdministratorAccess hoặc PassRole * để né lỗi**. Trust principal/permissions boundary cần operator xác định trước khi tạo role. KMS key policy cho account delegate qua IAM; runtime chỉ được use key, không key admin. Kiểm IAM Access Analyzer và permissions boundary/SCP trước apply khi có deploy principal.

## ACM là gì và phần đang chờ

**ACM (AWS Certificate Manager)** quản lý chứng chỉ TLS để trình duyệt/SDK kiểm tra HTTPS. ALB cần certificate **ISSUED ở Singapore, cùng account**, tên certificate khớp hostname client dùng. Thông thường nhóm cần domain mình kiểm soát, request certificate trong ACM, thêm DNS validation record tại nhà cung cấp domain rồi truyền ARN vào `certificate_arn`. Đây là input triển khai, không tạo certificate giả để đạt checklist.

Chưa có domain/certificate hoặc quyền DNS nên không tạo ACM certificate/DNS record trong task này; module nhận certificate ARN có sẵn, `enable_alb=false` mặc định. Bật ALB mà không có ARN/CIDR sẽ fail plan. ARN đúng cú pháp chưa chứng minh cert ISSUED/hostname đúng; phải kiểm `aws acm describe-certificate` trước apply và HTTPS client sau deploy. Không thay HTTPS bằng public HTTP listener.

Xem [inventory và quyết định chi phí](inventory-and-costs.md), [bằng chứng validation](validation.md). Không cắt R1–R4, không đổi scope Cloud/Edge, giữ các rules review hiện có. CI job Terraform mới được khai báo; kết quả run thực tế nằm trong Checks của PR. Required status check Terraform được cấu hình sau khi check đã chạy thành công.
