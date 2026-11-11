# Plan tiếp tục nghiệm thu AWS

Cập nhật sau lượt chạy ngày 10/11/2026. Mục tiêu là nghiệm thu lại bản sửa trên AWS, hoàn tất Edge C1/C4, recovery/GC và tám alarm, sau đó cleanup có bằng chứng.

## Trạng thái bắt đầu

- PR đang làm là [PR #76](https://github.com/KinasTomes/miles/pull/76), branch `docs/edge-aws-approval-package`. Worktree hiện tại `/home/ubuntu/miles-worktrees/edge-aws-approval-package`.
- Lượt trước dùng release `e87c665`. Edge ready và LIST thành công, PUT đầu tiên trả `404 NoSuchKey`. Fail-fast đã dừng lượt thử. C4, native recovery/GC và alarm chưa được nghiệm thu.
- `ef13240` sửa runtime dùng cache policy mặc định revision 1 cho bucket chưa từng được Admin chỉnh policy. Regression trên DynamoDB Local/Moto với race detector đã đạt, gồm shared runtime và GC.
- `df091a9` đưa recorder và `runner_diagnostics` vào release. Test bundle độc lập đã đạt. Lượt trước collector thiếu parser và không có telemetry window hợp lệ.
- `27f6a0b` sửa đồng bộ fixture test drain. `ebca172` sửa AMI mock cho precondition Terraform. Kiểm tra CI trên head cuối trước khi chọn release.
- Cleanup cuối đạt `PASS_WITH_PENDING` lúc 23:28 ngày 10/11, giờ Việt Nam. 80/82 identifiers vắng mặt, hai mục còn lại là KMS/secret mới chờ xóa 30 ngày theo duyệt. IAM đã khôi phục baseline, role audit và hai service-linked role tạo trong lượt đã xóa, Terraform state không còn managed resource.
- Chi phí lượt trước ước tính khoảng 1,25 USD trước thuế, chưa đối soát hóa đơn.

[Báo cáo lượt trước](../testing/edge-private-aws-validation.md) ghi kết quả và giới hạn bằng chứng. Các dữ kiện trên là lịch sử, cần kiểm tra lại hiện trạng lúc thực hiện.

Dùng [runbook và mẫu nhật ký từng bước](../testing/edge-aws-runbook.md) để lặp lại cách dựng đã xác minh. Tạo `run-log.md` trong thư mục riêng của lượt trước bước 1 và cập nhật ngay sau mỗi bước. Ghi rõ lý do nếu thay đổi trình tự hoặc cấu hình.

## 1. Chuẩn bị offline trước khi tính phí

- [ ] Dùng đúng worktree/branch và xác nhận GitHub CLI đang ở account `KinasTomes`. Nếu chạy trên máy khác, checkout branch của PR trong worktree riêng và đọc hướng dẫn implement của repo hiện tại.
- [ ] Đọc review mới nhất và kiểm tra CI head cuối. Sửa các lỗi còn lại, chốt source SHA, rồi build ARM64 từ worktree sạch.
- [ ] Xác nhận `make check`, toàn bộ Terraform mock tests và regression shared runtime/GC đã đạt trên code cuối. Chỉ chạy lại những phần có thay đổi hoặc bằng chứng chưa đủ.
- [ ] Build release bằng `python3 deploy/build-release.py --arch arm64 --out <new-private-archive>`. Lưu source SHA, SHA-256 archive và manifest. Dùng recorder và parser trong chính release này khi cài host.
- [ ] Chuẩn bị gói RPM ARM64 cho Nginx, Python 3.12 và Chrony từ image/repository đã pin, tạo metadata repository cục bộ và thử cài khi ngắt mạng. Xác minh chữ ký RPM và hash archive trước khi cài trên AWS.
- [ ] Chuẩn bị TLS/CA và identity Roles Anywhere mới. CA có `CA:TRUE` và key usage `keyCertSign`, identity có `CA:FALSE` và `digitalSignature`. Verify chain, hạn dùng và hostname TLS trước khi upload trust anchor.
- [ ] Chuẩn bị runner theo thứ tự bên dưới. Diễn tập phần tạo tín hiệu alarm và cách gỡ fault. Kiểm tra thời gian chờ cut age hơn một giờ cùng thời gian hồi phục, dành tối thiểu 50 phút cleanup.
- [ ] Chuẩn bị sẵn script rollback IAM, cleanup dữ liệu synthetic, freeze manifest và audit cuối. Rà cả đường lỗi từng gặp trước khi chạy thật.

## 2. Phiên AWS, baseline và plan thật

Chỉ bắt đầu lượt tính phí sau khi hoàn tất bước 1 và ghi nhận phê duyệt cho lượt mới. Mức tham chiếu là 10 USD trước thuế, tối đa bốn giờ gồm cleanup, tối đa năm VM và xóa root disk synthetic không backup. Refresh bảng giá và plan thực tế trước khi chốt lượt mới. [Gói chi phí tham chiếu](edge-aws-approval-package.md).

- [ ] Tạo phiên `miles-dev` bằng `aws login --remote`. Gửi link bấm trực tiếp. Người dùng Switch back rồi Switch role mới sang `MilesDevDeploy`, chọn đúng role khi cấp code.
- [ ] Làm trống `cli-login-code.txt` trước khi xin code, chuyển nguyên mã vào phiên CLI đang chờ mà không in ra output, xóa mã ngay sau khi dùng. Làm tương tự với `iam-login-code.txt` khi cần phiên quản trị IAM.
- [ ] Xác minh STS account/role, Region Singapore và expiry. Workload dùng deploy role, root chỉ thực hiện IAM và audit đã duyệt. Dùng credential_process có thể refresh cho Terraform, tránh giữ một bản credential hết hạn suốt apply.
- [ ] Chụp baseline account v2 và EBS trước khi tạo auditor hoặc gắn grant tạm. Dùng `capture_account_inventory.py --iam-role <deploy-role>` và `capture_volume_baseline.py`, ghi hash riêng. Sau baseline mới tạo auditor chỉ đọc và ghi đúng tên role này vào manifest.
- [ ] Kiểm tra role `AWSServiceRoleForAutoScaling_miles-dev`. Nếu có sẵn, xác nhận baseline ownership và gán ARN cho `existing_autoscaling_role_arn` trước saved fleet plan. Với role baseline đã import từ lượt cũ, lưu state backup rồi dùng `terraform state rm 'aws_iam_service_linked_role.autoscaling[0]'` để chuyển sang tham chiếu ngoài state. Xác nhận plan giữ nguyên role và metadata.
- [ ] Chuẩn bị grant tạm có expiry, permissions boundary, validation và simulation cả quyền tạo/chạy/xóa. Lưu default version và document baseline để khôi phục. Rà cảnh báo wildcard của policy generator trước khi cấp.
- [ ] Bổ sung sẵn quyền cleanup cho đúng inventory. Cần `dynamodb:DeleteTable` và `DescribeTable` kể cả sau khi tags của bảng đã mất, `s3:ListBucketMultipartUploads`/versions, `DeleteBucketPolicy`, quyền disassociate route trên subnet và route table, `kms:DeleteAlias` trên cả alias và key, `iam:ListInstanceProfilesForRole` cho role Edge.
- [ ] Review saved plans thật và hash, xác nhận private addressing, số VM/disk/endpoint/alarm và scope dữ liệu. Loại placeholder, overlay public cũ và tám alarm probe tổng hợp.
- [ ] Ghi T0, người vận hành, ngân sách và deadline. Mốc tham chiếu là dừng bài mới T0 + 180 phút, cleanup chậm nhất T0 + 190 phút và kết thúc mục tiêu T0 + 240 phút. Theo dõi lượng request, metric identities và logs bên cạnh thời gian.

## 3. Dựng hệ thống theo thứ tự

1. Apply foundation bằng deploy role. Xác nhận 20 bảng, S3 versioning, key/secret metadata, network và Roles Anywhere. Ghi thời điểm quan sát versioning Enabled, chờ propagation theo hướng dẫn S3 trước lần ghi đầu.
2. Dựng hai máy Edge/client riêng tư với SSM, khi Cloud ASG còn tắt. Pin SSH host key từ console AWS xác thực nếu dùng SSH qua SSM, rồi cài RPM đã ký và archive đã kiểm hash.
3. Cấu hình Chrony trỏ Amazon Time Sync. Chờ tracking/source và uncertainty đạt trước bootstrap. Tạo runtime secret một lần và bootstrap installation trên máy AWS bằng operator credential ngắn hạn.
4. Nếu bootstrap trả outcome unknown, giữ token, export trạng thái và xác minh tất cả bảng/installation marker trước khi quyết định retry. Lượt trước bị từ chối do clock chưa ổn định rồi mới chứng minh được metadata còn rỗng.
5. Cài Edge với Roles Anywhere certificate riêng. Instance profile của Edge chỉ phục vụ SSM/bootstrap. Kiểm tra readiness, identity backend, TLS, private HTTPS và Admin tunnel.
6. Upload release vào S3 theo prefix SHA, pin VersionId. Lập và rà saved fleet plan sau khi reconcile service-linked role. Apply hai Cloud node, ALB HTTPS nội bộ và đúng tám runtime alarms.
7. Kiểm tra hai Cloud healthy qua ALB, Edge ready, credential refresh, shared history và đúng release SHA trên từng node. Nếu đọc bucket policy để kiểm tra Cloud/Edge nhìn thấy cùng cấu hình, dùng bucket smoke riêng. Ghi inventory thật, EBS và đường network của client.
8. Chạy `edge_host_telemetry.py probe` từ release trên host và yêu cầu exit 0. Khởi động recorder, xác nhận process còn chạy, file 0600 có record Chrony/OS hợp lệ và sample tiếp tục tăng. Chưa đủ bằng chứng này thì dừng trước workload.
9. Sinh dataset `backup30` trên client AWS, lưu manifest hash/size. Tạo identity, user/domain và bucket synthetic riêng cho mỗi trial. Với Edge C1, ghi thứ tự CreateBucket → LIST → PUT vào nhật ký. Mọi Admin GET/PUT policy trên bucket C1 phải sau PUT đầu tiên vì ngay cả GET cũng có thể ghi policy mặc định.

## 4. Ma trận nghiệm thu

| Thứ tự | Bài | Tiêu chí đạt và evidence |
|---|---|---|
| 1 | Edge C1 | `warmbench --fail-fast --concurrency 1 --read-repeats 3 --drain-timeout 120s`. 90/90 PUT, 90/90 HEAD, 360/360 GET đúng SHA-256, không 500/503/404 ngoài dự kiến, không outcome chưa xác định. Dùng bucket mới và thực hiện PUT đầu tiên trước mọi Admin GET/PUT policy để kiểm bản sửa mặc định |
| 2 | Edge C4 | Chỉ sau C1 đạt, chạy trial riêng với concurrency 4 và cùng dữ liệu/release/policy. Cùng tiêu chí số lượng và checksum |
| 3 | Native R1/R2 | Admission, renewal, fencing, takeover, closed producer, journal export/seal, independent cut verification và checkpoint. Lưu trạng thái lease/cut và log. Phân biệt clock mô hình trong fixture với runtime nhiều process |
| 4 | Native GC | `TestAWSDevWarmGCObjectFlows`, verify phiên deploy và isolated resources. Kiểm real S3 versions, pins, compaction/checkpoint và GC. Dọn fixture xong trước bộ kế tiếp |
| 5 | Tám runtime alarms | Cloud/Edge × cut age, integrity, throttling, telemetry age. Thu native history `OK → ALARM → OK`, datapoint đúng dimensions/thresholds và telemetry freshness. Theo quy trình fault trong runbook |

Native fixtures có guard riêng. Đặt account/role/Region và cờ opt-in theo [runbook](edge-acceptance-aws-run-plan-draft.md). `TestAWSDevRecoveryFencedSealCheckpoint` cần bucket versioned được chỉ định. GC fixture tạo bucket riêng và đợi 15 phút sau bật versioning, cần timeout ít nhất 45 phút. Chuẩn bị đủ quyền và inventory cho các resource tạm, tối đa 20 bảng fixture tồn tại đồng thời.

Đối với alarms, chỉ bật test faults trên installation disposable. `corrupt_pack` sửa byte trong bộ nhớ, `ddb_throttle` mô phỏng lỗi SDK, `s3_5xx` chặn request trước network. Ghi rõ loại bằng chứng. Counter không phát zero có thể hồi phục theo missing-data policy, phải chứng minh exporter vẫn sống. Cut-age cần mốc seal thật và hơn một giờ chờ, kiểm tra thời gian còn lại trước khi bắt đầu.

`capture_alarm_evidence.py` thu cấu hình/history/datapoint rồi gọi validator. Reader này cần bằng chứng riêng về nguồn tín hiệu runtime. Không coi alarm probe tổng hợp hoặc `SetAlarmState` là PASS.

Với mỗi trial, recorder phải bao phủ từ trước dispatch tới sau response cuối. Lưu request ID, HTTP/sample error, gateway stage và host timing để đối chiếu riêng. Tóm tắt telemetry đúng UTC window, lưu hash raw và summary. Chỉ đưa bản đã rút gọn, che thông tin nhạy cảm vào Git.

## 5. Khi lỗi và cleanup

- [ ] Khi một gate lỗi, dừng dispatch, drain request đang chạy, export evidence và ghi FAIL cho gate đó. Các gate phụ thuộc còn lại ghi chưa nghiệm thu. Chuyển cleanup theo envelope đã duyệt.
- [ ] Chụp state JSON, outputs và native ASG/instance/EBS inventory trước destroy. Freeze manifest với state/runtime/account baseline hashes, deploy role và audit role. Ghi từng ngoại lệ đã duyệt bằng `--approve-pending-deletion kms_key=<key-id>` hoặc `--approve-pending-deletion secret=<secret-arn>`, chỉ dùng ID tài nguyên mới có trong frozen state. Thêm ACM certificate và tài nguyên ngoài Terraform vào inventory.
- [ ] Xuất dữ liệu synthetic cần chẩn đoán, đối chiếu PUT outcome chưa rõ. Xóa S3 versions/delete markers/multipart và fixture data, xác minh lại bucket rỗng.
- [ ] Rà plan gỡ deletion protection và plan destroy. Chỉ gỡ bảo vệ trong bản cấu hình cleanup riêng đã kiểm tra. Giữ tài nguyên baseline, bootstrap/state và role Auto Scaling có sẵn.
- [ ] Dừng gateway sạch và thu `shutdown complete` của đúng invocation, đợi target drain. Xóa root disk synthetic không backup theo duyệt. Kiểm cả node thay thế và detached volume bằng inventory native.
- [ ] KMS/secret mới schedule deletion 30 ngày. Xóa ACM, runtime IAM và service-linked roles tạo trong lượt. ELB role có thể còn bị AWS từ chối vì tham chiếu ALB cũ, theo dõi và thử lại đến khi có `GetRole NoSuchEntity`.
- [ ] Khôi phục default versions và document IAM, xóa toàn bộ policy versions/grants tạo trong lượt, so sánh policy baseline. Xóa auditor sau bước kiểm tra đầu.
- [ ] Chạy `verify_teardown_absence.py --final-audit` với danh tính root chỉ đọc và đầy đủ frozen artifacts. Xác nhận không thiếu tài nguyên baseline, role auditor mới đã biến mất, IAM khớp snapshot và chỉ còn đúng các ID KMS/secret đã được duyệt chờ xóa. `UNVERIFIED`, tài nguyên baseline thiếu hoặc tài nguyên dư cần tiếp tục đối chiếu.
- [ ] Đăng xuất root, dừng credential bridge/tunnel thuộc lượt, xóa mã login khỏi file. Lưu private progress/handoff và các resource ngoại lệ cần theo dõi.

## 6. Chốt kết quả

- [ ] Báo kết quả từng gate, source/release/dataset hashes, giới hạn bằng chứng, native cleanup ledger và ước tính chi phí. Đối soát billing khi AWS cập nhật.
- [ ] Commit evidence đã rút gọn và comment tiến độ/kết quả trên PR bằng account `KinasTomes`.
- [ ] Chỉ đề nghị chuyển PR sang ready và nghiệm thu khi các gate bắt buộc có bằng chứng đạt trên AWS. Nếu còn lỗi, bàn giao đúng điểm lỗi và bản sửa cần chạy lại.

## Hồ sơ riêng trên máy hiện tại

Thư mục `/home/ubuntu/.local/state/miles/edge-acceptance-prep/run` giữ `progress.json`, baseline, saved plans, activation/rollback ledger, log và evidence gốc. `final-absence-summary.json` là kết quả cleanup, `cost-observed-estimate.json` là bản tính chi phí. Nội dung có thể chứa secret/state, chỉ đọc trong môi trường được phép và giữ quyền truy cập riêng.

Các script riêng trong thư mục này thuộc lượt đã kết thúc. Khi thực hiện sau, tạo thư mục/run ID mới, đọc và cập nhật guards, tên tài nguyên, deadline, source hash và policy expiry trước khi tái sử dụng. Saved plans, authorization codes, tokens, TLS identity và temporary grants cũ cần thay bằng dữ liệu mới của lượt được duyệt.
