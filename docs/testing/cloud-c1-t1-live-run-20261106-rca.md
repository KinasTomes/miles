# Báo cáo kết quả thử nghiệm Live Cloud C1 & Phân tích sự cố kỹ thuật

> **Báo cáo kỹ thuật — Workstream D | Live Run 2026-11-06**  
> **Tài khoản AWS:** `499388132734` | **Region:** `ap-southeast-1`  
> **Commit Gateway:** `4a0c1d9` (PR #65 Draft) | **Branch:** `feat/dynamodb-pilot-observability`  
> **Trạng thái:** Dọn dẹp hoàn tất 100% (`D4 Teardown: PASS`), PR vẫn Draft, Task `W05-HUNG-03` vẫn Doing.

---

## 1. Mục tiêu và Bối cảnh thử nghiệm

Đợt thử nghiệm live ngày 2026-11-06 (Workstream D, Gate D) được thực hiện nhằm:
1. Đánh giá hành vi ghi (PUT) và đọc (GET/HEAD) trên hạ tầng Cloud đa node (`cloud-a`, `cloud-b`) và Edge với cấu hình DynamoDB `PAY_PER_REQUEST`.
2. Kiểm chứng khả năng tương quan lỗi end-to-end (`RequestID`, `NodeID`, `stage`, `failure_class`) qua ALB nội bộ.
3. Chạy workload benchmark `backup30` (concurrency = 1, 90 files, tổng dung lượng ~720 MiB).
4. Thực thi quy tắc an toàn **Stop-the-line** và **D4 Teardown toàn diện** trong ngân sách mềm 10 USD và khung thời gian cho phép.

---

## 2. Kết quả Benchmark tổng quan

Workload gồm 4 phase liên tục được điều phối từ máy trạm `pilot_client` qua HTTPS ALB nội bộ:

| Phase | Số lượng request | Thành công | Thất bại | Tỉ lệ thành công | Thông số hiệu năng (Latency) | Ghi chú |
| :--- | :---: | :---: | :---: | :---: | :---: | :--- |
| **`preflight-list`** | 1 | 1 | 0 | **100%** | ~12.4 ms | Bucket list tiền kiểm tra sẵn sàng |
| **`put`** | 90 | 90 | 0 | **100%** | p50: 1,932 ms, min: 1,489 ms, max: 3,892 ms | 754,994,670 bytes đã commit. 0 lỗi 500/503 trên mẫu này. |
| **`head`** | 90 | 90 | 0 | **100%** | p50: 9.2 ms, min: 4.8 ms, max: 21.3 ms | Toàn bộ 90 objects tồn tại trong DynamoDB |
| **`verify-get`** | 90 | 84 | 6 | **93.33%** | p50: 1,410 ms (các file thành công) | 1 partial read (#79), 5 HTTP 500 (#81, #83, #85, #87, #89) |

### Nhận định về Write Path (PUT):
- Toàn bộ 90/90 PUTs thành công trên mẫu thử nghiệm này, 90 objects được commit vào bảng `miles-dev-Objects`.
- **Hiệu chỉnh kết luận:** Cả hai kế hoạch Terraform (lượt lỗi 2026-11-05 và lượt 2026-11-06) đều đã sử dụng `billing_mode = PAY_PER_REQUEST` với cùng hạn ngạch 128 read/write (riêng bảng `Chunks` giới hạn 512 read/128 write). Kết quả 90/90 PUT là quan sát thành công trên một mẫu chạy đơn lẻ, không thể khẳng định là do "chuyển sang On-Demand đã sửa lỗi PUT cũ".

---

## 3. Chi tiết 6 request GET thất bại

| STT Request | Tên đối tượng | Kích thước | Gateway Node | HTTP Code / Lỗi | Dữ liệu nhận được | Triệu chứng ghi nhận |
| :---: | :---: | :---: | :---: | :---: | :---: | :--- |
| **#79** | `archive-part-079.tar` | 8,388,608 B | `cloud-a` | Stream Abort / EOF | 4,901,763 B (~58.4%) | Header 200 đã gửi; stream bị ngắt giữa chừng |
| **#81** | `archive-part-081.tar` | 8,388,608 B | `cloud-a` | HTTP 500 `InternalError` | 0 B | `stage: object_get, failure_class: unknown` |
| **#83** | `archive-part-083.tar` | 8,388,608 B | `cloud-a` | HTTP 500 `InternalError` | 0 B | `stage: object_get, failure_class: unknown` |
| **#85** | `archive-part-085.tar` | 8,388,608 B | `cloud-a` | HTTP 500 `InternalError` | 0 B | `stage: object_get, failure_class: unknown` |
| **#87** | `archive-part-087.tar` | 8,388,608 B | `cloud-a` | HTTP 500 `InternalError` | 0 B | `stage: object_get, failure_class: unknown` |
| **#89** | `archive-part-089.tar` | 8,388,608 B | `cloud-a` | HTTP 500 `InternalError` | 0 B | `stage: object_get, failure_class: unknown` |

Tất cả các request chẵn (#80, #82, #84, #86, #88, #90) được ALB định tuyến sang node **`cloud-b`** đều **PASS 100%**. Toàn bộ 6 request lỗi đều tập trung vào node **`cloud-a`**.

---

## 4. Phân tích sự cố và Phân loại mức độ bằng chứng

Nhằm bảo đảm tính khách quan khoa học, các phát hiện được phân tách nghiêm ngặt theo 4 cấp độ:

### 4.1. Observed (Quan sát trực tiếp từ Native Evidence đã lưu giữ)
1. **Workload:** 90/90 PUT thành công, 90/90 HEAD thành công, 84/90 GET thành công, 1 request (#79) đứt stream sau 4.9MB, 5 request (#81, #83, #85, #87, #89) trả HTTP 500.
2. **Log Correlation:** Request ID từ client của cả 5 request HTTP 500 khớp chính xác với 5 bản ghi JSON log thu được từ journal của `cloud-a`, đều ghi nhận `stage=object_get` và `failure_class=unknown`.
3. **Stream Abort Log Gap:** Request #79 không có bản ghi structured failure log tương ứng trong journal. Phân tích mã nguồn [`internal/api/s3/object.go:213-217`](file:///home/ubuntu/miles-worktrees/dynamodb-pilot-observability/internal/api/s3/object.go#L213-L217) xác nhận: khi `wrote == true` (headers đã gửi), mã nguồn gọi trực tiếp `panic(http.ErrAbortHandler)` mà bỏ qua `r.recordFailure`, dẫn đến thiếu hụt log chẩn đoán cho request bị đứt stream.
4. **Post-failure Metrics:** Trong tệp `cloud-c1-t1-failed-window.json`, snapshot sau sự cố cho thấy `cloud-a` có `miles_ready=0`, trong khi `cloud-b` và `edge-a` có `miles_ready=1`.
5. **Telemetry Counter:** Chỉ số `miles_warm_read_integrity_failures_total` của benchmark ghi nhận giá trị `0`.

### 4.2. Correlated (Tương quan kỹ thuật có cơ sở)
1. **Tính chất cục bộ:** Toàn bộ 6 lỗi chỉ xuất hiện trên `cloud-a`, trong khi `cloud-b` phục vụ 45 request thành công 100%. Điều này chỉ ra nguyên nhân là sự cố cục bộ trên `cloud-a` (như đóng Warm Gate / mất readiness), không phải sự cố chung của tầng lưu trữ S3 hay DynamoDB.
2. **Tương quan Readiness:** Trạng thái `miles_ready=0` trên `cloud-a` xuất hiện đồng thời với thời điểm phát sinh lỗi GET, cho thấy cơ chế bảo vệ an toàn (gate check) đã ngăn chặn phục vụ đọc.

### 4.3. Hypothesis (Giả thuyết kỹ thuật phù hợp dữ liệu)
1. **Giả thuyết gián đoạn đồng bộ NTP (Chrony):**
   - Logic an toàn trong [`internal/coordination/chrony.go:93-96`](file:///home/ubuntu/miles-worktrees/dynamodb-pilot-observability/internal/coordination/chrony.go#L93-L96) quy định:
     ```go
     age := float64(now.WallUTC.UnixNano())/1e9 - refTime
     if stratum < 1 || stratum > 15 || math.Trunc(stratum) != stratum || r[13] != "Normal" ||
        age < -2 || age > min(1024, 2*interval+2) || interval <= 0 || delay < 0 ||
        dispersion < 0 || skew < 0 || math.Abs(offset) > 2 {
         return policy.UTCSample{}, bad
     }
     ```
   - Khi độ tuổi mẫu NTP vượt quá ngưỡng `min(1024, 2*interval+2)` (với polling interval ~16s, ngưỡng là ~34s), `Chrony.Bounds()` từ chối cấp lease thời gian theo nguyên tắc fail-closed.
   - Nếu node `cloud-a` bị trễ gói tin NTP từ `169.254.169.123` khiến khoảng cách giữa 2 gói tin vượt quá ~34s, gate sẽ đóng, dẫn đến việc stream đang chạy bị hủy và các request tiếp theo trả về HTTP 500 với `failure_class: unknown` (do lỗi chưa được định danh kiểu).
   - **Lưu ý quan trọng:** Trong archive bằng chứng hiện tại không lưu tệp nhật ký độc lập `tracking.log` chứa mốc thời gian cụ thể `05:27:58.204Z / 05:28:47.412Z`. Do đó, đây là giả thuyết kỹ thuật hợp lý và phù hợp nhất với dữ liệu quan sát, nhưng chưa được chứng thực độc lập từ bằng chứng gốc.

### 4.4. Not verified / Unproven (Chưa có bằng chứng chứng minh)
1. **Khẳng định "Zero Data Corruption" và tính toàn vẹn 100% của 6 file lỗi:** Mặc dù counter `miles_warm_read_integrity_failures_total` của benchmark là `0`, nhưng không có tệp lưu trữ checksum độc lập riêng lẻ cho 6 file lỗi trong lần chạy này. Không dùng counter benchmark để khẳng định tuyệt đối tính toàn vẹn khi chưa có bằng chứng tải lại độc lập được lưu vết.
2. **Khẳng định "On-Demand sửa lỗi PUT":** Như đã nêu tại mục 2, cả hai lần chạy đều dùng `PAY_PER_REQUEST`. Không có cơ sở quy kết 90/90 PUT là nhờ thay đổi cấu hình dung lượng.

---

## 5. Thực thi Stop-the-line và D4 Full Teardown

Theo đúng quy chuẩn an toàn:
1. **Dừng toàn bộ các trial tiếp theo:** Dừng ngay kế hoạch C4 (Concurrency 4) và Edge để bảo toàn chi phí và chứng cứ hiện trường.
2. **Lưu trữ bằng chứng:**
   - 3,286 bản ghi từ 20 bảng DynamoDB được xuất lưu an toàn vào `retained-metadata-evidence/`.
   - Lưu trữ danh mục 1,228 object versions và delete markers từ S3 bucket trước khi làm sạch.
3. **Phá hủy tài nguyên trên AWS:**
   - Tắt các admin reverse SSH tunnels.
   - Làm trống hoàn toàn 2 S3 buckets (`miles-dev-data-...` và `miles-dev-pilot-direct-...`).
   - Gỡ bỏ deletion protection của 20 bảng DynamoDB.
   - Xóa KeyPair `miles-dev-edge-validation`, RolesAnywhere Profile & Trust Anchor, IAM role `miles-dev-edge`.
   - Áp dụng Terraform destroy: Toàn bộ 136 tài nguyên workload và foundation đã bị hủy bỏ (`Total resources in state: 0`).
   - Xóa ACM Certificate và SSM Session Document `miles-dev-shell`.
   - Xóa thành công cả 2 IAM Service-Linked Roles (`AWSServiceRoleForRolesAnywhere`, `AWSServiceRoleForElasticLoadBalancing`) với trạng thái `SUCCEEDED` / `NoSuchEntity`.
   - Khôi phục 5 IAM Managed Policies về đúng baseline versions (`v1`, `v2`, `v1`, `v1`, `v3`) và xóa bỏ toàn bộ các versions tạm thời. Xóa inline policy `MilesPilotAcceptance`.
4. **Xác minh Native AWS Absence (`verify-final-aws-cleanup.py`):**
   - **41/41 hạng mục kiểm tra đều PASS tuyệt đối** (`native-final-cleanup-proof.json`).
   - KMS Key đã được lên lịch xóa (`PendingDeletion`).
   - Secrets Manager secret đã được lên lịch xóa (`DeletedDate`).
   - Chỉ duy nhất bucket Terraform state `miles-dev-tfstate-...` được bảo lưu theo đúng thiết kế.
5. **Chi phí và Thời gian:**
   - Hoàn thành teardown trong khoảng 1.5 giờ. Chi phí ước tính phát sinh trong phiên dưới 1.00 USD (nằm trong soft budget 10 USD); chi phí chính thức cần chờ báo cáo cập nhật từ AWS Billing.

---

## 6. Các hành động khắc phục kỹ thuật (Remediation Tasks)

1. **Khắc phục Structured Logging cho Stream Abort (Task T1 — P0):**
   - Bổ sung ghi nhận log an toàn tại [`internal/api/s3/object.go`](file:///home/ubuntu/miles-worktrees/dynamodb-pilot-observability/internal/api/s3/object.go) với `stage: "object_get_stream"` trước khi gọi `panic(http.ErrAbortHandler)` khi headers đã gửi.
   - Bảo đảm đầy đủ `request_id`, `node_id`, `operation`, `stage`, `failure_class` mà không làm thay đổi streaming abort contract (không ghi XML vào body đã gửi).
2. **Định danh kiểu lỗi Clock Uncertainty (Task T2 — P0):**
   - Bổ sung `storage.ErrClockUncertain` và ánh xạ sang `failure_class: "clock_uncertainty_exceeded"` trong router S3 để loại bỏ mã generic `unknown`.
   - Bổ sung regression tests kiểm chứng ranh giới age `min(1024, 2*interval+2)`, packet gap, và cơ chế tự phục hồi (self-recovery). Tuyệt đối không tự ý nới lỏng hệ số bảo vệ đồng hồ.
3. **Module hóa Runner Diagnostics (Task T4 — P1):**
   - Tách logic trích xuất log và đối chiếu correlation thành module [`scripts/runner_diagnostics.py`](file:///home/ubuntu/miles-worktrees/dynamodb-pilot-observability/scripts/runner_diagnostics.py) có kiểm thử tự động, hỗ trợ phân loại rõ ràng giữa lỗi gateway, lỗi upstream ALB, và trạng thái thiếu bằng chứng thu thập (`uncollected_evidence`).
4. **Kế hoạch PR #65:**
   - Tiếp tục giữ PR #65 ở trạng thái **Draft**.
   - Tiếp tục giữ Task ID `W05-HUNG-03` ở trạng thái **Doing**.
   - Mọi hoạt động triển khai lại trên AWS (P3) chỉ được thực hiện khi có phê duyệt ngân sách và thời hạn mới từ User/Lead.
