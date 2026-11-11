# Runbook nghiệm thu Cloud và Edge trên AWS

Bắt đầu từ [plan tiếp tục nghiệm thu](../planning/edge-aws-acceptance-continuation.md) để lấy scope, thứ tự apply và tiêu chí từng bài. Dùng lại các bước đã chạy được bên dưới, giữ thứ tự và chỉ thay đầu vào riêng của lượt mới. Toàn bộ Cloud, Edge và client chạy trên AWS. Máy Oracle chỉ dùng để điều phối qua CLI.

## Các bước đã chạy được

| Bước | Cách đã chạy | Bằng chứng cần lưu trước khi đi tiếp |
|---|---|---|
| Foundation và TLS | Apply foundation khi Cloud ASG còn tắt. Dùng CA có `CA:TRUE` và `keyCertSign`, verify chain trước khi tạo Roles Anywhere trust anchor. Kiểm S3 versioning và chờ propagation trước lần ghi đầu. | Terraform apply log, 20 bảng, versioning, verify chain, trust anchor và trạng thái ACM. |
| Edge và client riêng tư | Dựng hai VM ARM64, vào qua SSM. Khi dùng SSH qua SSM, lấy host key từ EC2 console đã xác thực rồi pin. Dùng bundle RPM đã xác minh chữ ký và hash để cài qua repo cục bộ. | Native instance/SSM inventory, host key proof, RPM manifest và install log. |
| Bootstrap và khởi động Edge | Chờ Chrony có nguồn đồng bộ và uncertainty hợp lệ, rồi bootstrap trên AWS bằng operator credential. Cài release ARM64 đã kiểm hash, cấu hình Roles Anywhere và TLS, rồi xác minh readiness và Admin qua tunnel. | Kết quả Chrony trước bootstrap, kết quả bootstrap, release SHA, readiness, TLS và một request Admin thành công. |
| Cleanup | Freeze state, inventory native và tài nguyên ngoài Terraform trước destroy. Dừng gateway sạch, lưu bằng chứng, dọn dữ liệu thử, destroy phần thuộc lượt chạy, khôi phục IAM rồi audit lại. | `shutdown complete` của đúng invocation, Terraform state, native absence ledger, danh sách ngoại lệ xóa theo lịch, rollback IAM và chi phí ước tính. |

[Lượt 10/11](edge-private-aws-validation.md) xác nhận các bước trên đến Edge readiness và cleanup. Trên máy vận hành hiện tại, thư mục riêng `~/.local/state/miles/edge-acceptance-prep/run` giữ các script đã dùng như `host-prepare.py`, `bootstrap-from-operator.py`, `edge-install.py` và `final-audit.py`, cùng `progress.json` và log theo bước. Dùng các script đó để đối chiếu thao tác. Tạo run ID, saved plan, cert, session và grant mới theo plan trước khi dùng lại.

## Khác biệt cần giữ đúng

