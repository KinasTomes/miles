# Inventory mạng, IAM và chi phí W02-HUNG-01

Ngày tra: 2026-10-19; Region `ap-southeast-1`; currency USD; public On-Demand rates, chưa tính thuế/credit. Nguồn hợp đồng và cách chạy ở [README](README.md). Tất cả tài nguyên dưới đây là **Terraform planned**, chưa apply.

## 1. Network inventory

| Thành phần | Số lượng / route | Ranh giới |
|---|---|---|
| VPC | 1; mặc định10.0.0.0/16, DNS support/hostnames | Khác default VPC; cần xác nhận overlap trước apply |
| Subnets | 2 public +2 private ở AZ1a/1b | Không tự gán public IPv4 cho instance; chưa launch instance |
| Route tables | 1 public→IGW;2 private→S3/DDB endpoint | Private không có default Internet route ở isolated/interface |
| Gateway Endpoints | S3, DynamoDB; gắn cả private route tables | Endpoint policy allow đúng backend bucket và20 table ARNs/indexes; không thay IAM role |
| NAT optional | Dev1NAT/EIP ở AZ đầu; `nat_per_az=true`→2NAT/EIP | SingleNAT có phụ thuộc AZ/cross-AZ charge, không phải full HA; NAT không có nghĩa mọi HTTPS destination được IAM cho phép |
| Interface optional |8services×2AZ ENIs/private DNS | kms,ssm,secretsmanager,logs,monitoring,sts,elasticloadbalancing,autoscaling; AWS service names đã query và có ở Singapore |
| sg-alb | In443 từ client_cidrs; out8080/9100→sg-gateway | CIDR mặc định rỗng, ALB mặc định tắt; metrics không có public listener |
| sg-gateway | In8080 từ ALB;9100 từ ALB/monitor;9000 từadmin | Out443 cho AWS API; route/endpoints quyết định reachable;7000 chưa mở vì ringP2 chưa bật |
| sg-admin | In22 từadmin_ssh_cidrs;out9000→gateway | Bastion/SSH tunnel theo kiến trúc; SG chưa tự tạo bastion/SSH user/key |
| sg-monitor | Out9100→gateway | Chưa provision monitoring host |
| sg-jobs | Out443 | Không inbound public; EC2 worker ở task sau |
| sg-endpoints optional | In443 từgateway/jobs | Stateful return traffic; private DNS; không cho client trực tiếp |
| ALB optional | Public ở2AZ,443TLS→8080; health9100/readyz | Preserve Host, idle120, drain1800; chưa gắn targets/ASG |

AWS API route matrix:

| Dịch vụ / traffic | isolated | nat | interface |
|---|---|---|---|
| S3 data bucket /DDB tables | Gateway Endpoints | Gateway Endpoints, không qua NAT | Gateway Endpoints |
| KMS,SSM,Secrets,CloudWatch logs/metrics | Không có đường từ private | HTTPS qua NAT | Private DNS Interface Endpoints |
| STS regional (khi cần AssumeRole), ELB/ASG drain APIs | Không có đường | NAT, configure regional STS endpoint | Interface Endpoints; dùng regional STS hostname |
| Runtime EC2 role credentials | IMDS local theo launch config W4; không gọi CLI root | Như isolated | Như isolated |
| OS apt/package/GitHub/container registry, external Internet | Không có đường | Có HTTPS route, SG chỉ443 | Không có đường; bake dependencies trong AMI trước |
| ECR/SSM Session Manager | Không được khai báo là sẵn sàng | Cần IAM/runtime config thêm ở task deploy | Chưa có ecr.api/ecr.dkr/ssmmessages endpoints; không tự thêm ngoài nhu cầu |

Chọn **isolated cho W2 plan**; **single NAT theo phiên dev là đề xuất mặc định khi cần runtime Internet**, giữ option interface cho workload chỉ gọi AWS đã đóng gói sẵn. NAT/Interface đều tắt sau review bằng Terraform khi không dùng, không áp dụng việc xóa lên data/key/recovery roots. Two-AZ singleNAT không chịu lỗi mất AZ của NAT; dùng per-AZ nếu bài test yêu cầu network HA và ngân sách cho phép. Không cần thêm AWS service ngoài kiến trúc chỉ để lấp đường mạng.

