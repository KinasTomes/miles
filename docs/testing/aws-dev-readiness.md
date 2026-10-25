# Kiểm tra readiness của môi trường AWS dev Warm

Task: W03-HUNG-01. Đối chiếu ngày 25/10/2026 (UTC+7), sau khi đồng bộ
`main` tại `65e9c04`. Đây là bằng chứng chuẩn bị IaC, chưa phải nghiệm thu
môi trường đã chạy trên AWS.

## Kết quả không tạo tài nguyên

| Kiểm tra | Kết quả | Giới hạn |
|---|---|---|
| Terraform 1.15.2, AWS provider 6.21.0 | Giữ version và lockfile hiện có | Không nâng dependency trong task này |
| `terraform fmt -check -recursive` | Pass | Format |
| Init `-backend=false -lockfile=readonly` và validate | Bootstrap state và dev đều pass | Không kiểm remote state hoặc quyền apply |
| `terraform test` | 21 pass, 0 fail | Tất cả dùng mock AWS, kể cả các bước apply/cleanup trong test |
| Python identity/private-plan/session-plan tests | 11 pass | Kiểm offline; không chứng minh quyền IAM trên AWS |
| `make check` sau đồng bộ main | Pass | Go/Python tests, vet, format và build; không chạy ứng dụng trên EC2 |

Mock lifecycle kiểm active → pause → resume và interface → pause. S3,
DynamoDB, data EBS, KMS, VPC và runtime identities được giữ; EC2/root disk,
NAT/EIP, interface endpoints và ALB theo phiên được thu hồi. Gate plan chặn
thay đổi tài nguyên giữ lại, replace, drift, import/move và đổi AMI/topology.
Đây là kiểm graph và kế hoạch, chưa chứng minh dữ liệu hay network thật.

Lệnh tái hiện nằm trong [Terraform README](../../infra/terraform/cloud/README.md).
Không cần credential AWS để chạy bộ kiểm tra offline này.

## Dependency và bàn giao

- Hai hard dependency W01-HUNG-03 và W02-HUNG-01 đã Done. Các task W2 về
  pack/S3, Badger, policy, auth, read/cache và compatibility đã merge. Không
  cần chờ toàn bộ W3 để dựng và kiểm môi trường hạ tầng.
- [Mapping 20 bảng dev](../../infra/terraform/cloud/metadata-tables.md) là
  inventory cho adapter DynamoDB W03-HAI-03/W4. Review tên/kiểu key trước
  apply; không tuyên bố đã có adapter DynamoDB hoặc multi-node commit.
- Handler S3/Admin và runtime wiring thật chưa hoàn chỉnh. Bài kiểm tra
  hạ tầng không thay bằng chứng PUT/GET của W03-CHIEN-02 hay reader Warm
  của W03-PHONG-01; cũng không lấy việc các handler đó chưa xong làm lý do
  trì hoãn kiểm network, IAM, datastore và vòng đời hạ tầng.
- Phiên CLI role dùng cho Terraform đã hết hạn lúc đối chiếu. Không dùng
  root hoặc credential mặc định để thay thế; chưa tạo fresh AWS plan trong
  lượt này. Bằng chứng plan/IAM ngày 22/10 ở
  [dev preparation](../../infra/terraform/cloud/dev-preparation.md) là lịch
  sử, không chứng minh IAM live hoặc AMI vẫn launch được ngày 25/10.

## Phiên AWS thật cần báo cáo và được duyệt trước khi chạy

1. **Chuẩn bị plan:** đăng nhập lại deploy role bằng MFA, kiểm identity,
   review/cập nhật riêng các policy data/compute/session/teardown và boundary.
   Tạo fresh plan bootstrap và dev với config riêng; kiểm account, AMI,
   mapping DynamoDB, ngân sách phiên và phí tài nguyên giữ lại. Lưu plan và
   inventory ngoài Git. Không dùng plan cũ hoặc mock plan để apply.
2. **Dựng backend và datastore:** sau khi được duyệt, bootstrap state và
   kiểm lock/versioning; apply persistent plan đã review với `paused=true`.
   Trạng thái này vẫn tạo tài nguyên có phí lưu trữ/key, không phải miễn phí.
3. **Bật phiên dev:** tạo/gate/review resume plan cho một EC2 private và một
   NAT, ALB tắt. Kiểm SSM Online, session log mã hóa, data-volume identity
   và mount. Không format lại volume đã có dữ liệu hoặc chạy app khi thiếu
   mount. Giữ Admin/metrics private.
4. **Kiểm quyền và dữ liệu:** từ runtime identity, kiểm S3 put/get/hash,
   metadata control-object reads và DynamoDB read/write có dọn probe; kiểm
   truy cập state/quản trị IAM bị từ chối. Lưu sentinel/hash trên data EBS
   và datastore để đối chiếu qua pause/resume; không dùng output Terraform
   thay bằng chứng API/network. Dữ liệu kiểm thử phải tách khỏi dữ liệu cần giữ.
5. **Thu hồi và kiểm lại:** drain/unmount, gate/apply pause plan, kiểm không
   còn EC2/NAT/EIP/interface endpoints/ALB hoặc orphaned root volumes. Resume
   một lần để đối chiếu sentinel/hash, rồi pause lại. Full destroy môi trường
   disposable là kế hoạch riêng: kiểm dữ liệu/retention, quyền cleanup và
   deletion protection trước khi xóa. Không tự gỡ bảo vệ data/key để đóng task.

Quy trình và lệnh cụ thể ở [dev sessions](../../infra/terraform/cloud/dev-sessions.md).
Các API live, thời gian chạy, inventory sau thu hồi và chi phí thực phải có
evidence riêng trước khi đề nghị Done. Chưa tick checklist triển khai thật.
