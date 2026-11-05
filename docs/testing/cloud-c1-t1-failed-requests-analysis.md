# Phân tích chi tiết và đối chiếu sự cố Cloud C1 (PR #65)

> **Báo cáo kỹ thuật — Workstream A1 & B1 | Trạng thái: Hoàn thành Offline**  
> Dữ liệu phân tích dựa trên kết quả chạy thử nghiệm live Cloud C1 tại commit `0edbe26` (dataset `backup30`, concurrency 1).  
> Báo cáo này đã được khử định danh (sanitized), không chứa thông tin nhạy cảm (secrets, ARN riêng tư, raw tokens).

---

## 1. Tổng quan kết quả kiểm thử Cloud C1 (90 PUTs)

Workload kiểm thử gồm 1 phase `preflight-list` (1 sample, PASS) và 90 requests `PutObject` tuần tự (concurrency = 1, dataset `backup30`, mỗi file ~8 MiB):
* **Tổng số PUT thực hiện:** 90
* **Thành công (PASS):** 88/90 (97.78%)
* **Thất bại (FAIL):** 2/90 (2.22%)
* **Phân bố độ trễ PUT thành công:**
  * Min: 1,512.7 ms
  * Median (p50): 7,832.8 ms
  * Max: 14,086.7 ms

---

## 2. Bảng đối chiếu chi tiết 2 request thất bại

| Thứ tự PUT | File đối tượng | Kích thước | Độ trễ | HTTP Code | Mã lỗi SDK | Gateway Node | Trạng thái DDB Lease | Trạng thái DDB Quota | Trạng thái DDB ObjectHead | Tầng phát sinh lỗi (Stage) |
| :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **#71** | `compressed-identical-v23.gz` | 8,389,273 B | 8,885.08 ms | **503** | `ServiceUnavailable` | `cloud-b` | Đã cấp $\rightarrow$ `Released` (op_id: `dd56ab...`) | Đã giữ $\rightarrow$ `Released` (8,389,273 B) | Không có (chưa commit) | `prepare` (xử lý body/pack pipeline) |
| **#73** | `low-entropy-v24.bin` | 8,388,608 B | 233.41 ms | **500** | `InternalError` | Gateway (Cloud) | Không có (chưa đăng ký) | Không có | Không có | `credential_lookup` hoặc `authorize` |

---

## 3. Phân tích nguyên nhân và bằng chứng (RCA)

### Request #71 (HTTP 503 `ServiceUnavailable`, latency ~8.88s)
1. **Tiến trình xử lý:**
   - Request đã vượt qua các bước xác thực (authentication) và phân quyền (authorization).
   - Đã qua bước kiểm tra object tồn tại (`object_lookup`).
   - Đã đăng ký writer lease thành công trên node `cloud-b` (`i-0e493e4443d4f8cf0`).
   - Đã đặt trước quota (Quota Reservation) cho dung lượng 8,389,273 bytes.
   - Đã bind root manifest `69d1885d...` vào lease.
   - Khi thực thi pipeline streaming `Prepare` (chunking, compression, ghi pack lên S3 data bucket và DynamoDB chunk records): phát sinh lỗi tầng backend (`storage.ErrUnavailable` hoặc timeout).
2. **Hành vi an toàn của hệ thống (Correct Safety Behavior):**
   - Cơ chế thu hồi an toàn (`s.release`) được kích hoạt ngay lập tức trong khối `defer`.
   - Cả writer lease và quota reservation đều được chuyển sang trạng thái `Released`.
   - **Không tạo duplicate object head**, không để rò rỉ quota (no quota leak), không phá vỡ fencing.
   - Gateway phân loại chính xác lỗi thành HTTP 503 `ServiceUnavailable`.

### Request #73 (HTTP 500 `InternalError`, latency ~233ms)
1. **Tiến trình xử lý:**
   - Request **hoàn toàn chưa đăng ký lease** và **chưa giữ quota**.
   - Thất bại ở giai đoạn rất sớm (early gateway phase).
   - Độ trễ 233.41 ms khớp chính xác với chu kỳ retry bounded của DynamoDB store (`m.readItem` với exponential jitter backoff qua ~5–6 attempts tốn khoảng 200–300 ms) khi gặp lỗi tạm thời (như `ProvisionedThroughputExceededException`, `InternalServerError` hoặc connection glitch).
2. **Khuyết tật phân loại (Classification Defect):**
   - Khi metadata backend trả về lỗi `storage.ErrUnavailable` trong quá trình `Credential` lookup hoặc `Authorize` snapshot lookup, router S3 bọc lỗi thành generic error và ánh xạ sang HTTP 500 `InternalError`.
   - Về mặt S3 compatibility spec, AWS S3 trả 500 khi internal auth subsystem lỗi. Tuy nhiên trên tầng quan sát, log chẩn đoán cần ghi nhận rõ `stage: "credential_lookup"` và `failure_class: "backend_unavailable"` để tách biệt với crash hệ thống.

---

## 4. Tách biệt Bằng chứng đã xác nhận vs Giả thuyết

| Nội dung | Phân loại | Bằng chứng chứng minh |
| :--- | :---: | :--- |
| Request #71 thất bại trong `prepare`, đã dọn dẹp lease & quota an toàn | **ĐÃ CHỨNG MINH** | Dữ liệu `miles-dev-Leases.json` và `QuotaReservations.json` giữ op_id, root, và trạng thái `Released`. |
| Request #73 thất bại trước writer admission, không tạo lease | **ĐÃ CHỨNG MINH** | Không có bất kỳ bản ghi lease hay quota reservation nào cho đối tượng này. |
| Runner không đọc được log journalctl | **ĐÃ CHỨNG MINH** | SSM Session Manager lỗi `Standard_Stream not found`; binary ghi log dạng text thay vì JSON do chưa kích hoạt `slog.SetDefault(logger)`. |
| Request #71 thất bại do S3 data bucket chậm/timeout vs DDB chunk write | *Giả thuyết* | Cần đường log correlation chi tiết của đợt rerun mới để xác định chính xác I/O call cụ thể. |
| Request #73 thất bại do DynamoDB Read Throughput Throttling vs network socket | *Giả thuyết* | Metric `before` ghi nhận 0 throttle, nhưng không thu được `after` metrics do SSM Session Manager lỗi. |
