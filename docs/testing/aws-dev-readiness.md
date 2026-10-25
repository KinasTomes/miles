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

## Chi phí và trạng thái cuối

Kết thúc ở `paused=true`: API AWS xác nhận không còn EC2 đang tồn tại, NAT đang
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

Checklist dựng IaC và kiểm quyền/network/control objects có bằng chứng live.
Inventory và pause/resume đã kiểm; **full destroy môi trường disposable chưa chạy**.
Việc đó cần review/xác nhận riêng dữ liệu cần giữ, backup/retention, gỡ bảo vệ data
và quyền cleanup giới hạn, rồi nghiệm thu việc thu hồi thật. Giữ task Doing và
checklist full destroy chưa tick; PR #30 vẫn chờ review.

Hai hard dependency W01-HUNG-03/W02-HUNG-01 và các task W2 liên quan đã Done;
không còn chờ W2 để kiểm hạ tầng. [Mapping 20 bảng](../../infra/terraform/cloud/metadata-tables.md)
là bàn giao cho adapter DynamoDB W03-HAI-03/W4. Phiên này chưa deploy DSG, chưa kiểm
S3/Admin HTTP end-to-end, adapter DynamoDB production, recovery R1/R2 hoặc multi-node.
Interface mode và ALB chưa được nghiệm thu live. Runbook vận hành:
[dev sessions](../../infra/terraform/cloud/dev-sessions.md).
