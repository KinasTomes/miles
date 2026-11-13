# IAM cho từng installation

IAM bootstrap do chủ account hoặc danh tính quản trị IAM thực hiện. Workload chạy
bằng deploy role đã kiểm STS/identity gate. Runtime dùng instance profile hoặc
Roles Anywhere. Thư mục này chứa policy templates cần render và review trước khi gắn.

Operator user chỉ assume được deploy role đã khai báo và phải dùng MFA; deploy role
không assume tiếp runtime/role khác. Policy isolation chặn nhận tag từ installation
khác, đổi/xóa các nhãn sở hữu và thao tác regional ngoài Region đã chọn. Không nới
các deny này khi gặp `AccessDenied`: kiểm tra đúng input và tài nguyên trước.
Các API inventory dùng resource `*` vẫn có thể nhìn thấy metadata chung của account;
installation riêng không tạo account/quota/billing riêng.

## Một file cấu hình cho mỗi installation

`bootstrap_mode` trong cùng JSON chọn `new` (mặc định) hoặc `existing`.
`new` tạo IAM/state qua hai bootstrap stack. `existing` dùng IAM/backend đã cấp,
không import, không plan/apply hoặc xóa bootstrap. Member điền config trên máy
của mình, không phụ thuộc file private trên máy lead. Xem [luồng dùng tài nguyên cấp sẵn](../../../../../docs/testing/aws-personal-account-guide.md#11-dùng-iam-và-backend-đã-cấp-sẵn-trên-máy-member).

Ở `existing`, renderer chỉ xuất workload tfvars, `backend.hcl`,
`bootstrap-inventory.json` và `iam/deploy-bootstrap-read.json`. Policy đọc này
phải được chủ account duyệt/cấp trước nếu role còn thiếu; bộ dựng không tự attach.
Role deploy cần explicit Deny `s3:DeleteObject` trên đúng state key;
`implicitDeny` không đủ để chặn Allow từ bucket policy. Giữ Get/Put state và
Get/Put/Delete `.tflock`. Chế độ `new` sinh deny này trong `deploy-state.json`;
với `existing`, chủ account cấp trước, bộ dựng chỉ kiểm tra.
Chạy `prepare` → đăng nhập deploy role → `verify-bootstrap` → foundation rồi
dùng chung provision/active/verify/cleanup. Backend bucket/key đã cấp được điền
qua `operator.state_bucket`/`state_key`; không cần đổi defaults trong source.
Không dùng chế độ này để nhận state workload cũ hoặc bỏ qua review IAM/plan.

Copy [installation example](../../environments/dev/installation.example.json) ra
thư mục private ngoài Git. Điền account/Region, `installation_name`, `environment`,
`owner`, `runtime_iam_path`, boundary ARN, AMI/type, Edge certificate CN và operator.
Các ARN user/MFA/deploy role/boundary phải thuộc cùng account. `operator.terraform_profile`
là profile phụ đọc CLI login qua credential_process.

```bash
umask 077
python3 infra/terraform/cloud/scripts/render-installation.py \
  --config "$PRIVATE_DIR/installation.json" --out "$PRIVATE_DIR/rendered"
```

Output directory phải mới, nằm ngoài checkout. Renderer dùng standard library,
không gọi AWS, tạo directory 0700/file 0600 và từ chối trường cấu hình lạ.
Mỗi lần đổi input tạo output directory mới để review diff trước khi sử dụng.

Khi bật `enable_acceptance_nodes`, renderer thêm đúng hai runtime identity/profile
`<installation>-acceptance-edge` và `-acceptance-client`, vẫn buộc permissions
boundary, path và tag của installation. Quyền asset chỉ tới `acceptance-assets/`
trong bucket riêng. Khi tắt, các grant bổ sung này không được sinh.

Bảng dưới liệt kê output của chế độ `new`.

| Output | Cách dùng |
| --- | --- |
| `installation.tfvars.json` | Private var-file cho dev stack, có thể dùng foundation hoặc fleet theo input |
| `bootstrap-state.tfvars.json` | Var-file cho bootstrap state |
| `backend.hcl` | Backend bucket/key/Region/profile cùng installation |
| `bootstrap-inventory.json` | Binding account, IAM, tags và backend để đối chiếu private |
| `iam/user-policy.json`, `iam/trust-policy.json` | Quyền operator và trust deploy role |
| `iam/runtime-boundary.json` | Permissions boundary của runtime roles |
| `iam/deploy-*.json` | Baseline deploy permissions, cần đối chiếu plan thật |

Templates dùng các marker `{{ACCOUNT_ID}}`, `{{REGION}}`, `{{INSTALLATION_NAME}}`,
`{{ENVIRONMENT}}`, `{{RUNTIME_IAM_PATH}}` và ARN từ operator config. Renderer giữ
state DeleteObject chỉ trên `.tflock`. Runtime role creation bắt buộc boundary đúng
ARN đã chọn. PassRole tới EC2 chỉ cho gateway/jobs và Edge bootstrap role. Edge có cùng bounded role-management
contract, provider dùng trust anchor và certificate CN cấu hình trong Terraform.

State bucket là `<installation>-tfstate-<account>-<region>`, data bucket là
`<installation>-data-<account>-<region>`, table prefix `<installation>-`.
Grant dựa trên tags dùng cả `Environment` và `Installation`. Khi nâng cấp installation
cũ, review plan gắn tag Installation bằng quyền quản trị đã được duyệt trước khi
gắn các policy mới có điều kiện tag này. State/session outputs mới cũng cần được
review trong một plan riêng trước khi dùng gate pause/resume.

Đổi `installation_name`, Region hoặc IAM path của installation có dữ liệu cần
migration plan riêng vì nhiều tài nguyên sẽ thay tên hoặc bị replace.

## Bộ dựng account mới

`main.tf` tạo user, role MFA, runtime boundary và các deploy managed policies từ
`operator.tfvars.json`. Renderer chia deploy policies trong quota mặc định. Password
Console được bộ dựng tạo ngoài Terraform state. CA/key/material Edge cũng nằm ngoài
Terraform, chỉ ARN/public CA được truyền vào IaC.

Dùng `scripts/deploy-installation.py` theo [hướng dẫn account mới](../../../../../docs/testing/aws-personal-account-guide.md).
`plan operator` và `apply operator --plan-sha256 ...` chạy bằng admin profile sau khi
được chủ account duyệt. Các stage state/foundation/active dùng deploy role.
`operator.cleanup_expires_at` bật grant xóa có hạn tối đa bốn giờ. Xóa trường này và
review/apply operator plan để thu hồi. Giữ operator/state bootstrap sau lượt test.

## Bootstrap danh tính khi thao tác trực tiếp

1. Tạo operator user đúng `operator.user_arn`, Console password và MFA đúng
   `operator.mfa_arn`. Gắn user policy đã render. User policy deny tạo access key.
2. Tạo deploy role đúng `operator.deploy_role_arn`, trust user đã render và yêu cầu
   `aws:MultiFactorAuthPresent=true`. Giữ session duration đã review, baseline 3600s.
3. Gắn AWS `SignInLocalDevelopmentAccess` cho user và deploy role để dùng `aws login`.
4. Tạo boundary từ document đã render ở đúng ARN input, rồi gắn các deploy policy
   đã review. Deploy role giữ tách biệt với quyền sửa chính nó/operator/boundary.
5. Member đăng nhập Console bằng user + MFA, Switch role bằng đầy đủ path, bấm link
   từ `aws login --remote`, chọn deploy role rồi lưu code vào private file cho agent
   hoàn tất login. Xem [hướng dẫn đầy đủ](../../../../../docs/testing/aws-personal-account-guide.md).
6. Kiểm `aws sts get-caller-identity`, dùng profile credential_process cho Terraform
   backend/provider và chạy `check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"`
   trước mỗi apply. Giữ Terraform `deploy_role_arn=null` khi CLI đã là deploy role.

## Đối chiếu quyền với plan

Các templates bao phủ state/foundation, fleet/ALB/SLR, Edge/PrivateLink/Roles Anywhere,
release publisher, metadata bootstrap và cleanup có hạn. Từng account có boundary, key policy, quota và
service availability riêng. Quyền read/update/delete của provider cũng cần kiểm.

Tạo plan JSON private từ plan thật. Empty-state `plan-local.py` chỉ phục vụ phân tích
quyền và không đọc remote state. Nó hỗ trợ private var-file và `--region` override.
Dùng IAM Policy Autopilot để tạo baseline bổ sung, rồi review delta với policy hiện có.

```bash
python3 infra/terraform/cloud/scripts/plan-local.py --mode interface \
  --var-file "$PRIVATE_DIR/rendered/installation.tfvars.json" \
  --policy-plan-json "$PRIVATE_DIR/fleet-plan.json"
uvx iam-policy-autopilot@0.4.0 generate-policies "$PRIVATE_DIR/fleet-plan.json" \
  --region "$AWS_REGION" --account "$AWS_ACCOUNT_ID" --resource-cutoff 100 --pretty
```

Giữ telemetry theo lựa chọn operator, có thể đặt
`DISABLE_IAM_POLICY_AUTOPILOT_TELEMETRY=true`. Plan JSON chứa cấu hình private,
chỉ xử lý trong private directory. Không dùng `--upload-policies` trong bước review.

Đối chiếu baseline với API thực dùng. Loại warm-pool, tagging, PassRole và các service
ngoài scope. Giới hạn ALB/ASG/LT, custom SLR, trust anchor/profile, release prefix và
metadata bootstrap tới installation. Giữ IAM runtime role/boundary scope, tags,
Region và các điều kiện từ chối tài nguyên khác. Validate policy bằng Access Analyzer,
simulate ca allow/deny và review bằng danh tính quản trị trước khi gắn quyền.

Baseline Autopilot 0.4.0 đã chạy trên synthetic Terraform mock plan có fleet/Edge
ở account fixture và Sydney khi triển khai cấu hình này. Mock tests kiểm tên,
ARN, boundary, tags và mặc định tương thích. Tên action trong templates đã được
đối chiếu với [AWS Service Authorization Reference](https://docs.aws.amazon.com/service-authorization/latest/reference/)
ngày 03/11/2026, cùng dependent actions cho certificate/provider creation. Apply/permissions ở account member cần
bằng chứng riêng từ lượt AWS được duyệt.

## Audit và cleanup

`operator.audit_expires_at` là UTC ISO 8601 dạng `2026-12-01T10:00:00Z`. Khi có giá trị
ở tương lai, renderer sinh thêm `iam/teardown-audit.json` với hạn dùng tương ứng.
Grant này chỉ đọc inventory của installation. Gắn/gỡ nó trong cửa sổ audit đã duyệt,
giữ riêng quyền xóa datastore/KMS/secret cho bước cleanup được duyệt.

Giữ bản policy/version ban đầu trước mọi grant tạm. Sau cleanup, gỡ grant và kiểm
quyền đã thu hồi. Chi tiết dữ liệu, retention và bảo vệ ở
[full teardown](../../full-teardown.md), pause/resume ở [dev sessions](../../dev-sessions.md).