## 2. IAM / crypto / secret inventory

| Principal / tài nguyên | Quyền dự kiến | Ranh giới và bước còn lại |
|---|---|---|
| Gateway EC2 profile | List/get backend, Put/restore/MP backend tại packs/packs-index/journal/checkpoints; CRUD/query/scan/condition check20tables+indexes; runtime crypto; get bootstrap parameter/secret; put logs/metrics | EC2 service trust, không DSG user grant, không admin IAM, không S3 DeleteObject; không ghi recovery catalog; không state access |
| Jobs EC2 profile | Quyền runtime nhưgateway + DeleteObject tại4prefix; deregister đúng targetgroup miles-dev-s3; heartbeat/complete lifecycle ASG miles-dev-* | Destructive action vẫn cần R1/fence/retention trong app. Runtime gateway muốn tự điều khiển drain ELB/ASG phải dùng worker jobs role/orchestration đã thiết kế; không giả gateway đã có quyền này |
| KMS runtime | Key rotation, account IAM delegation; gateway/jobs Encrypt/Decrypt/GenerateDataKey/DescribeKey đúng key | Key admin không cấp runtime; prevent_destroy giữ key, deletion window30days không là permission xóa data sớm; SSE-KMS/Credentials theo thiết kế |
| Secrets Manager | Metadata secret miles-dev/runtime-secrets, CMK runtime, recovery30days | Chưa có version/token/key; operator provisioning ngoài TF; file tfvars/config không chứa secret |
| SSM | String /miles-dev/bootstrap-config, region/secret ARN/schema version | Non-secret, không là policy/grants authoritative. SystemConfig thật ở DDB |
| Log group | /miles-dev/runtime, retention14days | Runtime ghi stream, không tạo log group tùy ý; namespace PutMetricData=Miles/DSG |
| Deploy/bootstrap/offline recovery | Chưa tạo role vì chưa có trusted principal được xác định | Cần operator role trước apply; recovery catalog chỉ offline operator, không online runtime. State permissions riêng theoREADME |

IAM source schema20tables lấy KT §10.1; chưa provision chúng/data bucket ở task này, chỉ cố định naming contract cho ARN inventory. W3–4 cần tạo/import đúng tên hoặc sửa input/naming cùng adapter trước apply. Transaction IAM dùng item-level actions (Get/Put/Update/Delete/ConditionCheckItem); không thêm action giả TransactWriteItems/TransactGetItems. CloudWatch PutMetricData không có resource ARN, dùng `Resource=*` kèm namespace condition; KMS key policy `Resource=*` chỉ áp key đang gắn policy. Endpoint policy `Principal=*` là filter dùng kèm identity policies, không tự cấp user quyền AWS. EC2 service trust không thêm SourceArn giả chưa được EC2 gửi trong AssumeRole.

## 3. Đơn giá và công thức kiểm chứng

Đơn giá lấy bằng `aws pricing get-products --region us-east-1`, `location=Asia Pacific (Singapore)`. Lệnh read-only, không bật Cost Explorer. Services: AmazonEC2 (productFamily=NAT Gateway), AmazonVPC, AWSELB. Các giá lọc theo usagetype và operation **LoadBalancing:Application**, không lấy nhầm Classic/NLB/Outposts.

