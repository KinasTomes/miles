# Dựng một installation Miles mới trên AWS

Member dùng account AWS của mình, một file cấu hình và CLI login tạm thời. Bộ dựng tạo IAM operator, state backend, shared storage, hai Cloud node sau ALB và một Edge EC2. Tất cả runtime đặt trên AWS. Máy đang thao tác chỉ giữ công cụ, source và artifacts private.

Luồng dưới dành cho **dev/test có dữ liệu synthetic**. Bộ dựng tạo CA TLS riêng, certificate Cloud nhập ACM và certificate Edge cấp cho Nginx. Client xác minh bằng CA của installation, không cần domain có sẵn. Đây chưa phải cấu hình certificate công khai và vận hành production. Certificate TLS và certificate Roles Anywhere dùng hai CA/key riêng.

Code và mock/local tests kiểm tra luồng dựng, cấp quyền, retry và cleanup. Cần một lượt AWS trên account đích để xác nhận IAM/quota/network thực tế. Không dùng evidence cũ của account khác thay cho lượt này.

## 1. Chuẩn bị

Máy operator Linux cần Git, Make, Python 3.12+, OpenSSL, curl, OpenSSH, Go theo `go.mod`, AWS CLI v2 từ 2.32.0, Session Manager plugin và Terraform 1.15.2. Provider AWS pin trong lockfile. Operator cần chrony hoạt động và một nguồn IPv4 được tin cậy. Kiểm nguồn bằng `chronyc -n -c -h 127.0.0.1 tracking`. `operator.utc_reference` phải khớp nguồn này. Runtime EC2 dùng AWS Time Sync.

Chọn commit đã commit sạch và tạo private directory ngoài Git. Giữ directory 0700, file private 0600, không gửi state/plan/key/token lên PR.

```bash
umask 077
export MILES_WORK="$HOME/miles-member-test"
export MILES_REPO="$MILES_WORK/repo"
export MILES_PRIVATE="$MILES_WORK/private"
mkdir -p "$MILES_PRIVATE"
chmod 700 "$MILES_PRIVATE"
git clone https://github.com/KinasTomes/miles.git "$MILES_REPO"
cd "$MILES_REPO"
git checkout --detach REPLACE_WITH_COMMITTED_SHA
make check
cp infra/terraform/cloud/environments/dev/installation.example.json \
  "$MILES_PRIVATE/installation.json"
```

Điền file JSON trước khi chạy. Example chứa account/AMI/CIDR fixture.

| Trường | Giá trị cần điền |
| --- | --- |
| `account_id`, `region`, `availability_zones` | Account của member, commercial Region và hai AZ có thật |
| `installation_name`, `environment`, `owner` | Tên riêng cho lượt test và chủ sở hữu |
| `ami_id`, `release_architecture` | Amazon Linux 2023 đã pin, `arm64` hoặc `amd64` đúng AMI |
| `instance_type`, `fleet_instance_type`, `edge_instance_type` | Loại máy được account cho phép, cùng kiến trúc release |
| `client_cidrs` | IP public của máy probe/operator dạng `/32`, được mở cổng 443 tới ALB/Edge |
| `runtime_iam_path`, `runtime_permissions_boundary_arn` | Path và boundary mới trong account này |
| `operator.user_arn`, `mfa_arn`, `deploy_role_arn` | Danh tính operator mới, tên MFA và role deploy |
| `operator.login_profile`, `admin_profile` | Hai profile CLI riêng cho workload và quản trị IAM |
| `operator.utc_reference` | Nguồn chrony IPv4 trên máy operator |
| `deployment.max_cost_usd`, `max_active_minutes` | Envelope và thời hạn của lượt đã được chủ account duyệt |

`operator.terraform_profile` dùng khi render và chạy Terraform trực tiếp. Bộ dựng xuất credential tạm vào môi trường của Terraform cho cả backend/provider. `deploy_role_arn` ở cấp installation giữ `null` vì CLI đã đăng nhập role.

Một installation giữ cùng config, thư mục private, source SHA và remote state trong suốt lượt chạy. Nếu đổi account/name/AMI/topology, dùng installation mới hoặc thực hiện migration được review riêng. Chỉ thời hạn audit/cleanup được phép điều chỉnh mà không thay danh tính installation.

### Installation riêng trong account đang dùng chung

