# Dựng, tạm nghỉ và bật lại dev

Quy trình này dùng Terraform state đang quản lý môi trường. Không tạo lịch tự bật,
không tự apply, không chạy destroy toàn bộ stack để tiết kiệm tiền. Kết quả AWS thật ngày 25/10/2026 nằm ở
[bằng chứng nghiệm thu](../../../docs/testing/aws-dev-readiness.md).
Terraform tests trong CI vẫn dùng mock provider.

## Phần giữ và phần thu hồi

| Giữ qua mỗi phiên | Thu hồi khi `paused=true` |
|---|---|
| S3 versioned, 20 bảng DynamoDB/PITR | EC2 dev và root disk 8 GiB |
| Data EBS gp3 20 GiB ở AZ đầu, KMS | NAT Gateway, EIP và NAT routes |
| State backend, IAM, secret metadata, SSM document, logs | Interface endpoints và SG/rules riêng của chúng |
| VPC/subnets/SG chung, S3/DDB gateway endpoints | ALB/listener/target group nếu có |

Dữ liệu bền vững, Badger và cấu hình cần giữ phải nằm trên data volume, không phải
root disk. Chế độ nghỉ **terminate EC2**, không phải Stop để giữ nguyên root.
AMI có thể không còn launch được về sau; giữ AMI đã review trong config và kiểm tra
lại trước resume. Instance ID/private IP/ALB DNS có thể đổi. Không coi DNS hoặc
instance ID cũ là ổn định; lấy lại inventory sau mỗi phiên.

Giữ `enable_compute=true`, `egress_mode` và `enable_alb` theo cấu hình muốn chạy;
chỉ đổi `paused`. Không đổi AMI/schema/CIDR/AZ/role/boundary cùng một plan bật/tắt.
Pause không phải chi phí 0: EBS/data/PITR/key/secret/log/state còn phí.

## Danh tính và cấu hình riêng

Dùng operator/MFA và role như [bootstrap](bootstrap/operator/README.md). Để provider
6.21.0 dùng session `aws login` qua cơ chế chuẩn `credential_process`, cấu hình
profile phụ (không ghi raw credential):

```bash
aws configure set region ap-southeast-1 --profile miles-dev
aws configure set credential_process '/absolute/path/to/aws configure export-credentials --profile miles-dev --format process --region ap-southeast-1' --profile miles-dev-terraform
aws configure set region ap-southeast-1 --profile miles-dev-terraform
export AWS_PROFILE=miles-dev-terraform
```

Thay `/absolute/path/to/aws` bằng đường dẫn CLI trên máy. Profile gốc `miles-dev`
vẫn là session đăng nhập role; profile phụ chỉ đọc credential tạm qua AWS CLI.
Không đặt `credential_process` trỏ về chính profile phụ. Giữ `.aws/config` 0600.
Nếu shell còn credential AWS khác, xóa các biến credential đó trước khi dùng profile.

Đặt `PRIVATE_DIR`, `DEV_VARS`, `DEPLOY_ROLE_ARN`, `AWS_ACCOUNT_ID` từ cấu hình riêng
ngoài checkout; thư mục 0700, file 0600, `umask 077`. `DEV_VARS` gồm account đúng,
`paused=true`, `enable_compute=true`, AMI Amazon Linux 2023 ARM64/EBS đã pin,
egress dự kiến `nat` và boundary. AMI không lấy moving `latest` trong Terraform.
Backend dùng cùng profile phụ, region Singapore, encrypt/use_lockfile=true,
key `cloud/dev/terraform.tfstate` trong đúng bucket bootstrap account này.

## Lần dựng đầu tiên — sau khi được duyệt triển khai tính phí

1. Review và cập nhật các policy mới theo [bootstrap](bootstrap/operator/README.md).
   Đây là thay đổi IAM riêng; template trong Git không tự cập nhật role đang chạy.
2. Bootstrap S3 state riêng dưới role đã kiểm tra. Giữ bootstrap local state ở
   thư mục riêng 0700/file 0600; không dùng remote backend bucket trước khi nó tồn tại.