- **Cloud ASG.** Lượt 08/11 đã chạy Cloud qua ALB. Lượt 10/11 dừng ở ASG vì custom service-linked role đã có sẵn. Xác minh baseline ownership và dùng `existing_autoscaling_role_arn` trước saved fleet plan để giữ role ngoài state. Nếu đã import, backup state và bỏ binding theo plan tiếp tục. Xác nhận đủ hai Cloud target healthy trên lượt mới. [Báo cáo 08/11](live-acceptance-20261108-report.md) · [Báo cáo 10/11](edge-private-aws-validation.md)
- **PUT trên bucket mới.** Lượt 08/11 gọi Admin `GET /admin/v1/buckets/{bucket}/policy` sau CreateBucket. GET này ghi policy mặc định revision 1 khi chưa có. Lượt 10/11 bỏ bước đó, PUT đầu tiên lộ lỗi `404 NoSuchKey`. Để xác nhận bản sửa `ef13240`, chạy CreateBucket → LIST → PUT trên bucket riêng, trước mọi Admin GET/PUT policy. Ghi thứ tự request và kết quả PUT. Lượt 11/11 đã đạt trên AWS trong C1 và C4. [Handler Admin](../../internal/api/admin/handlers.go) · [Báo cáo chạy lại](edge-private-aws-recheck.md)
- **Telemetry trước tải.** Dùng cả recorder và parser từ cùng release, chạy `edge_host_telemetry.py probe` và xác nhận file thực sự tăng sample Chrony/OS trước khi dispatch. Bản đóng gói đã qua test local. Lượt 10/11 chưa có telemetry hợp lệ và chưa kiểm lại lỗi clock dưới tải. [Báo cáo 10/11](edge-private-aws-validation.md)
- **Baseline trước auditor.** Chụp account baseline v2 trước khi tạo role audit tạm. Khi dọn, kiểm cả tài nguyên phát sinh lẫn tài nguyên baseline bị thiếu, cùng trust policy, permissions boundary và policy của deploy role. Ghi ID từng KMS key/secret mới được duyệt chờ xóa vào manifest. [Plan cleanup](../planning/edge-aws-acceptance-continuation.md#5-khi-lỗi-và-cleanup)
- **Native fixtures qua gateway endpoints.** Trước R1/R2/GC, lưu policy gốc của đúng hai endpoint S3/DynamoDB. Quyền IAM của role deploy và policy endpoint cùng phải cho phép prefix bảng fixture, bucket GC và prefix journal của lượt. Ghi principal, ID endpoint, resource ARN và expiry trong ledger. R2 cần `s3:GetBucketVersioning` trên bucket journal và `s3:DeleteObjectVersion` dưới prefix journal thử. Sau cập nhật, probe từ client AWS phải đọc được versioning và nhận `ResourceNotFoundException` khi GetItem bảng probe chưa tồn tại. Chờ các action có hiệu lực trước khi tạo fixture, rồi khôi phục policy endpoint gốc sau fixture. Lượt 11/11 đã xác nhận R1/R2 đạt theo cách này. [Báo cáo chạy lại](edge-private-aws-recheck.md)
- **Fault và cache purge.** Cache Jobs dùng node ID kèm incarnation; API faults dùng ID gateway từ cấu hình, trong lượt EC2 này là instance ID. Gửi fault vào tunnel của đúng gateway. Cache purge phải báo succeeded trước corrupt-pack GET; heartbeat Ready không chứng minh runner đã xử lý child. Lượt 11/11 corrupt-pack trả 502 qua ALB/Nginx, tăng integrity counter và hồi phục GET 200 đúng SHA sau khi gỡ fault. Kiểm cả lỗi, counter và native alarm thay vì khóa assertion vào mã 500.
- **Pin release Cloud.** Lấy đúng `release_key` từ fleet contract (`releases/<sha>/miles-linux-arm64.tar.gz` cho ARM64). VersionId phải thuộc chính key đó. Trước apply, GET đúng cặp key/version, kiểm SHA-256 của archive với manifest và saved plan; HEAD/kích thước bổ sung kiểm tra cấu hình. Không dùng VersionId của cùng bytes ở tên file khác. Gate thất bại phải dừng trước apply. Khi thay node, xác nhận native máy cũ đã terminated trước bước có thể tạo máy mới và giữ mọi ID/volume cũ trong ledger cleanup.

## Nhật ký cho mỗi lượt

Tạo `run-log.md` trong thư mục riêng của lượt. Đầu file ghi run ID, Region, source/release SHA, topology và deadline cleanup. Sau **mỗi bước**, cập nhật một dòng trước khi chuyển bước hoặc bàn giao.

| Giờ UTC | Bước | Lệnh hoặc script và đầu vào đã che | Kết quả | Evidence | Bước kế |
|---|---|---|---|---|---|
| YYYY-MM-DD HH:MM | Tên bước | Tên script, flags và cấu hình có ảnh hưởng | PASS / FAIL / CHƯA CHẠY | Tên log hoặc artifact | Việc tiếp theo hoặc cleanup |

Khi cần đổi trình tự, config hoặc script, ghi điểm thay đổi và lý do trước khi chạy. Sau khi xác minh cách mới chạy được, cập nhật bước tương ứng trong runbook kèm release, kết quả và bằng chứng. Lưu raw logs, state, chứng chỉ riêng và thông tin xác thực trong thư mục private. Đưa bản tóm tắt đã che thông tin nhạy cảm vào PR.