Các giá trị `miles-dev` trong example/default giữ tương thích với môi trường cũ; không đổi hàng loạt chúng thành tên của một người dùng. Installation mới dùng JSON private với `installation_name`, `environment`, `owner`, runtime IAM path, role và backend riêng. Cấu hình runtime được sinh từ tên installation và Terraform outputs, gồm `ddb_table_prefix`, bucket, KMS key, secret và danh tính Edge; không chạy trực tiếp shared example chưa điền giá trị.

Nếu IAM hoặc bucket state đã được bootstrap bên ngoài Terraform, không chạy luồng fresh-install để nhận quản lý chúng tự động. Chủ account phải đối chiếu ARN, policy, trust, boundary và bucket policy hiện có rồi duyệt import/plan riêng. Giữ nguyên các deny bổ sung đã được phê duyệt; không render lại defaults rồi ghi đè quyền chỉ để vượt `AccessDenied`. Terraform CLI dùng workspace `default` trong backend riêng; bucket/key/lock key phải khớp policy, không migrate/copy state của installation khác.

Khi nâng cấp installation cũ, pin `owner` hiện tại trong private inputs và review mọi thay đổi tag. Các quyền mạng mới yêu cầu tag `Project`, `Environment`, `Installation` đã đúng trên tài nguyên. Nếu tài nguyên cũ chưa có `Installation`, chỉ chủ account được gắn nhãn sau khi đối chiếu từng ARN với state/inventory đã duyệt, trước khi chuyển sang policy mới. Không thêm quyền nhận tài nguyên chưa có tag một cách đại trà. Đổi `installation_name` trong state cũ có thể gây thay thế tài nguyên; đây không phải cách tách môi trường.

Bộ dựng cố định Terraform data directory theo từng stack, workspace `default`, loại bỏ biến `TF_*` kế thừa và không copy private `*.tfvars`/override files từ checkout. Khi chạy Terraform trực tiếp, người chạy phải dùng đúng private inputs/backend tương ứng và tự kiểm các biến môi trường này.

```bash
deploy() {
  python3 -B "$MILES_REPO/infra/terraform/cloud/scripts/deploy-installation.py" \
    --config "$MILES_PRIVATE/installation.json" \
    --private "$MILES_PRIVATE" "$@"
}
deploy prepare
```

`prepare` chạy offline, tạo CA/key, SSH key quản trị có giới hạn, render IAM/tfvars/backend và copy Terraform vào private directory. Nó chưa gọi AWS. Source phải sạch để release và Terraform được pin cùng SHA. Các bước sau tiếp tục dùng artifacts đã giữ.

## 2. Đăng nhập quản trị IAM bằng link và code

Dùng profile `operator.admin_profile`, ví dụ `miles-member-iam`. Chủ account có thể dùng root + MFA cho bootstrap IAM ban đầu. Phiên này chỉ chạy stack `operator`, không chạy workload.

```bash
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_SECURITY_TOKEN
aws configure set region ap-southeast-1 --profile miles-member-iam
aws login --profile miles-member-iam --region ap-southeast-1 --remote
```

Thay Region/profile theo JSON. CLI in URL rồi chờ code. Giữ process đang chờ.

Khi agent thao tác hộ, agent gửi **link bấm được vừa tạo** và file private `iam-login-code.txt`. Member mở link trong cửa sổ đăng nhập admin/root của account mình, chọn đúng danh tính, lưu authorization code vào file rồi báo xong. Agent đưa code vào stdin của process login tương ứng, không in code ra chat/log/command line, rồi xóa nội dung file. Khi tự chạy, dán code vào prompt CLI.

