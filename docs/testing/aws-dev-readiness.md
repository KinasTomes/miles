# Kiểm tra môi trường AWS dev Warm

Task: W03-HUNG-01. Kiểm AWS thật ngày 25/10/2026 (UTC+7), sau khi đồng bộ
`main` tại `65e9c04`. Phiên được lead cho phép chạy thật, dùng deploy role;
phiên root riêng chỉ kiểm/cập nhật IAM và đã đăng xuất sau khi hoàn tất.

## Bằng chứng AWS thật

| Phần kiểm | Kết quả |
|---|---|
| Terraform state | S3 riêng, versioning/SSE-S3/public block/TLS-only; một plan đồng thời bị S3 state lock từ chối |
| IAM | Template qua Access Analyzer không có findings; 28 simulator cases đạt, gồm tài nguyên có/không có ownership và từ chối xóa data/key; apply workload dưới deploy role |
| Datastore | 20 bảng ACTIVE, on-demand cap 10 read/10 write, SSE-KMS, PITR 1 ngày, deletion protection; S3 versioned/SSE-KMS/public block/BucketOwnerEnforced |
| Network/compute | Một t4g.small private, không public IP, IMDSv2/hop limit 1, CPU credits standard; một NAT, hai gateway endpoints S3/DDB; không ALB, không mở SSH/Admin/metrics Internet |
| Session Manager | Node Online, vào được đúng document audit; KMS data channel và CloudWatch streaming log mã hóa, marker lệnh đã tới log group retention 14 ngày; shell mặc định/port forwarding bị từ chối |
| Runtime I/O | Từ instance profile: S3 put/get SHA-256, SSE-KMS và version khớp; đọc checkpoint control object tách biệt; DynamoDB conditional put/consistent get đạt; truy cập state và IAM bị từ chối |
| Pause/resume | Hai lần bật, hai lần nghỉ; instance thay mới, cùng data-volume filesystem UUID; EBS/S3 SHA-256, S3 version và DynamoDB record khớp qua lần nghỉ; probe EBS/DDB đã dọn |

Data EBS mới được đối chiếu VolumeId/serial, xác nhận không có filesystem/signature,
không phải root rồi mới tạo ext4. Mỗi lần dừng đều sync/unmount; không có DSG/jobs
đang chạy để drain. Lần resume phải dùng filesystem cũ, không format lại.

Bản ghi DynamoDB và file EBS kiểm thử được dọn sau vòng đối chiếu. Hai object S3
versioned dưới prefix acceptance được giữ làm sentinel; gateway không được cấp
thêm quyền xóa object/version chỉ để dọn kiểm thử. Không ghi active-history pointer
hoặc dữ liệu nghiệp vụ. Inventory, plan/state, log chi tiết, UUID/version/hash và
định danh AWS nằm ngoài Git trong kho evidence riêng tư; public chỉ ghi kết quả.

## Sửa lỗi phát hiện khi chạy thật

- Terraform giữ `-var=paused=true/false` thành chuỗi trong JSON input. Gate hiện nhận
  boolean hoặc đúng chuỗi chuẩn, tiếp tục từ chối kiểu/giá trị khác; có regression test.
- Bổ sung đúng `ssm:DescribeParameters`, KMS EBS data-key/re-encryption qua EC2 và
  quyền đọc inventory ALB theo Region. Không cấp object/data mutation cho deploy.
- Dùng `BoolIfExists` cho SessionDocumentAccessCheck khi IAM đánh giá từng resource;
  node vẫn cần tags Miles/dev và chỉ custom document audit được cấp StartSession.
- Gate đã chặn các thay đổi computed sau khi resource con được tạo. Review riêng
  refresh-only rồi mới tạo/gate session plan; không bỏ kiểm drift.
