# Kế hoạch nghiệm thu AWS Edge, recovery và alarms

Lập ngày 10/11/2026, chờ Hưng duyệt. Các bước đăng nhập và kiểm tra chỉ đọc đã được cho phép và đã thực hiện. Lịch dưới tính từ lúc tạo tài nguyên tính phí đầu tiên, gọi là T0.

## Phạm vi xin duyệt

- Một lượt tại Singapore `ap-southeast-1`, trong account đã xác nhận bằng STS. Workload, dataset và các node chạy trên AWS. Oracle dùng làm máy điều khiển CLI.
- Ngân sách **10 USD trước thuế**. Dự toán thận trọng **6,32 USD**, dự phòng **3,68 USD**. Tính giá On-Demand, bỏ qua credit và Free Tier khi lập dự toán.
- Tối đa **4 giờ**, gồm bootstrap, kiểm thử và cleanup. Ghi T0 cùng các deadline theo giờ Việt Nam vào hồ sơ riêng trước khi bắt đầu.
- Thường trực 2 Cloud, 1 Edge và 1 client bằng `t4g.small` ARM64, CPU credit Standard. Tối đa 5 máy đồng thời khi thay một node, tổng tối đa 100 GiB root gp3 mã hóa.
- Một ALB HTTPS nội bộ, mạng hai AZ, tối đa 10 interface endpoints trên hai AZ và hai gateway endpoints S3/DynamoDB. Đường quản trị riêng qua SSM.
- Hai bucket dữ liệu/release và baseline, thêm tối đa một bucket GC tạm. 20 bảng runtime và tối đa 20 bảng fixture tồn tại đồng thời. Các bộ native fixture chạy tuần tự và dọn trước bộ kế tiếp.
- Đúng 8 alarm runtime cho Cloud/Edge. Một KMS key và một secret mới. TLS bằng CA kiểm thử tự ký và import ACM. Dataset tổng hợp `backup30`, 90 object khoảng 8 MiB/object, xác minh hash trước upload.

## Dự toán

Giá EC2, ALB, DynamoDB, PrivateLink, CloudWatch và KMS được lấy từ AWS Price List CSV Singapore ngày 10/11/2026. EBS và các dòng dành riêng là khoản dự trù. Bản tính sử dụng Decimal lưu riêng ngoài Git.

| Hạng mục | Cách tính hoặc lượng dành riêng | USD |
|---|---|---:|
| EC2 | 5 máy × 4 giờ × 0,0212 USD/giờ | 0,4240 |
| EBS gp3 | 100 GiB × mức dự trù 0,12 USD/GiB-tháng × 4/730 giờ | 0,0658 |
| Interface endpoints | 10 dịch vụ × 2 AZ × 5 giờ × 0,013 USD/endpoint-AZ-giờ | 1,3000 |
| ALB + LCU | 5 giờ × (0,0252 USD + 2 LCU × 0,008 USD) | 0,2060 |
| DynamoDB + PITR/storage | Dành 1,20 USD, gồm tổng 1 triệu WRU và 2 triệu RRU cho tất cả bảng, giá Standard 0,71 và 0,1425 USD/triệu, cộng storage/PITR | 1,2000 |
| CloudWatch | 640 metric identities × 0,30 USD/tháng × 5/730 giờ, 8 alarm × 0,10 USD/tháng × 5/730 giờ và 1 GiB logs × 0,70 USD | 2,0205 |
| S3 storage/requests | Dành riêng cho dataset, release, versions và fixture GC | 0,4000 |
| Transfer và xử lý endpoint | Dành riêng, chủ yếu traffic trong Region và xuất evidence rút gọn | 0,5000 |
| KMS, secret và API liên quan | Dành riêng, gồm thời gian sử dụng trước khi schedule deletion | 0,2000 |
| **Tổng dự toán** | Tổng tính bằng script | **6,3163** |
| **Dự phòng** | Phần còn lại trong ngân sách | **3,6837** |
| **Mức xin duyệt** | Trước thuế | **10,0000** |

Endpoint và ALB được tính 5 giờ để dự phòng làm tròn giờ trong lượt có tối đa 4 giờ thực tế. DynamoDB tính theo billed request units, gồm transaction/index. Theo dõi consumed units, số metric identities và lượng logs để dừng sớm. AWS billing có độ trễ nên mức này là ngân sách vận hành, không phải hard cap do AWS cưỡng chế.

Nguồn để kiểm tra lại giá

- [EC2 Singapore](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/ap-southeast-1/index.csv)
- [ALB Singapore](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSELB/current/ap-southeast-1/index.csv)
- [PrivateLink Singapore](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonVPC/current/ap-southeast-1/index.csv)
- [DynamoDB Singapore](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonDynamoDB/current/ap-southeast-1/index.csv)
- [CloudWatch Singapore](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonCloudWatch/current/ap-southeast-1/index.csv)
- [KMS Singapore](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/awskms/current/ap-southeast-1/index.csv)

## Lịch và kết quả cần thu

