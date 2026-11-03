# Dựng Miles trên AWS account cá nhân để kiểm thử

Dành cho member triển khai bằng account AWS và máy operator riêng. Toàn bộ runtime kiểm thử, gồm Cloud và Edge, đặt trên AWS. Máy operator giữ source, công cụ quản trị và evidence.

Tài liệu mô tả quy trình chung, đối chiếu repo ngày 03/11/2026. Khi thực hiện, pin commit cần kiểm thử và đọc các runbook tại chính commit đó. Mỗi lượt có account, state, IAM, ngân sách và thời hạn cleanup riêng.

Bộ công cụ trong repo sinh cấu hình và IAM baseline cho từng installation. Trước deploy đầy đủ, cần hoàn thành IAM delta từ plan thật, cấu hình VM Edge/TLS/network riêng và nghiệm thu trên account đích. Các bước này còn cần operator thực hiện, bằng chứng hiện tại gồm mock/local/CI.

## 1. Chọn phạm vi trước khi dựng

| Nhu cầu | Môi trường | Bằng chứng thu được |
| --- | --- | --- |
| Sửa logic, kiểm tra hồi quy | Unit/race, Badger | Hành vi phần mềm trong test |
| Kiểm tra shared backend và composition | DynamoDB Local + Moto trên máy dev | Transaction, fencing, object path và nhiều composition dùng chung backend |
| Kiểm tra triển khai | AWS thật | IAM, network, TLS, ALB/ASG, credential refresh, CloudWatch và drain trên VM thật |

Chạy `make check` và kiểm thử liên quan trước lượt AWS. Repo có `make test-local` cho fixture local. Các suite native có hướng dẫn riêng trong [shared runtime validation](shared-runtime-validation.md). Ghi cả các test bị SKIP.

Fixture local dùng credential giả, endpoint loopback và dữ liệu tự tạo/xóa. Giữ chúng trong môi trường local. Nghiệm thu AWS chạy qua runtime đã provision với dữ liệu synthetic riêng.

Trước lượt AWS, ghi một bản kế hoạch ngắn gồm các mục sau.

- Commit/PR và tiêu chí cần chứng minh, kèm danh sách ca kiểm thử.
- Account ID, Region, người sở hữu và người thực hiện cleanup.
- Topology, loại máy/RAM/kiến trúc, số VM tối đa khi replacement, đĩa và network.
- Mức chi được duyệt, giờ bắt đầu và hạn hoàn tất cả cleanup theo UTC.
- Dữ liệu được tạo, dữ liệu được xóa, phần cần backup và phần giữ sau lượt chạy.
- IAM cần gắn tạm, thời điểm hết hiệu lực và cách thu hồi.

Người sở hữu account duyệt kế hoạch trước khi gắn quyền hoặc tạo tài nguyên tính phí. Dự toán cần tính cả ALB, endpoints/NAT, EBS, DynamoDB/PITR, S3, KMS, Secrets Manager, logs và data transfer. Kiểm giá, quota và điều kiện Free Tier của account tại thời điểm chạy. Ghi số tiền là envelope dự kiến, cùng cách theo dõi chi phí và thời điểm dừng lượt test. Billing có độ trễ, nên theo dõi thêm resource inventory và thời gian active để chủ động cleanup.

## 2. Chuẩn bị source và thư mục riêng

### Công cụ

- Linux hoặc WSL2 cho máy operator, Git, Bash, Python 3.12+, Make, curl, unzip.
- Go theo `go.mod` và toolchain của commit được chọn. Repo tại thời điểm viết dùng Go 1.27.2. Race test cần C compiler.
- Docker Engine + Compose v2 nếu chạy fixture local.
- AWS CLI v2 từ 2.32.0 để dùng `aws login`.
- Terraform và provider đúng lockfile. Repo tại thời điểm viết pin Terraform 1.15.2, AWS provider 6.21.0.
- Host chạy Miles cần systemd và chrony hợp lệ. Host Edge cần thêm Nginx và helper IAM Roles Anywhere đã pin/verify.

