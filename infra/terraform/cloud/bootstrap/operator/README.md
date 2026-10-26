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
| MilesDevDeployData | deploy-data.json.tmpl | deploy role (review draft) |
| MilesDevDeployCompute | deploy-compute.json.tmpl | deploy role (review draft) |
| MilesDevDeploySessions | deploy-sessions.json.tmpl | deploy role (review draft) |
| MilesDevDeployTeardown | deploy-teardown.json.tmpl | deploy role (review draft) |
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

Baseline foundation/state và full Warm sinh từ private empty-state plans bằng
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
python3 infra/terraform/cloud/scripts/plan-local.py --mode nat --var-file "$DEV_VARS" \
  --policy-plan-json "$PRIVATE_DIR/warm-active-plan.json"
uvx iam-policy-autopilot@0.3.0 generate-policies "$PRIVATE_DIR/warm-active-plan.json" \
  --region ap-southeast-1 --account "$AWS_ACCOUNT_ID" --resource-cutoff 100 --pretty
```

Baseline được review lại: bỏ quyền service tùy chọn không dùng, sửa IAM path,
KMS alias và SSM parameter ARN; thêm giới hạn tags/Region, runtime boundary và
state/lock access theo hợp đồng backend. Boundary lấy trần quyền của module IAM
runtime hiện có. Tách managed policy để từng document dưới quota 6.144 ký tự.

Render `ACCOUNT_ID` thành account từ STS và `AMI_ID` thành AMI đã pin/review trong thư mục riêng có quyền 0700,
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
   chọn phiên role đó trong trình duyệt. Không chọn user hoặc root. Đặt region
   Singapore trên profile nguồn để refresh token không gặp NoRegion.
3. Đặt `AWS_PROFILE=miles-dev`, kiểm STS và chạy identity gate trước mỗi apply:

```bash
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
```

Backend và provider phải dùng cùng identity. Khi CLI đã là deploy role, giữ
`deploy_role_arn=null` để tránh tự AssumeRole lần nữa. Provider 6.21.0 chưa hỗ trợ
credential provider `aws login`; `plan-local.py` đã export credential tạm trong
subprocess. Với lệnh Terraform triển khai trực tiếp, dùng cùng cách export vào
process environment; không in hoặc lưu raw credential vào shell history/file.

## Mở rộng W3 — chưa cập nhật AWS

Các template data/compute/session/teardown và boundary mới đã validate/simulate,
chưa tạo managed policy hoặc đổi default version trên AWS trong lượt chỉ plan/test.
Trước apply cần administrative IAM review, đối chiếu version đang gắn và cập nhật
đúng document; không để deploy role tự đổi policy/boundary. Không overwrite policy
khác nội dung mà chưa review diff. Giữ trust/MFA/user policy như bootstrap hiện có.

Bốn policy mới cộng policy hiện có và SignInLocalDevelopmentAccess là 10 managed
policies/role; quota đọc từ account hiện là 20. Boundary không gắn như
identity policy vào deploy role. Kiểm quota/attachment count thực trước cập nhật.

Compute chỉ cho pinned AMI, t4g.small, IMDSv2, private ENI và profile gateway đã
bounded; volume mã hóa gp3 ≤20 GiB. CreateTags chỉ lúc RunInstances/CreateVolume,
không được tự gắn nhãn nhận instance có sẵn. Data policy giới hạn 20 bảng thật và
đúng backend bucket; không cấp object access/data mutation cho deploy chỉ để smoke
test. Chạy smoke dưới runtime profile trên dev node sau khi apply được duyệt.

Teardown thường ngày chỉ terminate instance, detach volume và bỏ NAT/EIP/routes,
interface SG/endpoints hoặc ALB đúng tên. Không DeleteBucket/DeleteTable/DeleteVolume,
ScheduleKeyDeletion hoặc xóa state. `prevent_destroy`/deletion protection vẫn cần;
IAM permissions không thay proof graceful drain/retention trong app.

Session access chỉ node Miles/dev và document `miles-dev-shell`; kết thúc/resume
session dùng system tag khớp `${aws:userid}` theo
[AWS Session Manager IAM examples](https://docs.aws.amazon.com/systems-manager/latest/userguide/getting-started-restrict-access-examples.html).
Agent gateway có UpdateInstanceInformation và bốn ssmmessages channel actions;
jobs không nhận các action này trong inline policy. Không cấp SendCommand hoặc
StartSession cho runtime. KMS grant chỉ cho AWSResource trên key Miles/dev;
CloudWatch key policy giới hạn đúng runtime log group bằng encryption context.

ALB disabled trong Warm plan hiện tại; template teardown chỉ cho cleanup ALB, chưa
cấp CreateLoadBalancer/listener/target group. Trước bật ALB phải generate/review
policy bổ sung từ plan certificate thật. Không dùng AdministratorAccess để né lỗi.

Quyền bootstrap/data/compute/NAT/session đã được kiểm bằng API AWS thật ngày
25/10/2026; xem [bằng chứng và giới hạn](../../../../../docs/testing/aws-dev-readiness.md).
Interface mode, ALB và full data/key teardown chưa được nghiệm thu live. Simulation không thay đánh giá key policy, SCP hoặc context
thực. Các API thiếu phải bổ sung theo lỗi đã xác định và validate lại, không nới
CreateTags/TagResource để nhận tài nguyên ngoài phạm vi. Xem
[quy trình vận hành](../../dev-sessions.md).

## Audit trước full teardown

`teardown-audit.json.tmpl` là grant tạm chỉ đọc inventory cần cho
[full teardown](../../full-teardown.md); không gắn vào role thường ngày. Render
`ACCOUNT_ID` và `EXPIRES_AT` (UTC ISO 8601, thời điểm kết thúc cửa sổ audit), review
và validate/simulate trước khi gắn bằng phiên quản trị IAM. Grant cho phép list
versions/multipart của đúng bucket dev, DescribeTable/Scan 20 bảng dev và đọc secret version
metadata. `ListBackups`/`DescribeSnapshots` cần resource `*` cho metadata inventory,
giới hạn Singapore và hạn dùng; không cấp đọc dữ liệu ngoài datastore dev.

Gắn bằng inline policy riêng `MilesDevTeardownAuditTemporary`; không overwrite
policy cùng tên chưa được đối chiếu. Chạy Scan với `--consistent-read --select COUNT`,
phân trang đầy đủ; không in item hoặc secret value. DateLessThan giới hạn hiệu lực,
vẫn phải gỡ policy bằng `iam:DeleteRolePolicy` sau audit/cleanup. Grant này không
cấp quyền xóa object/datastore, gỡ bảo vệ, xóa key hoặc cập nhật IAM.

DescribeTable giới hạn ARN và hạn dùng, không yêu cầu resource tags: delete waiter
và refresh phải nhận được ResourceNotFound khi bảng đã mất. Policy thường ngày có
điều kiện tags sẽ trả AccessDenied trong tình huống đó. Giữ grant audit đến khi
đã kiểm API và state sau destroy, rồi gỡ cùng quyền cleanup tạm.