Mỗi phiên có URL và code mới. Nếu link báo 400, tạo lại phiên CLI và gửi link mới. Chỉ thử copy URL gốc từ terminal khi trình duyệt/chat đã biến đổi URL. [AWS CLI remote login](https://docs.aws.amazon.com/cli/latest/reference/login/)

## 3. Bootstrap user, role và quyền của account mới

```bash
deploy plan operator
```

Review `$MILES_PRIVATE/operator-plan-details.json`, các JSON policy trong `rendered/iam` và descriptor `operator-plan.json`. Plan chỉ được chứa IAM. Dùng Access Analyzer `validate-policy` và IAM simulation cho các action/resource cần dùng. Có thể sinh baseline độc lập từ JSON plan bằng [IAM Policy Autopilot](https://github.com/awslabs/iam-policy-autopilot), rồi đối chiếu với các policy đã scope. Không thay policy bằng baseline rộng mà chưa review.

Chủ account duyệt chính bản plan/policy này trước apply. Mọi lệnh apply yêu cầu SHA256 từ descriptor của saved binary plan.

```bash
deploy apply operator --plan-sha256 REPLACE_WITH_REVIEWED_SHA256
```

Stack tạo user, deploy role yêu cầu MFA, runtime boundary và các managed deploy policy được chia theo quota IAM. Nó gắn `SignInLocalDevelopmentAccess` cho user/role. Không tạo access key. Password Console được tạo ngoài Terraform state, lưu trong `$MILES_PRIVATE/console-login.json` và buộc đổi khi đăng nhập đầu. Retry không reset password đã có.

Member mở `https://ACCOUNT_ID.signin.aws.amazon.com/console`, đăng nhập user vừa tạo bằng password trong file private, đổi password và enroll Authenticator MFA với tên khớp `operator.mfa_arn`. Đăng xuất rồi đăng nhập lại bằng MFA. Xóa file password khi đã đổi và lưu thông tin đăng nhập ở nơi phù hợp. [IAM security practices](https://docs.aws.amazon.com/IAM/latest/UserGuide/best-practices.html)

Nếu account đã có user/role trùng ARN, dừng và review/import state. Không tự nhận quản lý hay thay quyền của tài nguyên có sẵn. Giữ bootstrap local state trong private directory lâu dài.

## 4. Đăng nhập deploy role

Trong Console của operator user, bấm tên danh tính → **Switch role**. Điền account ID của mình và phần path/name sau `:role/` trong `operator.deploy_role_arn`, ví dụ `miles/bootstrap/miles-member-deploy`. Kiểm tên role ở góc phải trước lấy code. [Switch role trong Console](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_switch-role-console.html)

Agent hoặc member chạy bằng profile `operator.login_profile`.

```bash
aws configure set region ap-southeast-1 --profile miles-member
aws login --profile miles-member --region ap-southeast-1 --remote
```

Agent gửi link CLI mới. Member bấm link trong cửa sổ đang dùng deploy role, chọn đúng role/account, lưu code vào `cli-login-code.txt` rồi báo xong. Agent hoàn tất login như mục 2. Không chọn dòng IAM user.

```bash
aws sts get-caller-identity --profile miles-member
```

Account phải khớp JSON, ARN là `arn:aws:sts::<account>:assumed-role/<deploy-role-name>/<session>`. Bộ dựng kiểm lại danh tính trước Terraform/provision. Phiên root/user không qua được gate workload.

Nếu hết phiên, Switch back về user rồi Switch role lại. Khi phiên Console gốc đã cũ, đăng xuất và đăng nhập user bằng MFA trước. Sau đó tạo URL CLI mới. `aws login` không kéo dài phiên role cũ.

## 5. State và foundation

Trước bước này, chủ account duyệt topology, IAM, dự toán, deadline gồm cleanup và quyền xóa dữ liệu synthetic. Tính cả endpoints/ALB, EC2/EBS, public IPv4, DynamoDB/PITR, S3, KMS, Secrets Manager, logs và traffic. Kiểm quota/loại máy/Free Tier của account thực tế. Số tiền trong JSON là envelope do người chạy theo dõi, không phải hard billing cap.

```bash
deploy plan state
deploy apply state --plan-sha256 REPLACE_WITH_STATE_PLAN_SHA256
deploy plan foundation
deploy apply foundation --plan-sha256 REPLACE_WITH_FOUNDATION_PLAN_SHA256
```

Review từng plan trước apply. State stack giữ bootstrap bucket/state riêng, mã hóa SSE-S3. Foundation tạo shared S3, 20 bảng DynamoDB có PITR/deletion protection, runtime KMS, secret metadata và network nền. Chưa có Cloud/Edge serving VM. Dù paused, datastore/key/secret đã có thể tính phí.

Plan/state đầy đủ được giữ private. Descriptor chỉ hiển thị resource actions và SHA. Nếu apply lỗi hoặc timeout, giữ artifacts, kiểm state/API và tạo plan mới khi cần. Không sửa state bằng cách xóa private directory.

## 6. Provision runtime và publish release

```bash
deploy provision
```

Lệnh này thực hiện các việc sau bằng deploy role.

- Tạo sealing/cursor key một lần và put qua Secrets Manager. Retry dùng lại key/request token, từ chối ghi đè secret khác.
- Put TLS và Roles Anywhere material của Edge vào secret bootstrap riêng.
- Dùng `dsgctl admin-token bootstrap` để tạo user/token, policy, R1 history và genesis trên backend mới. Runtime đang dừng, backend phải rỗng.
- Build shared release từ SHA đã pin, kiểm manifest/hash và upload vào S3 versioned bằng SSE-KMS.
- Download Roles Anywhere helper có version/SHA pin trong source, verify rồi upload một version riêng.
- Import Cloud test certificate vào ACM và tạo `active.tfvars.json` với các ARN/S3 VersionId/hash thực.

Secret value/key/token không nằm trong Terraform, user-data hoặc release. User-data chỉ chứa ARN và public bootstrap references. Token Miles được giữ tại `system-admin-token` trong private directory.

Khi metadata bootstrap báo lỗi nhưng token đã tồn tại, lệnh dừng để reconcile kết quả chưa chắc chắn. Không xóa token rồi bootstrap lại backend đã có dữ liệu. ACM import mất ACK được đối chiếu bằng domain/tag/nội dung certificate trước khi reuse. Outcome chưa xác định phải được xử lý trước retry.

## 7. Bật hai Cloud node và một Edge trên AWS

```bash
deploy plan active
deploy apply active --plan-sha256 REPLACE_WITH_ACTIVE_PLAN_SHA256
```

Review plan active, IAM và chi phí cập nhật trước apply. Cloud dùng ASG hai AZ, private subnet, ALB HTTPS và interface endpoints. Edge EC2 nằm public subnet với cổng 443 giới hạn `client_cidrs`, không mở SSH/Admin/health ra Internet.

Edge bootstrap dùng instance profile riêng để lấy release/helper/material đã pin. Process Miles dùng Roles Anywhere `credential_process`, bị chặn truy cập IMDS bootstrap identity. Nginx giữ nguyên signed Host/path và streaming. Module endpoints bổ sung Roles Anywhere PrivateLink và ingress từ SG Edge.

Tên AWS role, bucket, table, SG, certificate subject và Region lấy từ JSON/outputs của installation. Không có bước nhập tên hay account của người khác.

Chờ bootstrap/SSM/ALB healthy, rồi chạy.

```bash
deploy verify
```

Bộ probe kiểm readiness của ba node, hai ALB target healthy, TLS verify và signed PUT Cloud → GET Edge rồi PUT Edge → GET Cloud, đối chiếu SHA256. Nó tạo riêng user/credential/bucket synthetic, materialize policy trước payload I/O và lưu evidence private. Admin đi qua SSH over SSM có host key pin và Unix socket riêng, token không được gửi trước khi forwarding được xác nhận.

Probe chạy từ máy operator nên `client_cidrs` cần chứa IP egress của máy này. `.test` hostnames được nối tới ALB/Edge bằng curl `connect-to`, vẫn giữ Host/SNI và TLS verification. Không dùng `-k`.

`verify` là smoke cho installation. Multipart/range/presigned, refresh qua hạn credential, Jobs/Nodes, CloudWatch ingestion, drain/replacement và fault tests cần chạy theo ma trận của task đang nghiệm thu. Ghi đúng source SHA, môi trường, expected/actual và PASS/FAIL/SKIP. Không suy ra production readiness hay performance từ smoke này.

## 8. Pause hoặc full cleanup

Dừng nguồn request mới và lưu evidence trước deadline. Phiên deploy cần còn hiệu lực cho toàn bộ cleanup. Theo dõi thời gian từ tài nguyên foundation đầu tiên, gồm bootstrap và drain. `max_active_minutes` chặn reactivation quá cửa sổ active đã ghi, không tự tắt AWS hay giới hạn hóa đơn.

### Pause giữ dữ liệu

```bash
deploy plan paused
deploy apply paused --plan-sha256 REPLACE_WITH_PAUSE_PLAN_SHA256
```

Gate chỉ cho xóa tài nguyên phiên, giữ nguyên datastore/IAM/KMS/topology. Apply chờ Edge Miles stop sạch theo journal đúng invocation và Nginx graceful stop trước Terraform. Cloud tiếp tục chạy tới khi ASG termination hook bắt đầu, để lifecycle controller bind đúng invocation rồi kiểm shutdown/ALB drain. Pause xóa cả Cloud/Edge serving VM, ALB và interface endpoints, giữ dữ liệu/key/secret/bootstrap/state. Những phần giữ lại tiếp tục có chi phí.

Nếu bootstrap lỗi và không có shutdown proof, dừng cleanup tự động để kiểm node qua SSM. Xác nhận không có request/worker và xin quyết định xử lý node lỗi trước khi bỏ qua proof. Không coi service bị kill là drain thành công.

### Full teardown dữ liệu synthetic

Chủ account phải duyệt danh sách dữ liệu và đĩa bị xóa, backup cần giữ và thời hạn grant. Full teardown xóa S3 versions/delete markers/multipart, 20 bảng, runtime IAM/network/logs và root disks không backup. KMS/secret dùng cửa sổ chờ xóa 30 ngày. Bootstrap user/role/boundary/state được giữ để tái sử dụng. Shared ELB service-linked role của account cũng được giữ, custom ASG role của installation bị xóa.

1. Trong JSON, thêm `operator.cleanup_expires_at` theo UTC ISO8601, ví dụ `2026-11-03T18:00:00Z`, trong tương lai và tối đa bốn giờ. Dùng thời điểm của lượt thực tế.
2. Đăng nhập admin profile nếu cần. `deploy plan operator`, review policy diff có cleanup grant theo installation/thời hạn, rồi `deploy apply operator --plan-sha256 ...` sau khi được duyệt.
3. Dùng deploy profile cho các bước còn lại.

```bash
deploy plan unprotect
deploy apply unprotect --plan-sha256 REPLACE_WITH_UNPROTECT_PLAN_SHA256
deploy plan destroy
deploy apply destroy --plan-sha256 REPLACE_WITH_DESTROY_PLAN_SHA256 --delete-data
deploy closeout
```

Unprotect chỉ được đổi DynamoDB deletion protection từ true sang false trên bản copy cleanup. Destroy plan chỉ được xóa graph runtime của installation, không đụng bootstrap/state. Lệnh xóa kiểm per-object errors của S3. `closeout` đối chiếu state rỗng và API độc lập cho EC2, EBS, VPC, ASG, bucket, 20 bảng cùng KMS/secret pending deletion, lưu `cleanup-evidence.json`.

Đối chiếu thêm các tài nguyên phát sinh ngoài state, import/apply mất ACK và SYSTEM backups/PITR theo [full teardown runbook](../../infra/terraform/cloud/full-teardown.md). Không chỉ dựa vào state rỗng để kết luận không còn orphan. Giữ inventory và thời điểm chờ xóa để hậu kiểm.

Cuối cùng xóa `cleanup_expires_at` khỏi JSON, review/apply lại operator plan bằng admin để thu hồi grant. Không xóa stack operator/state. Logout cả hai profile login nguồn.

```bash
aws logout --profile miles-member
aws logout --profile miles-member-iam
```

Giữ bootstrap local state và evidence private đã lọc. Thu hồi/xóa credential, key, token không còn cần thiết theo phạm vi lượt test. Khi chạy lại sau teardown, chọn installation name/private directory mới vì secret cũ có thể còn trong cửa sổ scheduled deletion.

## 9. Giao việc cho agent của member

```text
Dựng Miles commit [SHA] trên AWS account của tôi [account ID], Region [Region].
Cloud và Edge đều chạy trên AWS. Dùng docs/testing/aws-personal-account-guide.md
và deploy-installation.py của cùng commit.

Kiểm installation JSON, công cụ, chrony, quota và IAM trước apply.
Gửi link aws login --remote bấm được và đường dẫn private để tôi lưu code.
Tách phiên admin chỉ thao tác IAM với phiên deploy chạy workload.

Tiêu chí test [danh sách]. Envelope [USD], deadline gồm cleanup [UTC].
Dữ liệu synthetic/đĩa được xóa [phạm vi], backup/phần giữ [danh sách].
Trình từng saved plan, policy diff, chi phí và cleanup để tôi duyệt.
Đừng coi sample budget trong JSON là quyền tự tạo hoặc xóa tài nguyên.
Sau test, cleanup theo phê duyệt, kiểm orphan, thu hồi grant và báo bằng chứng.
```
