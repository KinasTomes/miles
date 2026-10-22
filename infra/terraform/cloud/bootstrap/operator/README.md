# Danh tính deploy dev

Bootstrap IAM một lần bằng danh tính quản trị hiện có. Terraform workload chạy bằng
role `miles-dev-deploy`; không apply bằng root. Các template dùng `ACCOUNT_ID`,
không chứa thông tin account thật. Config, plan và credential nằm ngoài Git.

## Operator và role

- User `hung-deploy`, path `/miles/operators/`, đăng nhập console bằng mật khẩu và
  MFA `hung-deploy`. Không tạo access key; user policy còn explicit-deny thao tác này.
- Role `miles-dev-deploy`, path `/miles/bootstrap/`, session tối đa 3.600 giây.
  Trust đúng user trên và yêu cầu `aws:MultiFactorAuthPresent=true`.
- User nhận inline `user-policy.json.tmpl`. User và deploy role nhận managed policy
  AWS `SignInLocalDevelopmentAccess` để dùng `aws login`.
- Bootstrap tạo các managed policy dưới `/miles/bootstrap/`. Deploy role không có
  quyền sửa chính nó, operator hoặc các managed policy bootstrap.

| Managed policy | Template | Gắn vào |
|---|---|---|
| MilesDevDeployNetworkCreate | deploy-network-create.json.tmpl | deploy role |
| MilesDevDeployNetwork | deploy-network.json.tmpl | deploy role |
| MilesDevDeployRuntimeRoles | deploy-runtime-roles.json.tmpl | deploy role |
| MilesDevDeployServices | deploy-services.json.tmpl | deploy role |
| MilesDevDeployState | deploy-state.json.tmpl | deploy role |
| MilesDevRuntimeBoundary | runtime-boundary.json.tmpl | boundary của gateway/jobs |

Boundary không phải identity policy cấp quyền cho deploy role. Nó giới hạn các
quyền runtime dù operator có thể sửa inline policy của hai runtime role.
`iam:CreateRole` chỉ được phép cho đúng hai role `/miles/dev/miles-dev-gateway`
và `/miles/dev/miles-dev-jobs` khi kèm boundary đã duyệt. Không cấp quyền gỡ boundary.
`iam:PassRole` chỉ hai role này, tới EC2.

Network create cần request tags `Project=Miles`, `Environment=dev`; các dependency
đã tồn tại phải có resource tags tương ứng. Quyền tag lúc tạo yêu cầu
`ec2:CreateAction` hiện diện, tránh nhận mạng có sẵn bằng cách tự gắn nhãn Miles.
Network read/write giới hạn Singapore. Backend state chỉ đúng bucket/key dev;
DeleteObject chỉ cho `.tflock`, không cho state object.

## Nguồn policy và kiểm tra

Baseline sinh từ hai private empty-state plans bằng
[iam-policy-autopilot](https://github.com/awslabs/iam-policy-autopilot) **0.3.0**:

```bash
# Thư mục và plan phải nằm ngoài checkout; umask 077 trước khi tạo file.
python3 infra/terraform/cloud/scripts/plan-local.py --mode nat \
  --policy-plan-json "$PRIVATE_DIR/foundation-plan.json"
python3 infra/terraform/cloud/scripts/plan-local.py --stack state \
  --policy-plan-json "$PRIVATE_DIR/state-plan.json"
uvx iam-policy-autopilot@0.3.0 generate-policies "$PRIVATE_DIR/foundation-plan.json" \
  --region ap-southeast-1 --account "$AWS_ACCOUNT_ID" --resource-cutoff 100 --pretty
uvx iam-policy-autopilot@0.3.0 generate-policies "$PRIVATE_DIR/state-plan.json" \
  --region ap-southeast-1 --account "$AWS_ACCOUNT_ID" --resource-cutoff 100 --pretty
```

Baseline được review lại: bỏ quyền service tùy chọn không dùng, sửa IAM path,
KMS alias và SSM parameter ARN; thêm giới hạn tags/Region, runtime boundary và
state/lock access theo hợp đồng backend. Boundary lấy trần quyền của module IAM
runtime hiện có. Tách managed policy để từng document dưới quota 6.144 ký tự.

Render `ACCOUNT_ID` thành account từ STS trong thư mục riêng có quyền 0700,
file 0600. Chạy `aws accessanalyzer validate-policy` với `IDENTITY_POLICY` trên
các policy; trust dùng `RESOURCE_POLICY` và resource type
`AWS::IAM::AssumeRolePolicyDocument`. Chỉ gắn document đã kiểm tra; nếu policy
cùng tên đã tồn tại nhưng khác nội dung thì dừng để review thay vì ghi đè.

Ngày 2026-10-22: Access Analyzer không có findings cho các document. IAM simulator
đã kiểm tra ca cho phép/từ chối: role có/thiếu boundary; sửa deploy role; PassRole
tới EC2/Lambda; gắn nhãn chiếm VPC chưa thuộc Miles; tạo subnet trong VPC ngoài
phạm vi; sửa VPC chưa có owner tags; xóa state object. Simulation và plan không
chứng minh mọi API apply đã được cấp đủ quyền.

## Phiên làm việc thường ngày

1. Đăng nhập console bằng operator và MFA, rồi Switch Role với đường dẫn đầy đủ
   `miles/bootstrap/miles-dev-deploy`.
2. Đăng nhập CLI bằng `aws login --remote --profile miles-dev --region ap-southeast-1`,
   chọn phiên role đó trong trình duyệt. Không chọn user hoặc root.
3. Đặt `AWS_PROFILE=miles-dev`, kiểm STS và chạy identity gate trước mỗi apply:

```bash
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
```

Backend và provider phải dùng cùng identity. Khi CLI đã là deploy role, giữ
`deploy_role_arn=null` để tránh tự AssumeRole lần nữa. Provider 6.21.0 chưa hỗ trợ
credential provider `aws login`; `plan-local.py` đã export credential tạm trong
subprocess. Với lệnh Terraform triển khai trực tiếp, dùng cùng cách export vào
process environment; không in hoặc lưu raw credential vào shell history/file.

Các policy này phục vụ foundation bootstrap đã plan, chưa cấp quyền triển khai
EC2/data bucket/DynamoDB hoặc ALB. Chưa đủ quy trình teardown: cần review quyền
xóa tài nguyên mạng, bảo vệ data/KMS và plan tắt NAT trước khi bật hạ tầng tính phí.
Không dùng AdministratorAccess để xử lý lỗi quyền và không coi template này là
quyền deploy hoàn chỉnh của toàn bộ W3.
