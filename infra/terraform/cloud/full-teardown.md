# Thu hồi hoàn toàn AWS dev

Quy trình này hoàn tất checklist teardown của W03-HUNG-01. Dùng sau khi chủ dữ liệu
đã duyệt danh sách xóa và saved plans cụ thể. Quyết định giữ pause vẫn có hiệu lực
cho đến lúc đó. Pause/resume dùng [dev sessions](dev-sessions.md).

## Inventory và dữ liệu cần giữ

Đăng nhập deploy role, dùng profile `miles-dev-terraform` cho cả backend và provider.
Đặt `PRIVATE_DIR`, `DEV_VARS`, `DEPLOY_ROLE_ARN` như runbook dev sessions; các file
state/plan/inventory/log nằm ngoài Git, thư mục 0700 và file 0600.

1. Kiểm đúng account/role/Region và remote backend. Pull một bản state dự phòng;
   bản này chỉ là evidence, không dùng làm backend apply. Ghi source commit và
   state lineage/serial; ngừng mọi phiên apply khác trong cửa sổ teardown.
2. Đối chiếu API inventory với state: S3, 20 bảng DynamoDB, data EBS, KMS/secret,
   audit log, runtime IAM, SSM và VPC. Kiểm cả tài nguyên ngoài state có tags dev,
   EC2/NAT/EIP/interface endpoints/ALB và root volumes mồ côi.
3. Dừng nguồn ghi và xác nhận nhóm không còn dùng môi trường. Kiểm S3 versions,
   delete markers, multipart uploads; Scan COUNT nhất quán từng bảng DynamoDB,
   backup/PITR; kiểm EBS và snapshot, secret versions và logs cần giữ. ItemCount
   từ DescribeTable không thay được kiểm tra dữ liệu trực tiếp.
4. Lập bảng giữ/xóa/backup, gồm hai S3 sentinel nghiệm thu nếu còn. Backup phải đọc
   lại được sau teardown. Nếu backup/log/secret còn phụ thuộc runtime KMS key thì
   giữ key, hoặc chuyển bản giữ sang cơ chế mã hóa độc lập rồi kiểm đọc lại trước
   khi lên lịch xóa key. Không coi snapshot cùng key là backup độc lập.

Role deploy thường ngày không có quyền Scan dữ liệu, đọc mọi version hoặc xóa
datastore. Quyền audit tạm chỉ đọc đúng tài nguyên; quyền cleanup được review riêng,
có hạn dùng và được gỡ khi xong. Cập nhật IAM qua phiên quản trị IAM; mọi thao tác
workload vẫn dùng deploy role. Không đổi trust/boundary hoặc cấp AdministratorAccess.

## Chuẩn bị cấu hình cleanup

Giữ `prevent_destroy`, S3 `force_destroy=false` và DynamoDB deletion protection trong
cấu hình dev chuẩn. Tạo bản cấu hình cleanup riêng từ commit đã review, chỉ chứa
modules và dev stack; giữ nguyên backend S3. Không sao chép bootstrap stack,
`.terraform`, state, tfvars hoặc saved plan từ thư mục thử nghiệm.

Trong bản cleanup, bỏ bốn lifecycle `prevent_destroy` của S3/DynamoDB/EBS/KMS và
đặt `deletion_protection_enabled=false` cho DynamoDB. Giữ `force_destroy=false`,
KMS deletion window và secret recovery window 30 ngày. Không thay AMI, topology,
policy runtime hoặc schema. Ghi diff cấu hình này vào evidence riêng để review.

`TEARDOWN_STACK` phải là đường dẫn tuyệt đối tới `environments/dev` của bản cleanup.
Init bằng cùng backend config đã đối chiếu với stack đang chạy:

```bash
umask 077
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
terraform -chdir="$TEARDOWN_STACK" init -input=false -reconfigure -lockfile=readonly -backend-config="$PRIVATE_DIR/backend.hcl"
terraform -chdir="$TEARDOWN_STACK" state pull > "$PRIVATE_DIR/before-teardown.tfstate"
terraform -chdir="$TEARDOWN_STACK" plan -input=false -lock-timeout=60s -var-file="$DEV_VARS" -var=paused=true -out="$PRIVATE_DIR/unprotect.tfplan"
terraform -chdir="$TEARDOWN_STACK" show -json "$PRIVATE_DIR/unprotect.tfplan" > "$PRIVATE_DIR/unprotect-plan.json"
```

Plan unprotect chỉ được update 20 bảng: duy nhất `deletion_protection_enabled`
chuyển true thành false. Không create/delete/replace/import/move, không drift chưa
giải thích. Nếu phát hiện drift, đối chiếu API và review riêng trước khi tạo lại
plan. Không dùng `-target`, `-refresh=false` hoặc copy state local để vượt review.

