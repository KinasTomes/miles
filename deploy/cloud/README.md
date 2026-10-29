# Cloud fleet — chuẩn bị cho W5

ASG là opt-in của stack dev hiện có: `enable_asg=true`, `enable_compute=false`.
Không tạo stack/VPC/datastore thứ hai. Mẫu ở
[`cloud-fleet.tfvars.example`](../../infra/terraform/cloud/environments/dev/cloud-fleet.tfvars.example)
phải copy ra thư mục riêng và thay mọi placeholder trước plan.

Hai private subnet khác AZ, desired/min/max=2, ELB health `/readyz:9100`, HTTPS ACM
443 → S3 8080. Tái dùng SG hiện có: 8080/9100 chỉ từ ALB (metrics thêm jobs/admin),
9000 chỉ từ admin SG; không public Admin/metrics. App idle 150s, ALB idle 120s,
target deregistration 1800s. ALB giữ Host và không có URL/Host rewrite rule.
Launch template dùng AL2023 Amazon pin AMI/kiến trúc, IMDSv2, private ENI, encrypted
gp3 20 GiB **disposable**. Không mount EBS data của single-node dev. Metadata phải
shared DDB, không dùng Badger node-local thay thế. ASG dùng custom service-linked
role `AWSServiceRoleForAutoScaling_miles-dev`; KMS grant chỉ role đó, điều kiện
`kms:GrantIsForAWSResource=true`.

## Trước lần chạy thật

1. Hoàn tất runtime composition W05-HUNG-01 và các handoff; không sửa readiness thành
   200 để vượt health check. S3 phải bind private IP:8080, health private IP:9100,
   Admin private IP:9000 khi handler/auth đã sẵn sàng.
2. Chọn AMI AL2023 standard có Python3, AWS CLI v2, systemd; xác nhận instance type
   khớp `arm64`/`amd64`. AMI data lookup kiểm owner/name/arch/root EBS; không chứng
   minh image đã boot hay mọi dependency đã chạy. Không `dnf install` lúc bootstrap
   trong mạng chỉ có interface endpoints.
3. Review IAM của operator và runtime boundary, ACM ISSUED/DNS/allowlist, budget.
   Deploy role hiện **chưa có đủ quyền ALB/ASG/LT/service-linked role**; policy draft
   boundary không tự cập nhật AWS. Generate baseline từ private plan, review scope
   và native Access Analyzer/simulator trước khi gắn. Xem [operator](../../infra/terraform/cloud/bootstrap/operator/README.md).
4. Build release sạch; upload vào bucket data versioned bằng publisher được duyệt,
   key `releases/<archive-sha256>/miles-linux-<architecture>.tar.gz`; pin VersionId
   khác `null` và SHA-256. Gateway chỉ thêm `GetObjectVersion` ở prefix SHA đã pin,
   không được upload/xóa release. SSE-KMS dùng key runtime. Không lưu payload bí mật
   trong archive; thông tin backend/secret lấy qua provider/secret store runtime.
5. Chọn `interface` (9 service endpoints × 2 AZ + S3/DDB gateway), hoặc NAT **mỗi AZ**.
   Không dùng single NAT cho fleet. Fleet đợi ALB listener/API endpoints sẵn sàng
   trước bootstrap, và bị xóa trước chúng khi pause.
6. Dùng backend/state hiện có và deploy role qua identity gate, review saved plan
   thật và chi phí rồi mới apply theo quyền được duyệt. Không dùng mock fixture plan
   hay empty-state policy plan làm plan apply. Rolling refresh 100% min / 150% max
   healthy có thể cần **3 instance tạm thời**, cùng ALB, endpoints và disks.

## Bootstrap, rollout và pause

User-data lấy đúng S3 version bằng instance identity, xác minh archive SHA-256 rồi
mới giải nén/cài. Installer bảo toàn config operator. Systemd chạy Miles bằng user
`miles`; AWS provider lấy instance profile IMDS, xóa static/env/web-identity/ECS
credential override. Controller root riêng chỉ dùng IMDS, không dùng config Edge.
Không inject secret vào user-data hay in credential ra log.

Launch template pin **numeric version**, mỗi thay đổi kích hoạt rolling refresh có
auto rollback. Trước rollout đầu: kiểm hai node ready, IAM/DDB/R1/R2, upload/download
và signed paths qua ALB thật; sau đó mới thử replacement/pause có active requests.

Termination hook được tạo cùng ASG (không có khoảng trống trước hook), heartbeat TTL
300s. Controller poll IMDS target state **`Terminated`**, vì IMDS không trả current
`Terminating:Wait`. Nó yêu cầu stop Miles không block trước khi gọi control plane,
heartbeat khoảng 30s và deregister target. Chỉ gửi `CONTINUE` khi đồng thời có:

- Process đã dừng thành công, exit 0 và log `shutdown complete` của đúng systemd
  invocation đã ghi lúc bắt đầu drain.
- Target ALB trả `unused` sau deregister.

State root-only giữ boot ID/instance ID/start monotonic, restart không reset deadline
2100s. Timeout, exit lỗi hay thiếu proof ghi `drain-incomplete`, không báo completed.
Systemd không SIGKILL app khi timeout 1850s. **Hook là best effort:** AWS vẫn có thể
terminate khi hết heartbeat/global timeout hoặc lỗi máy. `CONTINUE` bị giữ lại không
phải cơ chế giữ máy vô hạn; R2 durability phải do runtime thật đảm bảo. Không coi
process fixture/mock control plane là bằng chứng flush dữ liệu thật.

`paused=true` bỏ ASG/LT, ALB, NAT/interface endpoints; giữ data/key/release/SLR và
runtime IAM để hook vẫn có quyền trong lúc xóa fleet. Đợi lifecycle/replacement kết
thúc, kiểm active jobs/R2 theo W5 rồi review pause plan. Giữ `enable_asg=true` và
release pin qua pause/resume. Không dùng `force_delete`/kill để thay graceful drain;
retained storage/key vẫn có phí. Full destroy cần data review riêng.

Nguồn: [IMDS target state](https://docs.aws.amazon.com/autoscaling/ec2/userguide/retrieving-target-lifecycle-state-through-imds.html),
[lifecycle hooks](https://docs.aws.amazon.com/autoscaling/ec2/userguide/lifecycle-hooks.html),
[KMS cho ASG EBS](https://docs.aws.amazon.com/autoscaling/ec2/userguide/key-policy-requirements-EBS-encryption.html),
[ALB Host/idle](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/edit-load-balancer-attributes.html).
