# Ngưỡng benchmark và bằng chứng nghiệm thu

**Trạng thái:** candidate thresholds để review, **chưa được nhóm phê duyệt hoặc xác nhận đạt**. Giữ các mục tiêu gốc ở [definition of done](../docs/acceptance/definition-of-done.md) và [baseline](baseline.md); không âm thầm hạ threshold khi pilot không đạt. Định nghĩa và phạm vi không được thay chỉ để có số PASS.

| Chỉ số | Công thức và scope | Mục tiêu đề xuất | Quy tắc kết luận |
|---|---|---|---|
| Dedup (BEN-01) | `live logical bytes / unique live raw bytes` đếm `(domain, hash)` một lần; áp cho backup có phiên bản giống/đổi nhỏ | ≥5× | Tách random-distinct và domains; không dùng total reduction thay dedup |
| Cache (BEN-02) | `hit / (hit + miss)` cùng một granularity request/segment/byte xác định trước; Warm read sau verified reset | ≥80% | Cần cache counters từng node và cold/warm state; 3 lần GET lặp **không đủ** |
| Single PUT (BEN-03) | `successful logical bytes / elapsed`, Miles C1 / direct S3 C1 cùng client/network, single PUT, không multipart | ≥50% | C1 riêng với C4; report full sample/N/p95/repetitions/CPU/errors; lỗi không bị lọc khỏi evidence |
| PUT reduction (BEN-04) | `1 - (actual DSG PUT / chunk-per-object PUT)` cùng workload/interval; tính control, async, retry | ≥90% | **Không** thay chunk-per-object bằng direct S3; không đủ inventory → unknown |
| Integrity (BEN-05) | `full-file SHA-256 verified / total cases required to verify` và log mọi lỗi | 100% verified cases | Fail/hash mismatch/lost committed data chưa sửa là blocker; case không chạy = chưa đủ coverage, không phải 100% |
| Net saving | `cost_direct - cost_DSG` đầy đủ storage byte-time/requests/data transfer/control/infra/retention | Không có ngưỡng số được phê duyệt | Báo cả âm/0/dương; missing/unpriced price hoặc incomplete baseline → **unknown**, không công bố tiết kiệm |

**Bắt buộc phân biệt:** `dedup = live_logical / unique_raw`; `compression = unique_raw / unique_stored`; `total_reduction = live_logical / physical_retained`. Denominator 0/unknown → `null`/không thể kết luận. Tăng physical retained do revival và controls có thể khiến net saving âm dù dedup tốt. Cost dùng giá theo Region/ngày nguồn, không lấy simulator forecast làm bill thật.

## Baseline pilot đã công bố (không phải acceptance)

Báo cáo [AWS ngày 08/11/2026](../docs/testing/live-acceptance-20261108-report.md) ghi Cloud C1 4,56 MB/s và direct S3 C1 78,22 MB/s: tỷ lệ **xấp xỉ 5,83%**, thấp hơn mục tiêu 50% trong trial này. Cloud C1 dedup **1,94×** trên dataset tổng hợp, thấp hơn 5×; tỷ số đó **không** chứng minh mọi workload entropy đều thất bại. Cloud C4 thành công về tính toàn vẹn nhưng không thay thế C1. Edge C1 lỗi 44/90 PUT với HTTP 503 `clock_uncertainty_exceeded`; Edge C4, R1/R2 native và các Cold/clone workloads còn thiếu bằng chứng. Không dùng run lỗi làm PASS hoặc giả lập lại số đo đã xuất bản.

## Quy tắc verdict

- **PASS** chỉ cho *metric và workload scope đã xác định* khi full run/config, raw samples, inventory và baseline matched, sample count, đủ repetitions, checksum verified, cost window và pipeline async rõ ràng; các threshold được nhóm phê duyệt.
- **FAIL** khi sample hợp lệ nhưng dưới ngưỡng đã duyệt, hoặc integrity có lỗi; nêu value, group và nguyên nhân đã biết/chưa biết, không che workload kém.
- **INCOMPLETE / BLOCKED** khi thiếu data, runner, sample coverage, cache reset, Cloud/Edge topology, R1/R2/Cold proof, rate hoặc phê duyệt AWS. Không chuyển INCOMPLETE thành PASS, không gán số 0 cho unknown.

Mọi kết quả cần provenance của source/SDK/dataset/manifest/toolchain, client/Region/topology/encryption, policy/domain/cache, operation order/concurrency/elapsed, before/after metrics từng node, raw errors, async debt, current pricing và cleanup. Nhóm phải review/công bố *cả workload không tiết kiệm* trước khi đóng ngưỡng cuối. Không mở feature P2 để đạt performance.
