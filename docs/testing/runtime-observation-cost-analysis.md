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
  - **Chặn tải trong chế độ Benchmark:** Khi gateway chạy ở `mode = "benchmark"`, cờ `deep` bị cưỡng chế vô hiệu hóa (`deep = false`), bảo đảm zero-overhead và không tranh chấp tài nguyên với workload thử nghiệm.
  - **Giới hạn số trang (Page Bound):** Bảng `Uploads` chỉ quét tối đa 4 trang (tối đa 512 items thay vì 4.096 items).
  - **Chuyển sang Eventual Consistency cho Outbox Scan:** Trong `ObserveExportLag`, chuyển từ `ConsistentRead: true` sang `ConsistentRead: false`. Vì outbox lag chỉ là chỉ số telemetry xấp xỉ (`approximate base scan, never a closure proof`), đọc eventual giảm 50% RCU và không gây khóa trên partition leader.

### 2.2. Điều chỉnh Tần số Quan trắc (Polling Interval)
- Tăng chu kỳ nghỉ mặc định từ 15 giây lên **60 giây** (`observation_interval_seconds = 60`), đồng bộ với chu kỳ đánh giá của CloudWatch Alarms (`period = 60`).
- Giảm ngay lập tức 75% tần suất gọi API của mỗi worker quan trắc.

---

## 3. Bảng So sánh Định lượng (Quantified Impact)

| Thông số / Hạng mục | Trước Khi Sửa | Sau Khi Sửa (Mặc định Shallow) | Sau Khi Sửa (Deep Opt-In) |
| :--- | :--- | :--- | :--- |
| **Chu kỳ quan trắc** | 15 giây | 60 giây (configurable 10..600s) | 60 giây |
| **Số Scan Uploads / chu kỳ** | 1 – 32 | **0** | 1 – 4 |
| **Số Scan Outbox / chu kỳ** | 1 – 32 | **0** | 1 – 4 |
| **Tính nhất quán Outbox Scan** | `ConsistentRead=true` | N/A | `ConsistentRead=false` (50% RCU) |
| **Point GetItems / chu kỳ** | 3 | 3 | 3 |
| **Scan Calls / phút (cụm 3 node)** | Up to 768 Scans/phút | **0 Scans/phút** | Up to 24 Scans/phút |
| **RCU tiêu thụ / tháng (ước tính)** | ~35.000.000 RCU | **~390.000 RCU (-98.9%)** | ~1.800.000 RCU (-94.8%) |
| **Tác động lên Benchmark Trials** | Gây nghẽn RCU & latency spike | **Hoàn toàn bị cô lập (0 Scans)** | Bị vô hiệu hóa tự động |
| **Độ phủ CloudWatch Alarms** | Đầy đủ | `recovery_cut_age`, `telemetry_age` hoạt động 100% | Đầy đủ mọi metric |

---

## 4. Xác minh Thực nghiệm (Verification & Tests)

1. **Unit Test xác nhận Opt-In (`TestLocalRuntimeObservationDeepScanOptIn`):**
   - Chạy với `deep = false`: Không phát sinh scan; `StuckCompleting = 0`, `OldestPendingExport = zero`, `CutAt` được tính toán chính xác từ epoch sealed.
   - Chạy với `deep = true`: Quét bounded (tối đa 4 trang), lấy dữ liệu stuck và outbox lag hợp lệ.
2. **DynamoDB Eventual Consistency (`TestExportLagRejectsLegacyUnknownAndCursorLoop`):**
   - Xác nhận `ObserveExportLag` truyền `ConsistentRead: false`, giảm tải RCU và vượt qua kiểm thử vòng lặp cursor.
3. **Cấu hình Khởi động (`internal/config`):**
   - Bổ sung `observation_interval_seconds` (mặc định 60s, kiểm tra phạm vi hợp lệ 0..600) và `deep_observation` (mặc định `false`). Toàn bộ test suite `internal/config` đạt PASS.