| Usage / nguồn API | Giá đã đọc | Thành phần chi phí |
|---|---|---|
| APS1-NatGateway-Hours /AmazonEC2 |0.059 USD/NAT-hour | Count×giờ, cộng EIP và data/cross-AZ |
| APS1-NatGateway-Bytes /AmazonEC2 |0.059 USD/GB | Dữ liệu qua NAT, S3/DDB gateway route không tính NAT |
| APS1-VpcEndpoint-Hours /AmazonVPC |0.013 USD/endpoint-AZ-hour |8services×2AZ khi bậtinterface |
| APS1-VpcEndpoint-Bytes /AmazonVPC |0.010 USD/GB cho tier đầu≤1PB/tháng | Các tier sau khác; không áp giá này cho mọi volume |
| APS1-PublicIPv4:InUseAddress /AmazonVPC |0.005 USD/address-hour | EIP NAT và publicIPv4 ALB/instance nếu có |
| APS1-LoadBalancerUsage, LoadBalancing:Application /AWSELB |0.0252 USD/ALB-hour | Cộng0.008USD/usedLCU-hour và publicIPv4/data transfer |
| S3/DDB Gateway Endpoints |Không phí endpoint-hour/data processing của gateway endpoint | S3/DDB request/storage vẫn tính theo dịch vụ |

Ví dụ **chỉ fixed network hours** (Python Decimal,730giờ/tháng giả định; không phải observed bill/benchmark):

| Phương án |24giờ bật |730giờ bật | Chưa gồm |
|---|---:|---:|---|
|1NAT+1EIP |1.536 |46.720 | NAT processedGB, crossAZ, ALB và phần còn lại |
|2NAT+2EIP |3.072 |93.440 | Như trên |
|8Interface services×2AZ |4.992 |151.840 | endpointGB, ALB và phần còn lại |

Công thức:1NAT=(0.059+0.005)×hours;2NAT=2×(0.059+0.005)×hours; interfaces=8×2×0.013×hours. Các ví dụ chưa tính thuế, credits, Free Tier. Credit và ngân sách nội bộ không được đưa vào ví dụ public; budget chỉ cảnh báo, không là hard cap.

## 4. Inventory chi phí còn phải định lượng trước apply

| Hạng mục | Đơn vị cần đưa vào dự toán | Sau khi tắt EC2 |
|---|---|---|
| VPC/subnets/SG/IGW/routes | Không phí giờ cho chính các cấu phần nền này; traffic có thể tính riêng | NAT/endpoints/IPv4 vẫn tính nếu còn provisioned |
| EC2/ASG/EBS/AMI/bastion | instance-hours, CPU credit nếu burstable, GiB-month, snapshots, publicIPv4 | EBS/snapshots/AMI/EIP còn giữ vẫn có thể tính |
| KMS | key-month +API request | CMK còn tồn tại vẫn tính, không xóa key còn bảo vệ dữ liệu |
| Secrets Manager | secret-month +API calls | Secret metadata/resource vẫn có thể tính, không coi chưa có version là bảo đảm miễn phí |
| SSM String standard | parameter tier +throughput/API features | Xác minh giá nếu bật advanced/high throughput, không lưu secret plaintext |
| S3 state/data | byte-time/class, versions, requests, retrieval/restore/temporarycopy, transfer, minimum duration/size | Stateversions/pack/sidecar/journal/checkpoint và Coldretention vẫn tính |
| DynamoDB | requests/transactions, storage, PITR1day/backup | Không có server để stop; requests/storage/PITR vẫn tính khi phát sinh |
| CloudWatch | log ingestion/storage, metrics/API/alarm | Logs/metrics tồn tại theo retention, job tắt không xóa chúng |
| ACM/DNS | Xác minh loại certificate và điều kiện pricing hiện hành, domain/DNS fees nếu có | Không mặc định domain hoặc mọi cert miễn phí |

Các mục chưa có workload/size ởW2 không gán total giả. Trước apply hoàn tất estimate theo giờ phiên dev, data size, requests và resources thực bật; xem [AWS Pricing Calculator](https://calculator.aws/).

Nguồn chính thức: [VPC pricing](https://aws.amazon.com/vpc/pricing/), [PrivateLink pricing](https://aws.amazon.com/privatelink/pricing/), [ELB pricing](https://aws.amazon.com/elasticloadbalancing/pricing/), [Terraform S3 backend/lock IAM](https://developer.hashicorp.com/terraform/language/backend/s3), [AWS provider6.21.0 ALB](https://registry.terraform.io/providers/hashicorp/aws/6.21.0/docs/resources/lb), [DynamoDB transaction permissions](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/transaction-apis.html#transaction-permissions).