Kiểm phiên bản AWS CLI trước khi đăng nhập. [AWS CLI sign-in](https://docs.aws.amazon.com/signin/latest/userguide/command-line-sign-in.html)

Các lệnh mẫu dưới dùng Bash trên máy operator. Thay giá trị `REPLACE_...` trước khi chạy.

```bash
umask 077
export MILES_WORK="$HOME/miles-aws-test"
export MILES_REPO="$MILES_WORK/repo"
export MILES_PRIVATE="$MILES_WORK/private"
export MILES_EVIDENCE="$MILES_WORK/evidence"
export MILES_COMMIT="REPLACE_WITH_FULL_COMMIT_SHA"
export MILES_ACCOUNT_ID="REPLACE_WITH_12_DIGIT_ACCOUNT_ID"
export MILES_REGION="ap-southeast-1"

mkdir -p "$MILES_PRIVATE" "$MILES_EVIDENCE"
chmod 700 "$MILES_PRIVATE" "$MILES_EVIDENCE"
git clone https://github.com/KinasTomes/miles.git "$MILES_REPO"
cd "$MILES_REPO"
git fetch origin
# Nếu commit thuộc PR chưa merge, fetch thêm refs/pull/<số-PR>/head.
git checkout --detach "$MILES_COMMIT"
git rev-parse HEAD
go mod download
make check
```

Chọn thư mục mới nếu đường dẫn đã có công việc cần giữ. Ghi SHA đầy đủ và kết quả test vào evidence. Dùng checkout sạch đã commit để build release.

Private directory chứa login code, tfvars, plan/state, certificate/key và token. Giữ ngoài Git, file 0600. Evidence đưa vào PR cần được rà và che secret trước khi upload.

## 3. Cấu hình installation và bootstrap IAM

Copy [installation example](../../infra/terraform/cloud/environments/dev/installation.example.json)
ra `$MILES_PRIVATE/installation.json`. Điền một bộ giá trị cho installation của member.

- `account_id`, `region`, hai AZ thực tế, `installation_name` duy nhất, `environment`, `owner`.
- `runtime_iam_path`, `runtime_permissions_boundary_arn`, AMI/type/architecture đã review.
- `edge_certificate_common_name`, cùng CA public khi bật Roles Anywhere.
- `operator.user_arn`, `mfa_arn`, `deploy_role_arn` và `terraform_profile`.
- `operator.state_key` nếu cần key riêng. Mặc định là `cloud/<environment>/terraform.tfstate`.

Chạy renderer offline để sinh đồng bộ Terraform variables, backend và IAM.

```bash
python3 "$MILES_REPO/infra/terraform/cloud/scripts/render-installation.py" \
  --config "$MILES_PRIVATE/installation.json" --out "$MILES_PRIVATE/rendered"
export MILES_ACCOUNT_ID="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["account_id"])' "$MILES_PRIVATE/rendered/installation.tfvars.json")"
export MILES_REGION="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["region"])' "$MILES_PRIVATE/rendered/installation.tfvars.json")"
```

File example chứa account/AMI fixture, cần thay bằng giá trị thật trước khi dùng.
Directory output phải mới và nằm ngoài Git, directory 0700/file 0600. Tạo directory
mới cho mỗi lần render và review diff. Rendered IAM là baseline cần đối chiếu plan,
đặc biệt quyền fleet/ALB/SLR, Roles Anywhere, publisher và metadata bootstrap.

Đọc [operator runbook](../../infra/terraform/cloud/bootstrap/operator/README.md) để
validate/simulate policy và chuẩn bị delta quyền theo từng account. Cùng TF inputs có
thể cấp qua `TF_VAR_*` khi dùng Terraform trực tiếp. Đối chiếu saved plan để phát
hiện override và giữ IAM đã render khớp cấu hình thực áp dụng.

Account mới cần hoàn thành bước này một lần trước đăng nhập deploy. Account đã có Miles cần đối chiếu IAM và state hiện hữu trước khi tái sử dụng.

### Tách danh tính

| Danh tính | Phạm vi |
| --- | --- |
| Quản trị account, có MFA | Bootstrap và review/cập nhật/thu hồi IAM |
| IAM user `member-deploy`, có MFA | Đăng nhập Console và Switch role |
| Role từ `operator.deploy_role_arn` | Plan/apply workload và cleanup trong phạm vi đã duyệt |
| Gateway runtime role | Cloud lấy credential tạm qua IMDSv2 |
| Edge runtime role | Edge lấy credential tạm qua Roles Anywhere |
| User/token/credential của Miles | Gọi Admin/S3 API của Miles |

Dùng credential tạm cho người vận hành và runtime, bật MFA cho danh tính quản trị. [IAM security practices](https://docs.aws.amazon.com/IAM/latest/UserGuide/best-practices.html)

### Trình tự bootstrap

1. Đăng nhập Console account của member bằng danh tính quản trị đã có. Với account mới, chủ account có thể dùng root + MFA cho bước bootstrap IAM được giới hạn cụ thể.
2. Review các JSON trong `$MILES_PRIVATE/rendered/iam`, đối chiếu input và plan của account này.
3. Tạo user/MFA đúng ARN trong operator config, ví dụ user `member-deploy`, path `/miles/operators/`, MFA device `member-deploy`. Renderer đã điền trust và user policy theo các ARN này.
4. Tạo IAM user có Console password. Chủ account giao mật khẩu bằng kênh riêng, user đổi mật khẩu khi đăng nhập lần đầu, enroll MFA rồi đăng xuất/đăng nhập lại bằng MFA.
5. Tạo role đúng `operator.deploy_role_arn`, ví dụ role `miles-member-deploy`, path `/miles/bootstrap/`, dùng trust đã render và yêu cầu MFA. Baseline session duration là 3.600 giây.
6. Gắn user policy đã render. Gắn managed policy AWS `SignInLocalDevelopmentAccess` cho cả user và deploy role để dùng CLI login. [Quyền CLI sign-in](https://docs.aws.amazon.com/signin/latest/userguide/command-line-sign-in.html)
7. Chuẩn bị runtime permissions boundary và các deploy policy từ template. Review JSON, Access Analyzer và simulation trước khi gắn. Giữ deploy role tách khỏi quyền sửa chính role/operator/boundary bootstrap.
8. Lưu ARN/policy version ban đầu và mọi grant tạm vào private inventory. Đăng xuất phiên quản trị khi hoàn tất IAM.

Console tạo policy/role phải giữ đúng IAM path trong template. Agent có thể tạo bằng IAM API sau khi người sở hữu duyệt bản JSON và phạm vi thao tác. Luồng xác thực quản trị riêng ở mục 4.4 phục vụ trường hợp này.

Tên operator, deploy role, runtime IAM path và boundary lấy từ installation config. Đối chiếu ARN thực sau tạo với output render trước khi đăng nhập/apply.

**Template là baseline cần review theo plan thực tế.** Fleet/ALB/launch template/custom AutoScaling service-linked role, Edge Roles Anywhere, release publisher, bootstrap metadata, CloudWatch và cleanup có thể cần quyền bổ sung. Dùng [quy trình sinh và kiểm policy](../../infra/terraform/cloud/bootstrap/operator/README.md), đối chiếu đúng action/resource và hạn dùng. AccessDenied cần được chẩn đoán theo API bị từ chối, policy, boundary và key policy.

## 4. Đăng nhập bằng link và lưu authorization code

### 4.1. Khởi tạo phiên ở phía operator/agent

Đặt profile riêng cho account này và kiểm các biến credential còn trong shell trước khi dùng.

```bash
export MILES_LOGIN_PROFILE="miles-member-login"
export MILES_TF_PROFILE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["operator"]["terraform_profile"])' "$MILES_PRIVATE/installation.json")"
export MILES_DEPLOY_ROLE_ARN="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["operator"]["deploy_role_arn"])' "$MILES_PRIVATE/installation.json")"
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_SECURITY_TOKEN
export AWS_REGION="$MILES_REGION"
export AWS_DEFAULT_REGION="$MILES_REGION"
export AWS_PAGER=""

aws configure set region "$MILES_REGION" --profile "$MILES_LOGIN_PROFILE"
aws login --profile "$MILES_LOGIN_PROFILE" --region "$MILES_REGION" --remote
```

CLI sinh URL và chờ authorization code trên stdin. Giữ nguyên process đang chờ. Operator/agent gửi URL vừa sinh thành **link bấm được** cho member. URL gắn với phiên login đó, cần lấy mới mỗi lần. [Lệnh AWS login remote](https://docs.aws.amazon.com/cli/latest/reference/login/)

Khi agent chạy hộ, chuẩn bị file `$MILES_PRIVATE/cli-login-code.txt` rỗng, quyền 0600 và gửi đường dẫn tuyệt đối cho member mở. Sau khi member lưu code và báo xong, agent đọc file trong process mà không in nội dung, đưa code vào stdin của đúng phiên CLI đang chờ, rồi xóa nội dung file. Giữ code khỏi chat, command line và log.

Nếu tự thao tác terminal, member dán code trực tiếp vào prompt của CLI. Hai cách dùng cùng luồng remote login.

### 4.2. Member thao tác trên trình duyệt

1. Đăng nhập AWS Console bằng account ID/alias của mình, user `member-deploy`, password và MFA.
2. Bấm tên danh tính ở góc phải → **Switch role**. Với giao diện multi-session, mở **Add session → Switch role**.
3. Điền Account ID của mình, Role bằng phần path/name sau `:role/` trong `operator.deploy_role_arn`, ví dụ `miles/bootstrap/miles-member-deploy`. Display name có thể đặt `MilesMemberDeploy`. Bấm **Switch Role**, kiểm tên góc phải.
4. Bấm **link đăng nhập CLI mới nhất** do agent gửi. Trên màn hình cấp quyền, chọn deploy role vừa switch thuộc account của mình.
5. Copy authorization code AWS trả về, lưu vào file `cli-login-code.txt` agent đã chỉ. Báo “đã lưu code”.
6. Agent hoàn tất CLI login và kiểm STS. Chỉ tiếp tục khi Account và assumed role đúng dự kiến.

Switch role cần đầy đủ path. Phiên Console gốc cũng giới hạn thời gian còn lại của phiên role. [AWS Console role switching](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_switch-role-console.html)

Mẫu yêu cầu agent gửi cho member, thay các chỗ trong ngoặc bằng giá trị thực của lượt đăng nhập.

> Anh/chị đăng nhập Console bằng operator user + MFA, rồi Switch role theo path/name tôi gửi cho account của mình. Bấm link CLI tôi vừa tạo, chọn deploy role, lưu code vào file private tôi đã gửi đường dẫn và báo xong. Giữ password/MFA/code trong kênh đăng nhập và file private.

### 4.3. Kiểm danh tính và nối Terraform với CLI login

```bash
aws sts get-caller-identity --profile "$MILES_LOGIN_PROFILE"
```

`Account` phải bằng `$MILES_ACCOUNT_ID`. ARN STS có dạng `arn:aws:sts::<account>:assumed-role/<deploy-role-name>/<session>`. ARN STS bỏ IAM path, script gate trong repo đã xử lý trường hợp này.

Provider Terraform được pin trong repo dùng profile phụ `credential_process` để đọc credential tạm qua CLI. Cấu hình bên dưới ghi lệnh provider vào AWS config.

```bash
export MILES_AWS_CLI="$(command -v aws)"
aws configure set credential_process \
  "\"$MILES_AWS_CLI\" configure export-credentials --profile $MILES_LOGIN_PROFILE --format process --region $MILES_REGION" \
  --profile "$MILES_TF_PROFILE"
aws configure set region "$MILES_REGION" --profile "$MILES_TF_PROFILE"
export AWS_PROFILE="$MILES_TF_PROFILE"

python3 "$MILES_REPO/infra/terraform/cloud/scripts/check-deploy-identity.py" \
  --role-arn "$MILES_DEPLOY_ROLE_ARN"
```

Profile phụ trỏ tới profile login nguồn. Backend và provider dùng cùng profile phụ. Giữ `deploy_role_arn=null` trong tfvars vì CLI đã là phiên deploy role. [AWS process credentials](https://docs.aws.amazon.com/cli/latest/reference/configure/export-credentials.html)

### 4.4. Khi cần agent thao tác IAM bằng phiên quản trị

Chủ account duyệt rõ policy/action/ARN và thời hạn trước khi kích hoạt. Tạo profile riêng, ví dụ `miles-member-iam`, bằng `aws login --profile miles-member-iam --region "$MILES_REGION" --remote`.

- Agent gửi **link IAM mới**, đường dẫn `iam-login-code.txt` riêng và phạm vi thao tác đã duyệt.
- Member mở cửa sổ trình duyệt riêng, đăng nhập danh tính quản trị + MFA. Nếu đã duyệt dùng root, chọn root trong luồng này.
- Member lưu code IAM vào file riêng. Agent hoàn tất login, kiểm đúng account/danh tính, chỉ thực hiện phần IAM được duyệt rồi logout profile quản trị.
- Các bước Terraform/runtime tiếp tục dùng deploy profile đã qua identity gate.

### 4.5. Các lỗi đăng nhập thường gặp

| Hiện tượng | Cách xử lý |
| --- | --- |
| Bấm link gặp 400 | Agent gửi lại nguyên URL do CLI sinh, giữ nguyên query. Nếu link chat bị biến đổi, dùng file URL private làm phương án dự phòng. Nếu process chờ đã mất/hết hạn, tạo phiên và link mới |
| Chỉ thấy `member-deploy` trên màn hình cấp quyền | Quay lại Console, Switch role thành công rồi mở link của phiên đang chờ và chọn role |
| STS trả user/root hoặc account khác | Lấy code mới cho đúng danh tính, kiểm lại STS và identity gate |
| Code đã dùng hoặc không khớp phiên | Tạo link mới, lấy code mới cho đúng process CLI đó |
| Login xong vẫn `ExpiredToken` hoặc gần hết giờ | Switch back, đăng xuất/đăng nhập user bằng MFA, Switch role mới rồi tạo CLI login mới |
| Credential cũ ghi đè profile | Kiểm biến AWS credential, file credentials và cấu hình profile. Loại override đã xác định rồi kiểm lại STS |
| `AccessDenied` sau STS đúng | Đối chiếu action/resource và IAM. Tạo bản thay đổi quyền để review nếu cần |

Lên lịch refresh trước test dài và dành thời gian cho cleanup. Runtime dùng danh tính riêng của node, còn thao tác quản trị cần phiên operator hợp lệ. [Thời hạn và refresh CLI session](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sign-in.html)

## 5. Chuẩn bị topology, TLS và cấu hình account

Topology đầy đủ thường gồm hai Cloud node private ở hai AZ sau ALB và một EC2 riêng chạy profile Edge. Các node dùng chung S3/DynamoDB/history/secret trong account của member. Giới hạn số VM phải tính thêm node tạm khi rolling replacement và máy test client nếu cần.

| Cần chuẩn bị | Yêu cầu |
| --- | --- |
| Region/AZ | Commercial AWS Region trong config, hai AZ hợp lệ trong account, kiểm service availability/quota |
| AMI/type | Amazon Linux 2023 đã pin, kiến trúc khớp binary và loại máy, có dependency cần dùng |
| Boundary | ARN cùng account đã điền trong installation config |
| Cloud TLS | ACM certificate `ISSUED`, cùng account/Region, khớp hostname truy cập |
| Edge TLS | Nginx certificate/key khớp hostname Edge và trust của test client |
| Roles Anywhere | CA riêng, Edge leaf CN khớp `edge_certificate_common_name`, key riêng và provider của account này |
| Allowlist | Public IPv4/CIDR thật của test client và đường quản trị được duyệt |
| Clock | Chrony với nguồn IPv4 được chọn, `utc_reference` khớp observation trên từng host |
| Quản trị private | SSM hoặc private SSH/tunnel có IAM/document/host key đã kiểm tra |

Nếu có domain, cấp certificate và cấu hình DNS cho ALB/Edge trước smoke. Nếu chưa có domain, chuẩn bị CA test, import Cloud leaf vào ACM, cài Edge leaf và cấu hình resolver/trust trên test client. Giữ hostname ký SigV4, HTTP Host và TLS SNI nhất quán. Ghi phạm vi kiểm thử của resolver/CA private vào evidence.

Fleet cần interface endpoints ở hai AZ, hoặc NAT mỗi AZ. Với interface-only, AMI cần sẵn dependency. Edge dùng private DNS endpoints trong cùng VPC phải có đường 443 và SG cho phép tới endpoint ENI. Nếu dùng EC2 làm Edge, runtime vẫn dùng profile `edge` và Roles Anywhere để kiểm chứng đúng credential path.

Tên tài nguyên lấy từ `installation_name`, prefix bảng là `<installation_name>-`, bucket chứa account ID/Region. Dùng tên và state bucket riêng cho mỗi installation trong cùng account. Account đã có Miles cần xác nhận state sở hữu tài nguyên trước plan. Dùng chung installation/backend cho Cloud và Edge của lượt test.

Tag `Owner` lấy từ `owner`, `Environment` lấy từ `environment`, `Installation` lấy từ `installation_name`. Renderer dùng cùng Environment/Installation trong IAM conditions để scope tài nguyên của từng lượt dev. `Project=Miles` là tag ứng dụng dùng chung.

## 6. Foundation và bootstrap runtime

Mỗi apply dưới đây cần đúng account/role, saved plan đã review và nằm trong phạm vi người sở hữu duyệt. Plan thay đổi hoặc state stale cần review lại.

### 6.1. Tạo state backend lần đầu

Chỉ thực hiện khi account chưa có state backend được quản lý. Giữ bootstrap local state lâu dài trong private directory.

```bash
mkdir "$MILES_PRIVATE/bootstrap-state"
cp "$MILES_REPO"/infra/terraform/cloud/bootstrap/state/*.tf \
  "$MILES_PRIVATE/bootstrap-state/"
cp "$MILES_REPO"/infra/terraform/cloud/bootstrap/state/.terraform.lock.hcl \
  "$MILES_PRIVATE/bootstrap-state/"
# Review account, tags và bootstrap config trước plan.
terraform -chdir="$MILES_PRIVATE/bootstrap-state" init -lockfile=readonly
terraform -chdir="$MILES_PRIVATE/bootstrap-state" plan \
  -var-file="$MILES_PRIVATE/rendered/bootstrap-state.tfvars.json" -out="$MILES_PRIVATE/state.tfplan"
terraform -chdir="$MILES_PRIVATE/bootstrap-state" show "$MILES_PRIVATE/state.tfplan"
python3 "$MILES_REPO/infra/terraform/cloud/scripts/check-deploy-identity.py" \
  --role-arn "$MILES_DEPLOY_ROLE_ARN"
terraform -chdir="$MILES_PRIVATE/bootstrap-state" apply "$MILES_PRIVATE/state.tfplan"
```

### 6.2. Foundation với runtime chưa bật

Dùng `$MILES_PRIVATE/rendered/backend.hcl` và
`$MILES_PRIVATE/rendered/installation.tfvars.json` đã sinh ở mục 3. Foundation đặt
`paused=true`, `enable_compute=false`, `enable_asg=false`, `enable_alb=false` trong
installation JSON trước render. Kiểm account/bucket/key/profile trong backend.

```bash
export MILES_STACK="$MILES_REPO/infra/terraform/cloud/environments/dev"
export MILES_DEV_VARS="$MILES_PRIVATE/rendered/installation.tfvars.json"
terraform -chdir="$MILES_STACK" init -reconfigure -lockfile=readonly \
  -backend-config="$MILES_PRIVATE/rendered/backend.hcl"
terraform -chdir="$MILES_STACK" validate
terraform -chdir="$MILES_STACK" plan -input=false -lock-timeout=60s \
  -var-file="$MILES_DEV_VARS" -out="$MILES_PRIVATE/foundation.tfplan"
terraform -chdir="$MILES_STACK" show "$MILES_PRIVATE/foundation.tfplan"
python3 "$MILES_REPO/infra/terraform/cloud/scripts/check-deploy-identity.py" \
  --role-arn "$MILES_DEPLOY_ROLE_ARN"
terraform -chdir="$MILES_STACK" apply "$MILES_PRIVATE/foundation.tfplan"
terraform -chdir="$MILES_STACK" output -json runtime_inventory \
  > "$MILES_PRIVATE/runtime.json"
```

Foundation tạo datastore/KMS/secret metadata dù đang paused, có chi phí giữ tài nguyên. Lấy `data_bucket_name`, `kms_key_arn`, `secret_metadata_arn` và inventory 20 bảng từ output mới, lưu thành các biến private dùng ở bước sau.

### 6.3. Provision secret và metadata đúng một lần

Theo [shared runtime provisioning](../development/shared-runtime.md).

1. Tạo JSON secret với `key_id`, `seal_key`, `cursor_key`. Hai key độc lập, mỗi key là 32 byte ngẫu nhiên mã hóa base64, lưu file private 0600.
2. Dùng quyền publisher/provisioning đã duyệt để `secretsmanager put-secret-value` vào đúng ARN từ inventory, qua `file://<đường-dẫn-private>`. Giữ secret value ngoài Terraform/user-data/release/log.
3. Chuẩn bị operator startup JSON từ [shared Edge example](../../configs/shared-edge.example.json). Điền backend thật, đường dẫn data mới 0700, node label riêng, nguồn chrony hợp lệ và AWS config riêng.
4. AWS config operator dùng `credential_process` gọi CLI export-credentials từ deploy login. Section phải khớp `aws_profile` trong startup JSON, mặc định `miles-runtime`. Điền đường dẫn AWS CLI tuyệt đối. Trên Edge phục vụ client, thay provider provisioning bằng Roles Anywhere.
5. Giữ mọi gateway dừng, xác nhận 20 bảng của installation mới rỗng. Chạy bootstrap trên host Linux có chrony hợp lệ.

```bash
cd "$MILES_REPO"
./bin/dsgctl admin-token bootstrap \
  --runtime-config "$MILES_PRIVATE/operator-startup.json" \
  --user operator --token-file "$MILES_PRIVATE/system-admin-token"
```

Nếu máy operator thiếu chrony phù hợp, dùng EC2 provisioning host đã đưa vào plan và inventory. Bootstrap tạo user/token/policy, R1 history và immutable genesis/catalog. Khi lỗi hoặc mất ACK, giữ token/log và reconcile trạng thái đã ghi trước khi retry. Giữ nguyên sealing key của installation có credential đã seal cho tới khi có quy trình migration được review.

## 7. Build release, bật Cloud và dựng Edge

### 7.1. Build một release chung

```bash
cd "$MILES_REPO"
git status --short
export MILES_ARCH="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("release_architecture", "arm64"))' "$MILES_PRIVATE/rendered/installation.tfvars.json")"
export MILES_ARCHIVE="$MILES_PRIVATE/miles-linux-${MILES_ARCH}.tar.gz"
python3 -B deploy/build-release.py --arch "$MILES_ARCH" --out "$MILES_ARCHIVE" \
  > "$MILES_PRIVATE/release-build.json"
export MILES_RELEASE_SHA="$(sha256sum "$MILES_ARCHIVE" | cut -d ' ' -f 1)"
```

Gán `$MILES_DATA_BUCKET`, `$MILES_KMS_ARN` từ `runtime.json` sau khi đối chiếu đúng account. Upload bằng quyền publisher đã duyệt.

```bash
aws s3api put-object --region "$MILES_REGION" \
  --bucket "$MILES_DATA_BUCKET" \
  --key "releases/$MILES_RELEASE_SHA/miles-linux-${MILES_ARCH}.tar.gz" \
  --body "$MILES_ARCHIVE" \
  --server-side-encryption aws:kms --ssekms-key-id "$MILES_KMS_ARN" \
  > "$MILES_PRIVATE/release-upload.json"
```

Lấy VersionId thực từ kết quả upload, kiểm khác `null`. Pin SHA archive, S3 VersionId và kiến trúc cho mọi node. Đối chiếu manifest/source SHA khi thu evidence.

Giữ `release_architecture` của fleet khớp archive vừa build. Nếu đổi kiến trúc, build/upload release mới rồi cập nhật SHA/VersionId trước khi lập plan.

### 7.2. Bật Cloud fleet

Cập nhật installation JSON với AMI/type/architecture, certificate, client CIDR, release SHA/VersionId, `edge_ca_pem` public CA nếu dùng Edge, cùng network đã duyệt. Có thể đối chiếu [fleet example](../../infra/terraform/cloud/environments/dev/cloud-fleet.tfvars.example) để xem tên biến. Render sang directory mới `$MILES_PRIVATE/fleet-rendered`.

Đặt `paused=false`, `enable_asg=true`, `enable_compute=false`, `enable_alb=true`. Dùng **cùng stack và state foundation**. File fleet cần chứa đầy đủ cấu hình persistent đã chọn, gồm `runtime_secret_name` nếu đã đặt riêng.

```bash
python3 "$MILES_REPO/infra/terraform/cloud/scripts/render-installation.py" \
  --config "$MILES_PRIVATE/installation.json" --out "$MILES_PRIVATE/fleet-rendered"
export MILES_DEV_VARS="$MILES_PRIVATE/fleet-rendered/installation.tfvars.json"
terraform -chdir="$MILES_STACK" plan -input=false -lock-timeout=60s \
  -var-file="$MILES_DEV_VARS" -out="$MILES_PRIVATE/fleet.tfplan"
terraform -chdir="$MILES_STACK" show "$MILES_PRIVATE/fleet.tfplan"
# Review resource actions, IAM delta, quota và chi phí trước apply.
python3 "$MILES_REPO/infra/terraform/cloud/scripts/check-deploy-identity.py" \
  --role-arn "$MILES_DEPLOY_ROLE_ARN"
terraform -chdir="$MILES_STACK" apply "$MILES_PRIVATE/fleet.tfplan"
terraform -chdir="$MILES_STACK" output -json runtime_inventory \
  > "$MILES_PRIVATE/runtime-active.json"
terraform -chdir="$MILES_STACK" output -json edge_provider \
  > "$MILES_PRIVATE/edge-provider.json"
```

Đợi hai node `/healthz=200`, `/readyz=200` và ALB targets healthy. Chẩn đoán lỗi readiness từ config/backend/policy/history/clock/credential. Giữ readiness fail-closed khi thiếu điều kiện.

### 7.3. Dựng Edge trên EC2 riêng

Dev stack hiện tạo Cloud fleet và Edge IAM provider. **VM Edge cần cấu hình triển khai riêng** cùng inventory/state của nó. Review plan EC2/SG/disk/management trước khi tạo.

- Chọn Linux systemd, kiến trúc khớp release, encrypted disposable root disk và policy xóa đĩa đã duyệt.
- Cài Nginx, chrony và helper Roles Anywhere đã pin/verify. Tạo `miles` user, data directory 0700 thuộc user này.
- Cài cùng archive theo [Edge runbook](../../deploy/edge/README.md), verify SHA trước giải nén/installer, truyền `--region "$MILES_REGION"` cho `deploy/install.py`.
- Điền `shared-edge.example.json` với backend/secret chung. AWS config dùng section khớp `aws_profile` trong startup JSON, với credential_process của `aws_signing_helper`, ARN lấy từ `edge_provider` mới.
- Đặt Roles Anywhere certificate/key theo quyền đọc trong runbook, bật chặn fallback IMDS cho process Miles. EC2 management role nếu có phải được tách khỏi provider của Edge runtime.
- Cấu hình TLS Nginx và DNS/resolver của test client. Giữ Host/path và streaming theo `deploy/edge/nginx.conf`, kiểm `nginx -t` trước reload.
- Bind S3 8080, Admin 9000 và health/metrics 9100 trên loopback theo Edge config. Public ingress chỉ 443 từ allowlist được duyệt.
- Start Miles, kiểm health/readiness rồi đưa Nginx vào phục vụ. Chỉ Cloud chạy lifecycle controller ASG.

TLS front door và certificate Roles Anywhere có mục đích, key và vòng đời riêng. Khi Edge ở VPC dùng interface private DNS, kiểm cả route/SG tới endpoint trước test provider refresh.

Nếu Edge nằm trong private subnet có interface endpoints và không có NAT, cần tạo thêm endpoint `com.amazonaws.<region>.rolesanywhere` với private DNS. Module endpoints hiện tạo chín dịch vụ cho Cloud, danh sách chưa có Roles Anywhere và ingress SG hiện chỉ cho gateway/jobs. Cấu hình Edge phải cho phép HTTPS egress, đồng thời thêm ingress TCP 443 từ SG Edge vào SG của các interface endpoints cần dùng, gồm Roles Anywhere. SG Edge và SG endpoint phải thuộc topology đã review. Kiểm cả DNS resolution và credential helper trước khi start Miles. [Roles Anywhere PrivateLink](https://docs.aws.amazon.com/rolesanywhere/latest/userguide/vpc-interface-endpoints.html)

Nếu chọn Edge public subnet có Internet route hoặc private subnet có NAT, kiểm đường ra Roles Anywhere. Khi private DNS của các interface endpoints như Secrets Manager/KMS/CloudWatch đã bật trong VPC, vẫn kiểm SG tới từng endpoint có liên quan. Nếu Edge dùng gateway endpoints S3/DynamoDB, kiểm liên kết route table của subnet và endpoint policy. Ghi riêng các tài nguyên Edge/network được tạo ngoài dev stack vào inventory và phương án cleanup.

## 8. Nghiệm thu và thu evidence

Đầu tiên thiết lập đường quản trị private tới Admin loopback. SSM shell, SSM port forwarding và SSH forwarding cần đúng document/quyền cho từng cơ chế. Pin SSH host key qua kênh tin cậy. Xem [owner forwarding](../development/shared-runtime.md).

Qua Admin, tạo user/credential S3 thử nghiệm của Miles bằng `dsgctl`. Giữ deploy credential phục vụ AWS control plane và backend. Client gọi gateway dùng credential do Miles cấp, profile client riêng và path-style addressing. Sau CreateBucket, đọc cache policy qua Admin để materialize revision đầu trước payload I/O theo contract hiện tại.

| Nhóm test | Bằng chứng tối thiểu |
| --- | --- |
| Startup | SHA/build thống nhất, health/readiness và ALB targets healthy trên đúng node |
| Dữ liệu | Signed PUT/GET/HEAD/Range/LIST/multipart qua Cloud và Edge, đối chiếu hash/byte |
| HTTPS/SigV4 | TLS verify, Host/SNI/path đúng, Unicode/`%`/`+`/double slash và presigned nếu thuộc scope |
| Shared metadata | Ghi qua node A, đọc qua B/Edge, cursor/policy/history đồng nhất |
| Jobs/Nodes nếu có | Dùng node ID thực gồm incarnation, kiểm fan-out, partial result, takeover và cancel |
| Credentials | Edge refresh qua hạn session trong cùng process, helper lỗi bị từ chối đúng contract |
| Metrics | CloudWatch ingestion thực, exporter phục hồi sau lỗi, scrape hoạt động |
| Drain/replacement | Request đang chạy và hash, admission mới đóng, worker settle, Cloud shutdown proof đúng invocation và ALB unused |
| Regressions của thay đổi | Kịch bản, điều kiện kích hoạt, assertion và trạng thái hậu kiểm cụ thể |

Ma trận cần bám scope task/PR. Khi một executor nghiệp vụ chưa được tích hợp, ghi đúng capability hiện có và kết quả API, bàn giao cho task sở hữu executor. Với race/cut/takeover cần barrier hoặc instrumentation, ghi SHA bản test và chứng minh điều kiện overlap thực sự xảy ra.

Mỗi kết quả cần có SHA source/archive, thời gian UTC, môi trường, command, exit code, PASS/FAIL/SKIP, expected/actual và log đã lọc. Evidence local và AWS ghi riêng theo release thực chạy. Nếu cần chứng minh public Internet ingress, đặt probe ở mạng ngoài host đích và ghi nguồn probe.

## 9. Kết thúc lượt và cleanup

Cleanup nằm trong thời gian/ngân sách đã duyệt, cần giữ phiên deploy hợp lệ tới lúc xác minh xong. Khi còn lỗi test, vẫn ưu tiên thu evidence cần thiết rồi dọn đúng phạm vi trước deadline.

### 9.1. Drain rồi pause

1. Dừng nguồn request mới, lưu evidence và kiểm dữ liệu cần giữ.
2. Drain Cloud/Edge đúng runbook, chờ worker/request và journal export settle. Kiểm shutdown proof và trạng thái target trước khi chấp nhận drain hoàn tất.
3. Tạo saved pause plan từ cùng state, đầy đủ fleet vars/release pin, chỉ đổi `paused=true`.

```bash
terraform -chdir="$MILES_STACK" plan -input=false -lock-timeout=60s \
  -var-file="$MILES_DEV_VARS" -var=paused=true -out="$MILES_PRIVATE/pause.tfplan"
terraform -chdir="$MILES_STACK" show "$MILES_PRIVATE/pause.tfplan"
# Lấy Region/name/boundary từ installation config đã review.
export MILES_NAME="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["installation_name"])' "$MILES_PRIVATE/installation.json")"
export MILES_BOUNDARY_ARN="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["runtime_permissions_boundary_arn"])' "$MILES_PRIVATE/installation.json")"
python3 "$MILES_REPO/infra/terraform/cloud/scripts/check-session-plan.py" \
  pause "$MILES_PRIVATE/pause.tfplan" --account-id "$MILES_ACCOUNT_ID" \
  --region "$MILES_REGION" --installation-name "$MILES_NAME" --boundary-arn "$MILES_BOUNDARY_ARN"
python3 "$MILES_REPO/infra/terraform/cloud/scripts/check-deploy-identity.py" \
  --role-arn "$MILES_DEPLOY_ROLE_ARN"
terraform -chdir="$MILES_STACK" apply "$MILES_PRIVATE/pause.tfplan"
```

Gate chấp nhận create/delete đúng chiều cho tài nguyên phiên, gồm ASG/launch template, khi account/Region/name/boundary và session configuration khớp. Update/replace, drift, thay đổi datastore/IAM/KMS hoặc đổi topology cần review riêng. Trước khi dùng state cũ lần đầu với các trường session mới, review và áp plan cập nhật outputs/tag Installation bằng quyền đã duyệt trước khi gắn policy mới rồi tạo lại pause/resume plan. Pause fleet giữ IAM/SLR phục vụ lifecycle trong lúc drain.

4. Dọn Edge VM và đĩa của nó theo inventory/state riêng, cùng các host provisioning/test client đã tạo.
5. Kiểm API AWS độc lập với output Terraform. Rà EC2, EBS, ALB/TG, NAT, EIP, endpoints và các tài nguyên apply dở. Lưu các resource còn giữ và chi phí duy trì dự kiến.

Pause giữ datastore/key/secret/state và một số IAM/network/logs. Tiếp tục theo dõi chi phí các phần giữ lại.

### 9.2. Full teardown khi dữ liệu được phép xóa

Thực hiện [full teardown runbook](../../infra/terraform/cloud/full-teardown.md) với inventory và quyền cleanup được duyệt cho account của member.

- Review dữ liệu/backup/retention và danh sách ARN sẽ xóa. Backup cần đọc lại được, có key còn sử dụng được sau teardown.
- Lưu state backup private. Chuẩn bị cleanup config riêng, review việc gỡ protection và saved plans từ cùng remote backend.
- Dọn S3 đầy đủ versions, delete markers và multipart trong danh sách được duyệt. Kiểm từng lỗi API.
- Apply unprotect rồi lập/review saved destroy plan mới. State thay đổi hoặc apply dở cần inventory và plan mới.
- Kiểm 20 bảng, EBS và root volumes, runtime IAM/instance profiles, custom/service-linked roles thuộc lượt, network và tài nguyên ngoài state.
- Ghi SYSTEM backups/PITR, KMS/secret scheduled deletion và thời điểm xóa thực tế. Giữ bootstrap/state/evidence theo kế hoạch đã duyệt.
- Gỡ grant tạm và khôi phục policy version đã lưu, kiểm hiệu lực thu hồi.

Cuối lượt logout các profile login/quản trị đã dùng. Cache của profile login nguồn là phần cần thu hồi khi Terraform dùng profile phụ. [AWS CLI logout](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sign-in.html)

```bash
aws logout --profile "$MILES_LOGIN_PROFILE"
# Nếu đã tạo phiên quản trị trong lượt này.
aws logout --profile miles-member-iam
```

Thu hồi credential/certificate/token test theo phạm vi đã tạo, dọn file code/private key không còn cần giữ. Báo rõ tài nguyên đã xóa, đang chờ xóa và tiếp tục giữ. Chốt lượt sau khi kiểm orphan và ghi chi phí còn lại.

## 10. Mẫu giao việc cho agent của member

Điền nội dung trong ngoặc rồi gửi cùng tài liệu này. Các bước tạo tài nguyên chờ gói plan/IAM/chi phí được chủ account duyệt.

```text
Dựng Miles để kiểm thử commit [SHA] trong AWS account cá nhân [account ID].
Region [Region đã chọn]. Cloud và Edge đều chạy trên AWS.
Máy hiện tại chỉ dùng làm operator.

Tiêu chí cần nghiệm thu [danh sách].
Ngân sách dự kiến [USD], số VM tối đa [số], deadline gồm cleanup [UTC].
Dữ liệu synthetic được tạo/xóa [phạm vi], phần cần backup/giữ [danh sách].

Đối chiếu source và IAM/state của account trước khi làm.
Account hiện [chưa bootstrap / đã có operator, role, backend tại ...].
Domain/TLS hiện [thông tin].

Tạo phiên aws login --remote, gửi link bấm được cho tôi.
Tôi sẽ đăng nhập Console + MFA, Switch role và lưu authorization code vào
file private do bạn chỉ đường dẫn. Mỗi phiên dùng link/code mới.
Tách deploy profile và IAM admin profile, kiểm STS và identity gate.

Trước khi gắn IAM hoặc apply, đưa policy diff, saved plan, dự toán và
phương án cleanup để tôi duyệt. Sau nghiệm thu, cleanup trong phạm vi
được duyệt, kiểm orphan, thu hồi grant và báo phần giữ/chờ xóa.
```
