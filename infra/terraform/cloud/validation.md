# Evidence W02-HUNG-01 — chưa apply

Ngày thực hiện:2026-10-19 (UTC+7). Nhánh:`feat/w02-hung-01-terraform-foundation`, đã chuyển base sang `origin/main` tại commit `0c98d50` (W1 đã squash); toàn bộ thay đổi chưa commit được đối chiếu hash trước/sau để bảo toàn. Không đổi nguồn kiến trúc/API, backlog/report/apps, credential hoặc rules GitHub.

## Kiểm tra đã thực hiện

| Kiểm tra | Kết quả quan sát | Giới hạn |
|---|---|---|
| Terraform install |1.15.2 linux_arm64, archive hash khớp SHA256SUMS trên releases.hashicorp.com | Version pin cho task, không là latest |
| AWS provider init |6.21.0, package signed by HashiCorp | Lockfile có linux_arm64/linux_amd64 checksums |
| fmt -check -recursive |Pass sau format | Chỉ cú pháp/format |
| validate bootstrap và dev |Cả hai pass | Không chứng minh quyền apply/network runtime |
| terraform test mock |8passed,0failed | Plan-only, không tạo AWS; fake cert chỉ ở test |
| Dev isolated plan thật |42creates,0update/delete | Fresh empty local-state copy, enable_alb=false |
| Dev NAT plan thật |46creates,0update/delete | Single NAT/EIP; plan, không chạy mạng/đo throughput |
| Dev interface plan thật |53creates,0update/delete |8services,2subnets/AZ, không launch workload |
| Bootstrap state plan thật |5creates,0update/delete | Local bootstrap state, chưa có remote state bucket |
| Deployment identity gate offline |3 tests pass, gồm matching role và reject root/user/sai account/sai role/malformed ARN | Không tạo role hoặc apply |
| Availability của Interface Endpoints |AWS trả đủ8service names ở Singapore | Không thử reachability từ VM private |
| Pricing API |Đọc publicSingapore rates NAT/VPC/ALB; arithmetic dùng Python Decimal | Không là observed bill hoặc total estimate toàn dự án |

Lệnh tái hiện dev ở [README](README.md); `scripts/plan-local.py` chỉ in address/actions/count. Các summary thực của lượt kiểm tra lưu ngoài Git tại `/tmp/miles-plan-{isolated,nat,interface}-summary.json`; bootstrap binary plan ở `/tmp/miles-bootstrap.tfplan`, không commit/upload vì có thể chứa configuration. Artifact `/tmp` không bền vững; procedure là nguồn tái hiện chính. Không có secret hoặc state được đưa vào evidence này.

Bootstrap plan command: `terraform -chdir=infra/terraform/cloud/bootstrap/state plan -input=false -no-color -out=/tmp/miles-bootstrap.tfplan`, dưới environment credential tạm export bởi AWS CLI; không chép raw credentials vào command/history. Sau đó `terraform show -json` chỉ dùng local để đếm actions, không commit fullJSON.

Tám test gồm: isolated có4subnets+2gatewayendpoints và không claim workload-ready; singleNAT=1; perAZNAT=2; interface=8services/noNAT; ALB thiếu certificate/CIDR bị reject; positiveTLS fixture có443→8080,health9100/readyz,preserveHost=true,idle120,drain1800; reject /20 (subnet /28), accept /19 (public subnet /27). Những test này chứng minh các thuộc tính kế hoạch, không chứng minh SigV4 qua ALB thật hoặc cert ISSUED.

## Self-review và phần chưa nghiệm thu

- Mặc định không có private route tới KMS/SSM/Secrets/CloudWatch; cố ý ghi isolated là chưa ready. Before runtime chọn NAT hoặc Interface theo inventory, không dựa mỗi S3/DDB endpoints.
- Interface mode không có Internet package/ECR/SSM Session Manager: chưa có nhu cầu containers/Session Manager nên không tự thêm endpoints, cần AMI đã đóng gói hoặc NAT khi triển khai.
- Hai AZ nhưng singleNAT không HA: option per-AZ có sẵn, cost/tradeoff được công bố; không đổi thành lời hứa chịu lỗi AZ.
- IAM gateway/jobs scoped runtime, không tạo role admin cho root/deploy; bootstrap/deploy/offline recovery principal chưa được cấp. Chưa Access Analyzer validate từng policy/resolved resource ARN hoặc kiểm permissions boundary/SCP dưới role deploy. Review trước apply.
- Backend dùng S3 native lockfile/versioning/encryption trong stack riêng; bootstrap chưa apply, chưa migrate state/kiểm contention lock thật. Không tuyên bố remote backend đã hoạt động.
- Secret chỉ metadata, chưa có value; KMS prevent_destroy để tránh vô tình lên lịch xóa key còn dùng. Khi có data, mọi teardown phải kiểm retention/recovery roots, không dùng lệnh destroy toàn bộ như thao tác tắt compute.
- Chưa có domain/ACM ARN thật, chưa DNS validation; ALB disabled mặc định. Input certificate/CIDR là blocker bật HTTPS/apply ALB, không blocker fmt/validate/foundation plan.
- Chưa có EC2/ASG/bastion/app/backend data bucket/tables thực; đây là các task W3–5. Không test benchmark hoặc Edge/on-prem ở task W2 này.
- Workflow có Terraform CI và unit tests identity gate; kết quả remote xem Checks của PR, không lấy local validation thay CI/approval. Không tự đánh task Done.

Task ở mức **implementation/plan sẵn sàng review**, không phải hạ tầng đã triển khai hay PR đã merge.

## Xử lý review trước PR

- Gate đọc STS trên session hiện tại đã trả nonzero/reject như dự kiến; không gọi apply hoặc ghi AWS.
- Account input bắt buộc cho bootstrap/dev; sample ARN/account trong examples/tests chỉ dùng ID mẫu, không có account/hostname/credit nội bộ trong diff public.
- Nhánh dựa trên squash commit W1 ở main, không đưa lại commit W1 chưa squash.
- CIDR VPC chỉ /16.. /19 cho chia +8; có ca âm /20 và ca biên /19 kiểm public /27. Trước apply ALB vẫn kiểm IP trống thực tế.
- Cờ network đổi thành aws_api_egress_configured, chỉ mô tả đường API đã khai báo, không kết luận SSM Session Manager/ECR/workload ready.
- Gate STS read-only trước apply kiểm đúng role session; Terraform/identity policies vẫn là lớp quyền thực. Không thay root credentials hay thực hiện apply để chứng minh gate.
- Không thêm Transact* IAM actions: quyền item-level hiện có là hợp đồng đúng.