3. Init backend dev bằng file riêng. Chạy gate danh tính trước **mỗi** apply.
4. Plan lần đầu với `paused=true`. Review toàn bộ tài nguyên persistent và phí giữ,
   rồi apply saved plan nếu được duyệt. Gate session sẽ từ chối plan tạo cả datastore;
   không bỏ qua bằng `-target`/`-refresh=false`.

```bash
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
terraform -chdir=infra/terraform/cloud/environments/dev init -backend-config="$PRIVATE_DIR/backend.hcl" -reconfigure
terraform -chdir=infra/terraform/cloud/environments/dev plan -input=false -lock-timeout=60s -var-file="$DEV_VARS" -var=paused=true -out="$PRIVATE_DIR/initial.tfplan"
# Review đầy đủ initial.tfplan bằng terraform show; apply là một bước được duyệt riêng.
```

Lần dựng đầu có S3/DDB/KMS/EBS nên vẫn tính phí dù paused. Plan-local là empty-state
read-only copy, không dùng nó làm saved plan apply hoặc thay remote-state review.

## Bật một phiên

```bash
terraform -chdir=infra/terraform/cloud/environments/dev plan -input=false -lock-timeout=60s -var-file="$DEV_VARS" -var=paused=false -out="$PRIVATE_DIR/resume.tfplan"
python3 infra/terraform/cloud/scripts/check-session-plan.py resume "$PRIVATE_DIR/resume.tfplan" --account-id "$AWS_ACCOUNT_ID"
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
terraform -chdir=infra/terraform/cloud/environments/dev apply -lock-timeout=60s "$PRIVATE_DIR/resume.tfplan"
```

Các lệnh apply ở runbook chỉ chạy sau review/authorization phiên tương ứng, không
được gọi bởi scripts kiểm tra. Áp đúng saved plan đã kiểm tra, không tạo lại plan
khác giữa bước gate và apply. Nếu state đã đổi, tạo/review/gate plan mới.

Lấy inventory riêng tư, đợi EC2/SSM Online, rồi dùng document `miles-dev-shell`:

```bash
terraform -chdir=infra/terraform/cloud/environments/dev output -json runtime_inventory > "$PRIVATE_DIR/runtime.json"
aws ssm start-session --target "$INSTANCE_ID" --document-name miles-dev-shell --region ap-southeast-1
```

AMI nền có SSM Agent; chưa có binary DSG hay secret. Không mở SSH/Admin/metrics
Internet. Shell session dùng TLS + KMS data channel, log stream vào log group
runtime mã hóa KMS. Không cho phép thay sang document không audit/port-forward trong
policy operator này. SSM qua interface mode cần endpoint `ssmmessages`, đã bổ sung
(thành 9 services × 2 AZ). CloudTrail/API audit và log delivery cần kiểm thật khi apply.

Data volume được attach nhưng **không tự format**. Trong phiên đầu, đối chiếu đúng
VolumeId từ inventory với serial bằng `lsblk -o PATH,SERIAL,FSTYPE,MOUNTPOINTS`.
Chỉ khởi tạo ext4 khi đã xác nhận đó là volume mới, rỗng, chưa mount và không phải
root. Ghi filesystem UUID vào evidence riêng rồi mount vào `/var/lib/miles`.
Khi resume, tìm volume theo ID/serial và UUID đã lưu, mount filesystem cũ;
không chạy mkfs. Nếu thiếu/mismatch UUID hoặc mount thất bại thì dừng, không khởi động
DSG trên thư mục root disk trống. Không dùng tên `/dev/nvme1n1` như định danh cố định.

Deploy binary ARM64/config của nhóm sau khi W2 sẵn sàng, lấy secret qua instance
profile. Đặt Badger/config cần giữ trên data mount. Kiểm mountpoint, đọc sentinel/hash
cũ trước khi khởi động; chưa có volume mount không được coi node ready.

## Kết thúc phiên

1. Ngừng nhận request mới, dừng DSG/jobs đúng quy trình graceful drain, xử lý write
   đang chạy; flush Badger và unmount data volume. Không force-detach hoặc kill để
   né drain. Session gate không chứng minh ứng dụng đã drain.
2. Tạo/gate saved plan pause, chỉ được xóa các tài nguyên theo giờ trong bảng đầu:

