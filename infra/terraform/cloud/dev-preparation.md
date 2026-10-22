# Chuẩn bị môi trường AWS dev

Ngày kiểm tra: 2026-10-22. Phần IAM operator/deploy đã được bootstrap; hạ tầng
workload chưa apply. W03-HUNG-01 chưa hoàn tất.

## Kết quả đã kiểm tra

- Operator có MFA, đã đổi mật khẩu, không có access key. CLI profile `miles-dev`
  dùng session `miles-dev-deploy`; deployment identity gate PASS.
- Role đọc được VPC/security groups/log groups tại Singapore; EC2 Sydney bị từ chối.
- Terraform validate PASS; 11 mock plan tests PASS, gồm từ chối boundary khác account
  và kiểm cả hai runtime role nhận boundary. Năm Python tests PASS.
- Fresh local foundation plan NAT: 46 creates, không sửa/xóa; hai runtime role
  có boundary. State bootstrap plan: 5 creates. Không phải remote-state/drift check.
- Quyền IAM đã qua Access Analyzer và các ca simulator nêu ở
  [operator bootstrap](bootstrap/operator/README.md). Chưa thử apply thực tế.

Config thật nằm ngoài Git, gồm account input, Singapore, single NAT, ALB tắt,
CIDR client/admin rỗng và runtime boundary. Backend dự kiến dùng profile
`miles-dev`, S3 encryption và native lockfile. Bucket backend chưa được tạo.
Full plan, mật khẩu, authorization code và raw credentials không là PR artifact.

## Dự toán công khai để review

Nguồn: AWS Price List API, location `Asia Pacific (Singapore)`, đọc 2026-10-22.
Public On-Demand USD, chưa tính thuế/credit; đây là phép tính theo giả định sử dụng,
không phải hóa đơn hoặc giá trọn gói cho workload.

| Thành phần | Đơn giá |
|---|---:|
| NAT Gateway | 0,059 USD/giờ + 0,059 USD/GB xử lý |
| Public IPv4 của NAT | 0,005 USD/giờ |
| EC2 t4g.small Linux | 0,0212 USD/giờ |
| EBS gp3 | 0,096 USD/GiB-tháng |
| KMS key | 1 USD/key-tháng |
| Secrets Manager | 0,40 USD/secret-tháng |

Giả định giữ một KMS key và một secret cả tháng; NAT/IP và EC2 chỉ chạy theo số giờ
trong bảng. Ví dụ Warm thêm một t4g.small và 20 GiB gp3 giữ cả tháng; EC2/EBS này
chưa được khai báo trong foundation hiện có. Tính bằng Decimal, làm tròn 2 chữ số:

| Phương án | 24 giờ/tháng | 96 giờ/tháng | 730 giờ/tháng |
|---|---:|---:|---:|
| Foundation: 1 NAT + IPv4 + key + secret | 2,94 | 7,54 | 48,12 |
| Ví dụ Warm: foundation + EC2 + EBS | 5,36 | 11,50 | 65,52 |

Foundation = (0,059 + 0,005) × giờ + 1 + 0,40.
Warm = foundation + 0,0212 × giờ + 20 × 0,096.

Chưa gồm request/storage S3/DynamoDB, PITR, logs, KMS/Secrets API calls,
NAT data, Internet/cross-AZ transfer hoặc CPU surplus credits. Không giả định
Free Tier/credit. Mốc 96 giờ là ví dụ, chưa có cơ chế tự tắt hoặc hard spending cap.
Ngừng EC2 không dừng phí NAT/EIP/EBS/key/secret.

## Trước khi triển khai tính phí

Chốt số giờ chạy và chi phí; hoàn thiện quyền và plan teardown phần mạng tính phí
mà vẫn giữ state/data/KMS. Apply state bootstrap rồi kiểm locking/backend thực.
Review remote plan dưới đúng role trước apply foundation. Bổ sung data bucket,
DynamoDB, compute và runtime config theo phạm vi task; không coi 46 tài nguyên
foundation là môi trường Warm đã chạy. ALB vẫn cần domain/certificate/CIDR thật
nếu quyết định đưa HTTPS vào phạm vi triển khai.
