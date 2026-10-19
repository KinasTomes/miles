# Môi trường AWS, quyền GitHub và kế hoạch tích hợp Miles

**Phạm vi:** W01-HUNG-03 (A01/A04) · **Owner:** Hưng (lead) · **Đối chiếu:** 19/10/2026, UTC+7.

**Mục tiêu:** Xác nhận quyền hợp tác GitHub, lựa chọn môi trường AWS dev/benchmark, giới hạn chi phí và điểm bàn giao giữa các thành viên. Đây là **kế hoạch và kiểm tra readiness**, không phải bằng chứng Cloud/Edge đã triển khai hoặc benchmark đã đạt.

**Nguồn:** [kiến trúc v4](../dedup-storage-gateway-architecture.md) §11–11.2, §13, §15.2, §19; [API v2](../dedup-storage-gateway-api.md) §1.1, §8, §12–13; [scope/DoD](../acceptance/scope-and-priorities.md). Dependency `W01-HUNG-02` đã **Done** trên Miles ([PR #3](https://github.com/KinasTomes/miles/pull/3)).

> **Bảo mật tài liệu public:** Repo `KinasTomes/miles` là public. Không đưa AWS account ID, VPC/subnet ID, ARN, credential, địa chỉ máy cá nhân, email nhận budget, số dư/hạn credit hay bằng chứng nội bộ có thông tin định danh vào Git/PR. Bằng chứng AWS chi tiết lưu trong kho riêng tư của nhóm, ghi người xác minh và thời điểm; chỉ công khai kết quả tổng hợp dưới đây.

## 1. Quyền GitHub và quy trình review

| Vai trò | Tài khoản GitHub | Quyền được xác minh (19/10) | Cách duyệt/merge |
| --- | --- | --- | --- |
| Hưng — lead | `KinasTomes` | Admin | Có thể sử dụng ngoại lệ admin của ruleset để squash merge PR do mình tạo; **không thể tự submit `APPROVED` review cho PR của mình** |
| Chiến | `mchienn` | Write | Cần approval hợp lệ từ người khác có quyền phù hợp |
| Hải | `ToNgocHai1543` | Write | Cần approval hợp lệ từ người khác có quyền phù hợp |
| Phong | `Mango-Bird` | Write | Cần approval hợp lệ từ người khác có quyền phù hợp |

`main` có ruleset **Protect main: PR, review and CI**: mặc định yêu cầu 1 approval độc lập, không dùng approval cũ sau push mới, last-push approval, resolve discussion, CI `check` và squash merge. Trường hợp admin bypass là **ngoại lệ merge**, không phải GitHub self-approval. Miles đã cấu hình ngoại lệ tự động Done cho **task của lead** khi tác giả PR khớp GitHub identity liên kết, PR đã merge, CI passed và các điều kiện khác hợp lệ; task thành viên vẫn cần approval thật. Không thay ruleset trong W1.

**Quy trình:** tác giả mở PR gắn đúng task ID và bằng chứng; CI chạy; reviewer kiểm tra HEAD cuối cùng và thay đổi; thành viên nhận approval độc lập trước merge; lead có thể quyết định bypass cho PR của mình sau khi tự kiểm tra CI và acceptance. Sau merge, GitHub Sync trên Miles đánh giá task và chỉ chuyển Done nếu checklist, evidence, dependency và chính sách đều đạt. Không coi comment của bot là approval.

## 2. Môi trường AWS và các giới hạn đã kiểm tra

- **Region dev và benchmark:** `ap-southeast-1` (Singapore), cùng Region khi so sánh workload Cloud A và thu chi phí. AWS CLI và `sts get-caller-identity` hoạt động; account cụ thể được xác minh ngoài Git.
- **Inventory (19/10, read-only):** có VPC mặc định, nhưng chưa có EC2, ALB/ASG, NAT Gateway, VPC endpoints, bảng DynamoDB, bucket S3 hay KMS key customer-managed được liệt kê trong phạm vi kiểm tra. Không suy ra account sạch ở mọi Region; `list-buckets` là account-wide.
- **Quota:** đã đọc được giới hạn EC2 On-Demand, VPC, ALB và Elastic IP trong Region; số lượng chưa chứng minh có instance capacity hoặc được phép tạo resource. Lưu giá trị/ảnh chụp kiểm tra chi tiết trong evidence riêng tư và kiểm tra lại trước `terraform apply`.
- **Quyền IAM:** môi trường điều khiển hiện xác thực bằng root principal có MFA; AWS báo không có root long-term access key. **Không dùng root để chạy Terraform, CI, thử nghiệm hoặc runtime.** Trước W2 apply phải có deployment principal giới hạn quyền, MFA/federation hoặc role assumption, phân quyền riêng EC2 gateway/jobs, state backend và người vận hành; không commit access keys. Việc tạo role/chỉnh chính sách là task triển khai được phê duyệt riêng, chưa thực hiện ở đây.
- **Máy làm việc:** chỉ dùng soạn tài liệu, code, CLI quản trị và điều khiển remote; không chạy gateway/cache/benchmark/fault injection. Runtime dev/integration dùng **EC2 thử nghiệm riêng** trong AWS. Không dùng EC2 chạy cấu hình Edge làm bằng chứng Edge/on-prem thật.

### Ma trận hạ tầng để thực hiện W2–W5

| Dịch vụ / thành phần | Hiện trạng đã kiểm tra | Thiết kế / cấu hình cần quyết định & kiểm chứng | Owner / hạn đề xuất |
| --- | --- | --- | --- |
| IAM / deployment | Có phiên điều khiển root; chưa chứng minh role deploy đủ quyền | Least-privilege deploy role, EC2 instance profile cho gateway/jobs; CI chỉ assume role ngắn hạn khi được duyệt; policies/permission boundary; không dùng root cho apply | Hưng — W2 trước plan/apply |
| Terraform state / secrets | Chưa provision | Backend S3 mã hóa + versioning, cơ chế lock phù hợp, tách state dev, quyền state riêng; SSM/Secrets Manager cho secrets; không commit state hay `.tfvars` nhạy cảm | Hưng — W2 |
| VPC / subnet / SG | Chỉ xác nhận VPC mặc định | Terraform **VPC riêng**, public/private ở ≥2 AZ; SG ingress/egress tối thiểu, route tables, ALB public và gateway private; không coi default VPC là thiết kế đã đạt | Hưng — W2 |
| S3 | Chưa có bucket | Bucket dev cho pack/sidecar/checkpoint/catalog; versioning, SSE-KMS, retention/recovery roots, chính sách truy cập và lifecycle theo contract | Hưng/Hải — W2–3 |
| DynamoDB | Chưa có table | Tables/keys và transaction/index theo contract Hải; PITR, IAM và cost/read-write capacity/on-demand; Badger chỉ dùng dev một node | Hải/Hưng — W2–4 |
| KMS | Chưa có customer-managed key liệt kê được | Key policy/grant cho S3, DSG secrets, restore/jobs; xem chi phí request/key và tránh khóa mất dữ liệu | Hưng/Hải — W2–4 |
| Network endpoints / NAT | Chưa có NAT/VPCE | Gateway endpoint **S3 và DynamoDB** (route tables + endpoint policies); tính NAT **hoặc** interface endpoints cho KMS, SSM, Secrets Manager, CloudWatch, STS/ECR nếu dùng; đo phí theo AZ/GiB | Hưng — W2 |
| ALB / TLS / admin | Chưa có ALB/cert trong Region kiểm tra | HTTPS/cert/Host/path cho S3-facing; admin port chỉ private qua bastion + SSH local-forward theo kiến trúc; metrics không public; health/ready đúng ngữ nghĩa | Hưng/Chiến — W3–4 |
| Cloud A runtime | Chưa triển khai | EC2 + ASG + ALB ở hai AZ; instance/EBS/AMI sizing, SSM/observability và teardown theo ngân sách thực, không chọn loại máy trước cost estimate | Hưng — W3–5 |
| Edge B runtime | **Chưa có host ngoài AWS được chỉ định** | VM ngoài AWS 1–2 node, TLS giữ Host/path SigV4, IAM Roles Anywhere/credential helper hoặc IAM hạn chế theo thiết kế; Internet egress AWS→Edge và cache locality cần đo trên đường thật | Hưng — chốt host trước W5 |

**Gates bắt buộc:** `terraform fmt/validate/plan`, reviewer kiểm IAM/network/state/cost, test kết nối và các quyền create/apply bằng principal deploy **trước khi** apply; IAM/S3/DDB/KMS/secret state phải sẵn sàng và được phê duyệt. Không cấp quyền ngoài nhu cầu và không tạo production resource trong task này. Endpoint Gateway cho S3/DDB không thay thế kết nối tới mọi API AWS khác; Edge ngoài AWS không dùng được Gateway Endpoint của VPC.

## 3. Ngân sách và kiểm soát chi phí

- AWS Budgets đang có budget dev **theo tháng, loại COST**, phạm vi toàn account. Có **4 cảnh báo đã xác minh**: ACTUAL vượt 50%, 80%, 100% và FORECASTED vượt 100%; mỗi ngưỡng có một subscriber. Không đưa số tiền hoặc email nhận cảnh báo lên repo public. Cần kiểm tra email người nhận hoạt động; API trả về subscriber **không đồng nghĩa thư đã đến inbox**.
- Budget chỉ là **cảnh báo có độ trễ**, không giới hạn chi tiêu cứng. Tại 80%, review tài nguyên đang chạy trước bài test phát sinh phí; tại 100%, ngừng lên lịch test mới và báo lead. Không xóa dữ liệu/retention/recovery roots theo cảnh báo tự động.
- Credit AWS do lead thông báo **chưa được đối chiếu số dư, hạn dùng hay danh mục dịch vụ được áp dụng**; không tính là ngân sách miễn phí. Việc có Free Tier không bảo đảm chi phí bằng không.
- Trước W2 deploy, lập estimate theo `ap-southeast-1` cho EC2 hours/EBS, ALB, S3 request+storage+retrieval, DynamoDB/PITR, KMS, CloudWatch, NAT/interface endpoints và lưu Cold/retention sau khi tắt compute. Chi phí benchmark Edge B phải bao gồm truyền dữ liệu qua Internet thật, không suy từ EC2 nội vùng.
- Compute chỉ bật trong phiên thử nghiệm và có lịch tắt. Teardown theo quy tắc dữ liệu/recovery trước khi xóa bất kỳ resource nào; việc terminate instance **không** loại bỏ mọi loại phí.

**Evidence nội bộ cần lưu:** `sts get-caller-identity` (không publish account ID), IAM/quota/inventory read-only theo Region, budget + notifications + subscribers, pricing estimate có ngày và Region, xác nhận trạng thái credit qua console nếu được quyền. Không cung cấp credential hoặc output chứa secret trong PR.

## 4. Lịch phối hợp và đường tích hợp

**Lịch đề xuất**, không phải lịch họp đã được chấp thuận hay lời mời đã gửi. Mốc W2 dưới đây lấy tuần 19–25/10/2026; thay đổi ngày cần được nhóm ghi nhận công khai trong task/PR, không tự coi đã xác nhận.

| Thời điểm / gate | Owner chính | Bàn giao / quyết định cần chốt | Điều kiện kiểm chứng / người phối hợp |
| --- | --- | --- | --- |
| **W2 Thứ 2 (19/10)** — mở dependency | Hưng | Xác nhận S3/API W1 của Chiến đã Done; liệt kê các interface/format W1 của Hải/Phong đang thiếu, đặt owner và hạn review | Chiến: link PR #1/#4/#5; Hải/Phong: task và link draft cụ thể hoặc đánh dấu `Blocked` |
| **W2 Thứ 4 (21/10)** — interface review | Hải & Phong; Hưng điều phối | Pack/manifest/metadata, writer/Clock/R1/R2, boundary SigV4-auth/API ↔ storage, serialization/event/checkpoint version | Chiến đối chiếu fixtures; ghi schema, compatibility, reviewer và các quyết định còn mở |
| **W2 Thứ 6 (23/10)** — W2 integration gate | Hưng | `terraform plan` + IAM/network/cost review; draft health/pricing; freeze **chỉ những format đã review**; PR/evidence của Hải/Phong và contract API | Chiến/Hải/Phong confirm bằng CI/test/fixture; phần thiếu giữ `Blocked`, không freeze giả |
| **W3 Thứ 4 (28/10)** — Warm MVP integration | Chiến/Hải/Phong; Hưng điều phối | S3 router+auth, engine/Badger/S3 adapter, cache/read path, config/health trên EC2 dev riêng | Round-trip PUT→GET hash, checksum và lỗi upload phải giữ nguyên head cũ; log CI + artifact |
| **W3 Thứ 6 (30/10)** — pilot check | Hưng | Số đo sơ bộ và danh sách W4 blocker: DDB transaction, multipart, admin, R1/exporter, Cloud/Edge deployment | Không dùng Badger/EC2 Edge-profile làm bằng chứng DDB/Edge B thật |
| **W4–W5** — deployment và multi-node | Hưng | Cloud A ALB/ASG/SG/TLS, DDB/R1/R2, kiểm jobs/drain và Edge-functional test; chọn host Edge ngoài AWS | Host ngoài AWS phải có trước acceptance Edge B; IAM/secret/retention và egress có evidence |
| **W6–W8** — đo và bàn giao | Hưng cùng nhóm | Pricing/simulator/faults, benchmark Cloud A + Edge B, freeze, báo cáo/demo/cleanup | Gắn workload, Region, giá có ngày, provenance test; không biến giả định thành số đo |

**Nhịp làm việc:** đầu tuần owner cập nhật dependency và ETA trong Miles; thứ Tư review interface; thứ Sáu chạy integration gate với CI, fixture, owner và evidence. Reviewer không cần cố định cặp, nhưng review member-PR vẫn phải khác tác giả. Chỉ commit contract sau review. Khi chậm, ghi task còn `Doing`/`Blocked`, không tự đánh Done.

**Tiến độ W1 tại 19/10:** Hưng `W01-HUNG-01/02` và Chiến `W01-CHIEN-01/02/03` đã Done; `W01-HUNG-03` đang Doing. Các task W1 của Hải và Phong còn Todo trên Miles, là **rủi ro trực tiếp** với kế hoạch freeze W2. Cần xác nhận người phụ trách, đường bàn giao và chặn mục phụ thuộc cụ thể trước 23/10; không mặc định họ chưa làm chỉ vì chưa cập nhật Miles.

## 5. Phạm vi xác minh, blocker và handoff

| Hạng mục | Trạng thái W1 | Bước tiếp theo |
| --- | --- | --- |
| Quyền GitHub, ruleset, ngoại lệ lead | **Đã xác minh qua GitHub API và PR #3** | Reviewer theo quy trình, không gọi merge bypass là approval |
| AWS Region, CLI, inventory, quota | **Đã kiểm tra read-only** | Lưu evidence riêng tư, kiểm tra lại trước provision |
| Budget và cảnh báo | **Đã kiểm tra qua AWS Budgets API** | Xác minh inbox, estimate và cost gates trước W2 apply |
| IAM deploy role/permission, Terraform state | **Chưa triển khai** | W2, tuyệt đối không dùng root runtime/apply |
| Host Edge B ngoài AWS | **Chưa xác định** | Phải có cho benchmark/acceptance thật W5–7 hoặc duyệt lại phạm vi và nêu rõ giới hạn |
| AWS credit khả dụng | **Chưa xác minh** | Kiểm tra riêng tư số dư, hạn và dịch vụ áp dụng; không chi tiêu dựa trên giả định |
| Tài liệu W1 của Hải/Phong | **Chưa có trạng thái Done trên Miles** | Review draft/PR, xác minh format gate trước freeze W2 |

**W01-HUNG-03 đủ điều kiện đề nghị review khi:** tài liệu này được commit trong PR, thông tin GitHub/AWS kiểm chứng được, Region và cách quản lý chi phí rõ, bảng môi trường thiếu + owner/hạn rõ, lịch bàn giao có gates, và 3 checklist/evidence trên Miles được cập nhật. Không đổi API/kiến trúc nguồn, không chỉnh settings AWS, không áp Terraform trong PR tài liệu này.