```bash
terraform -chdir=infra/terraform/cloud/environments/dev plan -input=false -lock-timeout=60s -var-file="$DEV_VARS" -var=paused=true -out="$PRIVATE_DIR/pause.tfplan"
python3 infra/terraform/cloud/scripts/check-session-plan.py pause "$PRIVATE_DIR/pause.tfplan" --account-id "$AWS_ACCOUNT_ID"
python3 infra/terraform/cloud/scripts/check-deploy-identity.py --role-arn "$DEPLOY_ROLE_ARN"
terraform -chdir=infra/terraform/cloud/environments/dev apply -lock-timeout=60s "$PRIVATE_DIR/pause.tfplan"
```

3. Kiểm inventory không còn instance/NAT/EIP/interface endpoints/ALB; data volume
   detached vẫn tồn tại, S3/DDB/key/state còn nguyên. Kiểm cả orphaned root volumes,
   Elastic IP và phần apply dở; output Terraform đơn thuần không chứng minh không rò phí.
4. Giữ `paused=true` trong file riêng cho thao tác plan mặc định về sau. `paused=false`
   chỉ là override khi bắt đầu phiên được duyệt.

Gate chỉ cho pause=delete, resume=create trên danh sách resource phiên cụ thể;
chặn update/replace/import/move, drift/deferred work và thay đổi datastore/IAM/KMS.
`session_configuration` trong state giữ AMI/topology đã review, phải khớp khi bật/tắt.
Nếu gate chặn, review plan riêng để giải quyết drift; không mở rộng allowlist tùy tiện.
Thay cấu hình topology (NAT↔interface) cũng là plan riêng, không phải resume.

## Đối chiếu state và xử lý pause dở

Gate giữ nguyên nguyên tắc chặn drift. Sau apply, provider có thể đọc thêm các
thuộc tính computed do resource khác quản lý: inline policy của role, routes của
route table, data-volume attachment/root tags của instance, association của EIP.
Nếu gate chặn, đối chiếu từng field với resource sở hữu và API AWS trước khi lập
**plan refresh-only riêng**. Dùng cùng config/giá trị `paused` của môi trường hiện
hữu, review saved plan phải không có AWS resource actions rồi mới apply để cập
nhật state. Sau đó tạo lại session plan và chạy gate; không dùng refresh-only để
che thay đổi IAM/network/data chưa rõ nguồn.

AWS provider 6.21.0 có thể giữ association EIP cũ trong destroy plan sau khi NAT đã
xóa. Phiên kiểm thật gặp `DisassociateAddress` bị từ chối ở bước EIP, dù EC2/NAT đã
thu hồi. Cách phục hồi đã kiểm: đối chiếu AllocationId thuộc state/tags Miles/dev;
`describe-addresses` phải cho thấy EIP không còn AssociationId/NetworkInterfaceId.
Review refresh-only riêng cho association và NAT routes đã mất, áp saved plan cập
nhật state, rồi tạo/gate/apply **pause plan mới** để ReleaseAddress. Cuối cùng kiểm
inventory không còn EIP. Không áp lại plan cũ hoặc cấp quyền disassociate toàn
Region chỉ để vượt qua lỗi này. Vì vậy pause chưa được coi là xong chỉ dựa vào EC2
terminated hay lần apply đầu; phải có bước kiểm orphan và phục hồi khi cần.

## Thu hồi hoàn toàn môi trường tạm

Pause đã thu hồi phần theo giờ; không tự xóa data để tiến về 0 USD. Full teardown
chỉ dành cho dataset disposable đã được chủ dữ liệu xác nhận, sau khi kiểm snapshot,
retention, recovery roots, object versions/delete markers và state backup. Cần review
riêng việc gỡ prevent_destroy/deletion protection, quyền cleanup giới hạn tài nguyên,
plan destroy và thời gian chờ KMS/Secrets deletion. Role deploy thường ngày không có
DeleteBucket/DeleteTable/DeleteVolume/ScheduleKeyDeletion. Không cấp Admin để lách.

Trước Done W03-HUNG-01 cần evidence AWS thật: dựng, quyền/kết nối, S3/DDB operations,
sentinel/hash qua pause/resume, inventory sau thu hồi và chi phí. Mock test có thể
chứng minh graph giữ tài nguyên, không chứng minh dữ liệu/cloud/teardown thật đã đạt.