- Provider 6.21.0 giữ association EIP cũ sau khi NAT bị xóa, làm pause lần đầu dừng
  ở EIP. Quy trình đối chiếu association, refresh-only và plan pause mới đã thu hồi
  được EIP; xem [phục hồi pause dở](../../infra/terraform/cloud/dev-sessions.md#đối-chiếu-state-và-xử-lý-pause-dở).
  Không mở quyền disassociate toàn Region. Không coi lần apply đầu là bằng chứng
  đã hết tài nguyên tính phí.

## Kiểm offline và CI

Terraform 1.15.2/AWS provider 6.21.0 giữ nguyên lockfile. Fmt, init/validate
bootstrap + dev đạt; 21 Terraform mock tests, 12 Python guard tests và `make check`
đạt. CI kiểm Go/race, benchmark contracts và Terraform đạt. Các apply/cleanup trong
Terraform tests đều dùng mock; bằng chứng live trong bảng trên là phiên riêng.
Lệnh tái hiện: [Terraform README](../../infra/terraform/cloud/README.md).

## Chi phí và trạng thái cuối phiên 25/10/2026

Các dự toán ở phần này thuộc phiên trước full teardown ngày 26/10, không đại diện
chi phí còn lại sau thu hồi. Kết thúc phiên 25/10 ở `paused=true`: API AWS xác nhận không còn EC2 đang tồn tại, NAT đang
hoạt động, EIP, interface endpoints, ALB hoặc root volume mồ côi của môi trường.
Data EBS detached vẫn có, S3/DDB/key/secret/state và cấu hình được giữ để bật lại.

Price List công khai Singapore đối chiếu 25/10/2026: NAT 0,059 USD/giờ + 0,059
USD/GB, IPv4 0,005 USD/giờ, t4g.small Linux 0,0212 USD/giờ, gp3 0,096 USD/GiB-tháng,
CMK 1 USD/key-tháng, secret metadata 0,40 USD/tháng. Khi bật: fixed rate khoảng
0,0863 USD/giờ gồm root disk 8 GiB; khi nghỉ: EBS 20 GiB + key + secret khoảng
3,32 USD/tháng. Chưa gồm S3/DDB/PITR/log/state/requests, data transfer, thuế/credit.
NAT tính mỗi phần giờ thành tròn một giờ, nên hai lần tạo NAT ngắn vẫn có ít nhất
hai NAT-hours; không nhân toàn bộ fixed rate với vài phút để gọi đó là hóa đơn.
Chưa đối chiếu bill/Cost Explorer đã chốt; các con số trên là dự toán, không phải
chi phí 0 hoặc hard cap.

Nguồn: [EC2 regional Price List](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/ap-southeast-1/index.csv),
[VPC regional Price List](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonVPC/current/ap-southeast-1/index.json),
[KMS regional Price List](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/awskms/current/ap-southeast-1/index.json),
[Secrets Manager Price List](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSSecretsManager/current/ap-southeast-1/index.json),
[quy tắc tính NAT/IPv4](https://aws.amazon.com/vpc/pricing/).

## Phần còn lại trước Done

### Full teardown ngày 26/10/2026

Đăng nhập lại đúng deploy role; trước teardown API xác nhận vẫn pause. Audit có
phân trang: cả 20 bảng Scan COUNT nhất quán bằng 0; S3 chỉ có đúng hai acceptance
versions khớp evidence cũ, không delete marker/multipart; secret không có version;
không có backup DynamoDB hoặc snapshot EBS dùng runtime key. Tám audit log events
đã lưu riêng ngoài Git. EBS chưa mount lại filesystem; việc xóa volume 20 GiB
không snapshot đã được duyệt rõ trước khi thực hiện.

Cùng remote backend thật, đã review/apply plan chỉ update deletion protection của
20 bảng, xóa đúng hai S3 versions, rồi lập/review destroy plan mới xóa đúng 70
resource dev, giữ bootstrap/state. Quyền cleanup được generate từ plan bằng IAM
Policy Autopilot 0.3.0 rồi thu hẹp tới action thiếu và ARN thực; bỏ replication,
snapshot creation, Object Lock bypass và dịch vụ không dùng. Candidate qua Access
Analyzer không findings, 28 simulation cases; chỉ cho window KMS/secret 30 ngày.

Lần apply đầu xóa bảng thành công nhưng waiter DescribeTable gặp AccessDenied vì
policy thường ngày yêu cầu tags của bảng đã mất. Grant audit tạm bổ sung
DescribeTable trên đúng 20 ARN, không phụ thuộc tags, giữ cùng hạn dùng; validation
không findings và 18 simulation cases đạt. API xác nhận 20 bảng ResourceNotFound;
fresh recovery plan chỉ còn xóa runtime key. So sánh raw state với refreshed prior
values đã giải thích 20 bảng mất, không dùng state rm hoặc áp lại saved plan cũ.

Nghiệm thu sau recovery:

| Phần | Kết quả |
|---|---|
| Remote dev state | Không còn managed resource |
| Datastore | Bucket và 20 bảng đều không còn qua API; hai versions đã xóa, EBS đã xóa theo quyết định không backup |
| Runtime/network | Không còn EC2/NAT/EIP/ALB/volume/VPC/subnet/route table/SG/endpoints/IGW; runtime roles/profiles/SSM document/config/log group đã mất |
| Bootstrap | State bucket vẫn versioned/SSE-S3/public block; operator/deploy/boundary được giữ |
| Quyền tạm | Đã gỡ cả audit và cleanup grants; 7 principal simulation cases xác nhận quyền xóa/Scan bị từ chối trở lại; trust và 10 managed policy attachments không đổi; root đã logout |

KMS đang PendingDeletion, API ghi ngày dự kiến 25/11/2026 UTC. Secret đã được đánh
xóa với recovery window 30 ngày; chưa coi là đã purge vật lý. ListBackups ALL sau
teardown thấy 20 SYSTEM backups tự sinh từ PITR, API hiện báo BackupExpiryDateTime
ngày 27/10/2026 UTC. Đây là trạng thái quan sát được, chưa chờ đến hạn để kiểm việc
xóa cuối cùng. Không claim mọi tài nguyên AWS đã biến mất hoặc chi phí bằng không.
Nguồn mô tả SYSTEM backup và trường expiry: [AWS BackupSummary](https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_BackupSummary.html).

Source dev vẫn giữ bốn prevent_destroy, DynamoDB deletion protection và
force_destroy=false. 12 Python guard tests, Terraform fmt, runbook shell syntax
và CI 3/3 tại head chuẩn bị `1ee9cd6` đạt; grant audit sửa waiter đã có bằng chứng
IAM thật nêu trên. [Runbook](../../infra/terraform/cloud/full-teardown.md) ghi cách
review/apply/recover. Inventory/state/plans/policy/UUID/identifiers lưu riêng ngoài
Git; public chỉ ghi kết quả tổng hợp.

Cả ba checklist có bằng chứng live, gồm thử destroy môi trường disposable. Task
chưa tự coi Done trước review/CI/merge PR #30. Môi trường dev đã thu hồi; task tích
hợp AWS cần dựng lại khi dùng, lưu ý tên secret còn trong recovery window cần
restore/reconcile hoặc chờ xóa trước khi tạo lại.

Hai hard dependency W01-HUNG-03/W02-HUNG-01 và các task W2 liên quan đã Done;
không còn chờ W2 để kiểm hạ tầng. [Mapping 20 bảng](../../infra/terraform/cloud/metadata-tables.md)
là bàn giao cho adapter DynamoDB W03-HAI-03/W4. Phiên này chưa deploy DSG, chưa kiểm
S3/Admin HTTP end-to-end, adapter DynamoDB production, recovery R1/R2 hoặc multi-node.
Interface mode và ALB chưa được nghiệm thu live. Runbook vận hành:
[dev sessions](../../infra/terraform/cloud/dev-sessions.md).
