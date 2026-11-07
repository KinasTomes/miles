# DynamoDB Runtime Observation Bounded Load and Cost Analysis

> **Trạng thái:** Hoàn thành Phân tích Kỹ thuật & Remediation (PR #65)
> **Tác giả:** Trịnh Quang Hưng
> **Mục đích:** Đánh giá tải, chi phí và hiện tượng throttling của cơ chế quan trắc runtime (`runtimeObserver`) trên DynamoDB, đồng thời xác minh các biện pháp chặn tải (bounded load controls).

---

## 1. Bối cảnh & Vấn đề Kỹ thuật (P1 Finding)

Trong phiên bản ban đầu của PR #65:
- `internal/app/shared.go` kích hoạt `runtimeObserver` vô điều kiện trên mọi gateway Cloud và Edge.
- `internal/app/operations.go` thực hiện chu kỳ thu thập mỗi 15 giây (`15 * time.Second`).
- Mỗi chu kỳ quan sát gọi:
  1. `m.GetR1Gate(ctx)` (point read, 1 GetItem)
  2. `m.GetRecoveryState(ctx, history)` (point read, 1 GetItem)
  3. `m.GetRecoveryEpoch(ctx, history, epoch)` (point read, 1 GetItem)
  4. Quét bảng `Uploads` (`m.ObservePage`) tối đa 32 trang $\times$ 128 items = 4.096 items với `ConsistentRead = true`.
  5. Quét bảng `Outbox` (`m.ObserveExportLag`) tối đa 32 trang $\times$ 128 items = 4.096 items với `ConsistentRead = true`.

### Phân tích Tải Cực Đại (Worst-Case Load Trước Khi Sửa)
- Đối với cụm 3 node (2 Cloud + 1 Edge):
  - Số chu kỳ quan sát mỗi phút: $3 \times 4 = 12\text{ chu kỳ/phút}$.
  - Mỗi chu kỳ tối đa: 3 GetItems + 32 Scan (`Uploads`) + 32 Scan (`Outbox`) = 67 DynamoDB API calls.
  - Tổng số Scan calls: $12 \times 64 = 768\text{ Scans/phút} = 46.080\text{ Scans/giờ} \approx 1.105.920\text{ Scans/ngày}$.
- **Hệ quả tiêu cực:**
  1. **Throttling Pressure:** Quét toàn bảng với `ConsistentRead = true` tập trung tải lên partition leader, dễ gây `ProvisionedThroughputExceededException` (DynamoDB throttling).
  2. **Nhiễu Benchmark:** Khi chạy kiểm thử hiệu năng (`warmbench`), background scans liên tục tranh chấp Read Capacity Units (RCU) với luồng ghi/đọc PUT/GET của benchmark, làm tăng tail latency (P99) và gây lỗi timeout/503.
  3. **Chi phí On-Demand RCU:** 1,1 triệu Scan requests/ngày tạo ra phụ phí không cần thiết cho telemetry.

---

## 2. Giải pháp Triển khai (Remediation Architecture)

Để giải quyết triệt để rủi ro trên mà vẫn đảm bảo tính chính trực của telemetry và R1/R2, kiến trúc quan trắc được tái cấu trúc theo mô hình **Shallow by Default, Deep Opt-In**:

### 2.1. Phân tách Quan trắc Nông (Shallow) và Quét Sâu (Deep)
- **Shallow Observation (Mặc định):**
  - Chỉ thực hiện 3 lệnh GetItem trực tiếp theo primary key ($O(1)$ point lookups): `GetR1Gate`, `GetRecoveryState`, và `GetRecoveryEpoch`.
  - Cập nhật đầy đủ các tín hiệu sống còn: `CutAt` (đo độ tươi của checkpoint/RPO), `GateClosed` (trạng thái đóng/mở gate), `GateStartedAt` (thời điểm gate đóng), và `miles_runtime_observation_age_seconds`.
  - **Không thực hiện bất kỳ lệnh Scan nào trên bảng dữ liệu.**
- **Deep Observation (Opt-in Bounded):**
  - Chỉ kích hoạt khi cấu hình khởi động bật rõ ràng `deep_observation = true`.
  - **Benchmark:** Gateway shared Cloud/Edge chạy `mode = "runtime"`; không có chế độ tự động tắt deep scan khi client benchmark bắt đầu chạy. Với benchmark, phải cấu hình `deep_observation = false` từ trước. Shallow không quét bảng, nhưng vẫn thực hiện các point reads.
  - **Giới hạn số trang (Page Bound):** Bảng `Uploads` chỉ quét tối đa 4 trang (tối đa 512 items thay vì 4.096 items). Nếu sau trang thứ tư vẫn còn cursor, trả `ErrUnavailable`; không công bố số liệu bán phần.
  - **Chuyển sang Eventual Consistency cho Outbox Scan:** Trong `ObserveExportLag`, chuyển từ `ConsistentRead: true` sang `ConsistentRead: false`. Vì outbox lag chỉ là chỉ số telemetry xấp xỉ (`approximate base scan, never a closure proof`), đọc eventual giảm 50% RCU và không gây khóa trên partition leader.

### 2.2. Điều chỉnh Tần số Quan trắc (Polling Interval)
- Tăng chu kỳ nghỉ mặc định từ 15 giây lên **30 giây** (`observation_interval_seconds = 30`), tạo biên thời gian trước ngưỡng stale gauge 60 giây; thời gian thực giữa các lần đo còn bao gồm thời gian collect và scheduling.
- Giảm khoảng 50% số chu kỳ so với mặc định 15 giây trước khi sửa.

---

## 3. Bảng So sánh Định lượng (Quantified Impact)

| Thông số / Hạng mục | Trước Khi Sửa | Sau Khi Sửa (Mặc định Shallow) | Sau Khi Sửa (Deep Opt-In) |
| :--- | :--- | :--- | :--- |
| **Chu kỳ quan trắc** | 15 giây | 30 giây (cấu hình 0..600s; 0 dùng mặc định) | 30 giây |
| **Số Scan Uploads / chu kỳ** | 1 – 32 | **0** | 1 – 4 |
| **Số Scan Outbox / chu kỳ** | 1 – 32 | **0** | 1 – 4 |
| **Tính nhất quán Outbox Scan** | `ConsistentRead=true` | N/A | `ConsistentRead=false` (50% RCU) |
| **Point GetItems / chu kỳ** | 3 | 3 | 3 |
| **Scan Calls / phút (cụm 3 node)** | Up to 768 Scans/phút | **0 Scans/phút** | Up to 48 Scans/phút (3 node × 2 chu kỳ × 8 scans) |
| **RCU/tháng thực tế** | Chưa có số đo đủ để định lượng | Cần đo số point reads, kích thước item và `ConsumedReadCapacityUnits` | Cần đo thêm RCU do bounded scans; không suy ra RCU chỉ từ số lần gọi `Scan` |
| **Tác động lên Benchmark Trials** | Nhiễu đọc nền đáng kể | Không có Scan, nhưng vẫn có point reads | Vẫn quét nếu bật cờ; phải tắt rõ ràng trước benchmark |
| **Độ phủ CloudWatch Alarms** | 5 alarms/profile trước đây | 4 base alarms/profile; không xuất `stuck_completing` hoặc `export_lag` chưa đo | 5 alarms/profile khi runtime và Terraform cùng bật deep; kết quả quá giới hạn scan bị withhold |

---

## 4. Xác minh Thực nghiệm (Verification & Tests)

1. **Unit Test xác nhận Opt-In (`TestLocalRuntimeObservationDeepScanOptIn`):**
   - Chạy với `deep = false`: Không phát sinh scan; `StuckCompleting = 0`, `OldestPendingExport = zero`, `CutAt` được tính toán chính xác từ epoch sealed.
   - Chạy với `deep = true`: Quét bounded (tối đa 4 trang), lấy dữ liệu stuck và outbox lag hợp lệ.
2. **DynamoDB Eventual Consistency (`TestExportLagRejectsLegacyUnknownAndCursorLoop`):**
   - Xác nhận `ObserveExportLag` truyền `ConsistentRead: false`, giảm tải RCU và vượt qua kiểm thử vòng lặp cursor.
3. **Cấu hình Khởi động (`internal/config`):**
   - Bổ sung `observation_interval_seconds` (mặc định 30s, kiểm tra phạm vi hợp lệ 0..600; 0 dùng mặc định) và `deep_observation` (mặc định `false`). Terraform truyền cùng giá trị `deep_observation` tới fleet và module alarms; chỉ tạo alarm Completing khi bật deep.


## 5. Giới hạn và nghiệm thu

Tỷ lệ giảm **Scan calls** theo giới hạn lý thuyết không chứng minh cùng tỷ lệ giảm RCU hoặc chi phí. DynamoDB tính mức đọc theo kích thước dữ liệu được quét, loại nhất quán và cấu hình tính phí; cần lấy số đo thực tế để kết luận. Kết quả quá giới hạn phân trang hoặc thất bại của deep observer được đánh dấu không khả dụng, không xuất số liệu hoàn chỉnh giả.

Việc merge phần implementation và unit/contract tests không thay thế cho Cloud/Edge/direct S3 AWS live acceptance, kiểm tra R1/R2, chi phí endpoint/PITR và dọn dẹp. Các checklist chưa hoàn tất vẫn thuộc `W05-HUNG-03` và được thực hiện trong follow-up riêng sau khi có phê duyệt AWS.
