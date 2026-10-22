# Chuẩn bị môi trường AWS dev

Ngày kiểm tra: 2026-10-22. IAM operator/deploy đã bootstrap trước đó; các policy
mở rộng trong lượt này chưa gắn lên AWS. **Không apply workload, chưa Done W03-HUNG-01.**

## Đã chuẩn bị

S3 versioned/SSE-KMS/private, 20 DDB tables có PITR/deletion protection, một EC2
private dùng image AL2023 ARM64 đã pin, root 8 GiB theo phiên và data EBS 20 GiB giữ
lại. Không format ổ, seed metadata/secret, cài app hay tự chạy lifecycle AWS.
Admin shell dùng Session Manager với log/data channel mã hóa; không public SSH,
Admin hoặc metrics. [Schema và ảnh hưởng adapter](metadata-tables.md),
[quy trình bật/nghỉ](dev-sessions.md).

`paused=true` thu hồi compute và paid networking, giữ data/key/state. Chạy bình
thường theo phiên; chưa có lịch tự bật, automatic shutdown hoặc hard spending cap.
AWS Budgets là cảnh báo có độ trễ. Không coi stopped EC2 là đã dừng mọi phí.

## Bằng chứng và giới hạn

- Operator MFA/no access key và CLI role đã kiểm ở bước bootstrap; identity gate PASS.
- Full fresh empty-state plan: active single-NAT Warm **76 creates**, paused **70
  creates**. Đây là hai plan từ rỗng, không phải diff pause/resume của AWS thật.
- Full plan dùng root session **chỉ đọc** để resolve AMI khi deploy role chưa nhận
  DescribeImages mới. Không apply bằng root. Provider qua profile credential_process
  được kiểm riêng bằng paused plan dưới deploy role.
- Terraform validate và 21 tests PASS; 11 Python tests PASS. Mọi mock apply/cleanup
  không gọi AWS.
- Mock lifecycle kiểm active→pause→resume và interface→pause; giữ S3/DDB/EBS/key/IAM.
  Các diff mock pause/resume được đối chiếu với gate chỉ đổi session resources.
- Access Analyzer không có findings cho policy review; IAM simulator kiểm tạo
  compute bị giới hạn size/IMDS/private/pinned image, teardown chỉ đúng Miles/dev,
  từ chối xóa datastore/volume/key và từ chối runtime IAM/state access.
- Full policy baseline sinh bằng IAM Policy Autopilot 0.3.0 từ private active plan;
  được refine theo IAM path, tài nguyên thực, bounds và teardown operations.
  Không coi simulation/plan là chứng minh mọi API apply đã có đủ quyền.

Config thật và full plan nằm ngoài Git, file 0600/thư mục 0700. Bản riêng giữ
`paused=true`, `enable_compute=true`, NAT dự kiến, ALB tắt, AMI đã kiểm tra ở
Singapore và boundary. Backend bucket chưa được tạo; profile CLI gốc giữ nguyên,
profile phụ `miles-dev-terraform` dùng credential_process, không lưu raw keys.

## Dự toán để review

Public On-Demand USD, Singapore, AWS Price List API ngày 2026-10-22, chưa thuế/credit.
Đơn giá: NAT 0,059 USD/giờ + 0,059 USD/GB; IPv4 0,005 USD/giờ; t4g.small Linux
0,0212 USD/giờ; gp3 0,096 USD/GiB-tháng; KMS 1 USD/key-tháng; secret metadata
0,40 USD/secret-tháng. Nguồn dịch vụ:
[VPC](https://aws.amazon.com/vpc/pricing/), [EC2](https://aws.amazon.com/ec2/pricing/on-demand/),
[EBS](https://aws.amazon.com/ebs/pricing/), [KMS](https://aws.amazon.com/kms/pricing/),
[Secrets](https://aws.amazon.com/secrets-manager/pricing/).

Ví dụ giữ 20 GiB data + một key + một secret cả tháng; EC2/NAT/IPv4/root 8 GiB
chỉ tồn tại trong phiên. Decimal, quy ước tháng 730 giờ cho ước lượng root disk:

| Giờ chạy/tháng | Fixed estimate USD |
|---|---:|
| 0 — đã pause | 3,32 |
| 24 | 5,39 |
| 96 | 11,60 |
| 730 | 66,28 |

Công thức: 3,32 + (0,059 + 0,005 + 0,0212) × giờ + 8 × 0,096 × giờ / 730.
**Chưa gồm** S3/DDB storage/requests/PITR, logs, KMS/secret requests, NAT data,
Internet/cross-AZ transfer, snapshots, ALB hoặc state storage. Không phải tổng bill
hay cam kết trần chi phí. CPU credits standard có thể throttle thay vì tính surplus.
Interface mode 9 services × 2 AZ riêng endpoint-hours khoảng 170,82 USD/730 giờ,
chưa data processing; không phù hợp để treo dev liên tục trong lúc chờ W2.

## Trước khi đề nghị Done

Review schema vật lý với adapter; review/cấp policy mới, budget cho phiên và saved
plan thực. Sau khi được duyệt apply: kiểm state locking/backend, SSM Online/log
stream, mount đúng data volume, permissions/API routes, S3/DDB smoke operations,
sentinel/hash qua pause/resume và inventory/chi phí sau thu hồi. App/SigV4/storage
integration phụ thuộc phần W2/W3 của nhóm; chưa có evidence Cloud chạy thật.