| Khoảng từ T0 | Công việc | Điều kiện chuyển bước |
|---|---|---|
| Phút 0–45 | Foundation, Cloud/Edge/client, TLS, release, SSM, Roles Anywhere, fixtures và recorder Chrony/CPU. Tính cả propagation versioning S3 | Đủ node healthy, clock source tin cậy, đường Admin riêng và evidence hoạt động |
| Phút 45–90 | Edge C1 rồi C4. Native R1 lease/fencing và R2 seal/checkpoint/GC trên backend AWS, fixture tuần tự | Mỗi workload đủ 90 PUT, 90 HEAD, 360 GET đúng SHA-256, không có 500/503 hoặc outcome chưa xác định |
| Phút 90–190 | Tạo tín hiệu lỗi có kiểm soát, thu 8 alarm OK → ALARM → OK. Bao gồm cut age trên 1 giờ và thời gian hồi phục | Native history, datapoint và telemetry còn sống. Phân biệt lỗi SDK tiêm vào với throttling từ AWS |
| Phút 190–240 | Thu evidence, drain, xóa dữ liệu tổng hợp, teardown, đối chiếu baseline và thu hồi IAM | Dọn tài nguyên của lượt, khôi phục quyền, ghi rõ ngoại lệ chờ xóa |

GC native dùng node trong tiến trình và clock mô hình, báo riêng với bài runtime nhiều máy. Nếu một bước lỗi, dừng workload mới, thu bằng chứng rồi cleanup. Các bài chưa chạy được ghi rõ trong báo cáo.

## IAM và điều kiện trước apply

Đề xuất duyệt gắn/gỡ IAM tạm tối đa 6 giờ cho đúng lượt này và lưu baseline để khôi phục. Root chỉ thực hiện IAM và audit. Deploy role vận hành workload. Dùng credential ngắn hạn qua `aws login`. Làm trống file mã trước mỗi lần lấy code, xóa mã sau khi sử dụng.

Hoàn tất các điều kiện kỹ thuật sau trong phạm vi được duyệt trước khi apply.

1. Hoàn thiện và kiểm thử cấu hình Edge/client riêng tư, ALB nội bộ, SSM và bootstrap dependencies. Endpoint Roles Anywhere đã có test Terraform, còn phải nối security group và danh tính SSM-only của Edge.
2. Diễn tập quy trình alarm để giai đoạn chờ hơn 1 giờ và cleanup nằm trong cửa sổ 4 giờ.
3. Hoàn thiện đối chiếu cleanup, gồm tài nguyên ngoài state và quyền audit cuối cùng. Verifier hiện còn trả `UNVERIFIED` cho phần chưa chứng minh đủ.
4. Build ARM64 từ source cuối đã qua CI, tạo dataset/TLS mới và chốt hash. Lấy fresh baseline và saved plans thật, thay placeholder, rà IAM/boundary, Access Analyzer và simulation cho thao tác tạo/chạy/xóa.
5. Ghi hash plan/release/dataset và T0 vào hồ sơ riêng. Kiểm tra resource nằm trong giới hạn trên. Thay đổi vượt ngân sách, thời gian, số máy, quyền hoặc phạm vi xóa thì trình duyệt lại.

Preflight ngày 10/11 xác nhận quota EC2 Standard 16 vCPU, Cloud state có 0 managed resource, inventory Singapore có 0 EC2/EBS/ASG/ALB/DynamoDB/alarm. Source candidate `b39e89a`. Hai plan thăm dò 99 và 132 resource dùng giá trị giả và overlay cũ, chỉ phục vụ ước lượng. Sáu policy candidate qua Access Analyzer nhưng còn 10 cảnh báo wildcard từ công cụ sinh policy cần rà. Các hash artifact cuối sẽ được ghi sau khi hoàn thiện code.

## Dừng sớm và cleanup

- Tại T0 + 180 phút dừng bắt đầu bài mới, chậm nhất T0 + 190 phút chuyển cleanup. Chuyển cleanup sớm nếu dự báo chi phí đạt 7,50 USD, lượng metric/log/request vượt dự trù hoặc mất khả năng quan sát/cleanup.
- Xuất bằng chứng, xóa versions, delete markers, multipart và dữ liệu synthetic. Xóa fixture tables trước, sau đó runtime tables và hạ tầng theo reviewed destroy plan.
- Cho phép xóa tối đa 5 root disk 20 GiB **không backup**, gồm disk node thay thế. Dọn ACM import, identity thử, runtime IAM, endpoints và networking thuộc lượt.
- KMS và secret mới chờ xóa 30 ngày, ghi ARN và trạng thái native. Giữ bootstrap/state và tài nguyên có trước lượt. Baseline hiện có 11 customer KMS key và 11 secret đang chờ xóa, cùng các bucket có sẵn.
- Khi quá hạn hoặc phiên hết hạn, ưu tiên đăng nhập lại và cleanup đến khi dọn xong, báo ngay thời gian/chi phí phát sinh. Báo cáo cuối ghi kết quả từng bài, residual, IAM diff và ước tính chi phí. Đối soát billing khi AWS cập nhật.

## Nội dung xác nhận

Anh có thể duyệt bằng câu **“Duyệt IAM + AWS/cleanup tối đa 10 USD trước thuế, tối đa 4 giờ theo plan này, xóa dữ liệu synthetic và tối đa 5 root disk không backup.”**

Phê duyệt bao phủ việc tiếp tục hoàn thiện kỹ thuật và chạy ngay khi các điều kiện trước apply đạt trong cùng phạm vi trên. Hiện tại chờ phê duyệt và còn điều kiện kỹ thuật cần hoàn tất.