Generate baseline quyền apply từ JSON plan bằng IAM Policy Autopilot; review lại
resource/action, scope tới đúng ARN và bổ sung hạn dùng. Không attach tự động bằng
Autopilot. Kiểm Access Analyzer và simulation gồm các ca từ chối tài nguyên ngoài
phạm vi, bootstrap state, expired grant và quyền IAM ngoài cleanup runtime roles.

## Apply các bước đã được duyệt

Chỉ sau khi inventory/dữ liệu/quyền và plan được duyệt, kiểm lại identity và áp đúng
saved plan unprotect đã review:

```bash
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
terraform -chdir="$TEARDOWN_STACK" apply -input=false -lock-timeout=60s "$PRIVATE_DIR/unprotect.tfplan"
```

Dừng mọi nguồn ghi trước khi dọn S3. Xóa đúng danh sách object versions/delete
markers đã duyệt, abort multipart uploads đã duyệt rồi list lại đến khi rỗng.
`aws s3 rm --recursive` không dọn hết versioned bucket. Kiểm từng lỗi trong kết quả
DeleteObjects; không coi HTTP 200 là mọi object đã xóa thành công.

Sau khi protection update và dọn dữ liệu hoàn tất, lập **plan destroy mới** từ remote
state thật; plan tạo trước bước unprotect chỉ dùng review, không được apply:

```bash
terraform -chdir="$TEARDOWN_STACK" plan -destroy -input=false -lock-timeout=60s -var-file="$DEV_VARS" -var=paused=true -out="$PRIVATE_DIR/destroy.tfplan"
terraform -chdir="$TEARDOWN_STACK" show -json "$PRIVATE_DIR/destroy.tfplan" > "$PRIVATE_DIR/destroy-plan.json"
```

Đối chiếu toàn bộ address/ARN với inventory và danh sách đã duyệt. Chỉ delete các
resource dev được duyệt; không có bootstrap/state/operator/boundary hoặc create,
replace, thay đổi chưa giải thích. Nếu số lượng hoặc phạm vi đổi, dừng và review
lại; không cố định số 70 từ inventory lịch sử. Gate `check-session-plan.py` chỉ dành
cho pause/resume và phải tiếp tục từ chối full teardown.

Lưu SHA-256 của saved plan đã duyệt, kiểm lại trước apply. Nếu state thay đổi hoặc
plan stale, tạo/review plan mới. Chỉ chạy lệnh sau khi đúng plan được duyệt:

```bash
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
terraform -chdir="$TEARDOWN_STACK" apply -input=false -lock-timeout=60s "$PRIVATE_DIR/destroy.tfplan"
```

## Nghiệm thu và phục hồi khi dở

Kiểm API độc lập với Terraform outputs: không còn datastore/network/runtime dev
ngoài danh sách giữ đã duyệt; không còn orphan EC2/NAT/EIP/volume/endpoints/ALB.
Kiểm cả backup/snapshot còn giữ, secret scheduled deletion và KMS PendingDeletion;
ghi thời điểm xóa dự kiến và chi phí còn lại. Terraform state rỗng không chứng minh
AWS đã xóa mọi tài nguyên. Giữ bootstrap backend, state versions và audit evidence.

KMS key và secret có thời gian chờ xóa. Báo riêng phần đã xóa và phần chờ xóa,
không ghi toàn bộ đã biến mất hoặc chi phí bằng không. Secret chờ xóa có thể ngăn
dựng lại cùng tên; cần restore/reconcile hoặc chờ xóa trước khi tạo lại. Dữ liệu
S3/DynamoDB/EBS đã xóa không tự phục hồi khi hủy lịch xóa key/secret.

Nếu apply dở, giữ state và log, kiểm inventory hiện tại rồi lập plan mới từ cùng
remote backend. Không force-unlock khi chưa chứng minh phiên giữ lock đã kết thúc;
không state rm hoặc chạy lại saved plan cũ để giấu lỗi. Nếu chưa xóa dữ liệu và hủy
teardown, review plan bật lại protection; khi dữ liệu đã xóa, chỉ phục hồi từ backup
đã kiểm, đồng thời đối chiếu key/secret và state trước khi resume.

Gỡ quyền audit/cleanup tạm, xác minh deploy role lại bị từ chối xóa datastore/key,
đăng xuất phiên quản trị IAM. Cập nhật bằng chứng tổng hợp trong
[readiness](../../../docs/testing/aws-dev-readiness.md), PR và backlog. Checklist chỉ
đạt khi destroy thật đã kiểm và các mục giữ/chờ xóa được ghi rõ; Done còn theo
điều kiện review/CI/merge của task.

Nguồn: [S3 empty bucket](https://docs.aws.amazon.com/AmazonS3/latest/userguide/empty-bucket.html),
[KMS deletion](https://docs.aws.amazon.com/kms/latest/developerguide/deleting-keys.html),
[Secrets Manager deletion](https://docs.aws.amazon.com/secretsmanager/latest/userguide/manage_delete-secret.html).
