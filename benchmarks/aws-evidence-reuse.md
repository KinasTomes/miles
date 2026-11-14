# Tái sử dụng bằng chứng AWS lịch sử cho benchmark

**Trạng thái: đã đối chiếu 16 `warmbench result.json` lịch sử và telemetry summary hiện có qua bộ tổng hợp read-only; nguồn AWS/Chrony độc lập vẫn giữ verdict gốc.** Các lần chạy này là *observed AWS evidence* cho workload cụ thể và release cụ thể, **không** tự trở thành kết quả benchmark chính thức. Không chạy AWS mới để lập inventory này. Nếu muốn kết luận performance, phải đối chiếu artifact gốc với checksum/provenance, thời gian đo, config, isolation và comparator thực tế theo [ma trận](benchmark-matrix.md) và [ngưỡng](thresholds.md).

## Kết quả raw reconciliation (offline, 12/11/2026)

Bộ chọn 16 file kết quả có tên rõ ràng ở [aws-history-selection.json](evidence/aws-history-selection.json). Bộ tổng hợp [reuse_aws_evidence.py](../scripts/reuse_aws_evidence.py) chỉ đọc file private tại vị trí operator chỉ định, đối chiếu manifest 90 file, tổng bytes, từng mẫu PUT/HEAD/GET, các trường SHA-256, trạng thái fail/skip, thời gian, hash binary/release và summary clock (nếu có). Chỉ những trường an toàn được xuất ra [sanitized evidence](evidence/aws-historical-edge-summary.json) cùng `artifact_sha256` của raw input.

| Nhóm lịch sử | Tổng trial | Warm PUT/HEAD/4×GET verified | Failed/incomplete | Tác dụng |
|---|---:|---:|---:|---|
| PR #76 — 3 phiên Edge ngày 11/11 | 12 | 8 | 4 | Edge Warm C1/C4, clock, fail-fast/cancellation và các regression khác release |
| PR #86 — focused recheck 12/11 | 4 | 3 | 1 | Candidate Edge C1/C4 một chu kỳ, baseline C1 clock chưa đủ, extra C1 bị dừng |
| **Tổng** | **16** | **11** | **5** | **Raw-backed client workload evidence, không phải benchmark chính thức** |

Kết quả minh họa từ **raw per-phase**: PR #86 final candidate Edge C1 PUT **4,392 MB/s**, Edge C4 PUT **5,443 MB/s** (decimal MB/s, không phải network wire). Từng phép đo giữ elapsed, N, p95 và hash nguồn riêng trong JSON. Không tính ratio BEN-03 vì thiếu đối chứng direct-S3 ghép cùng trial/source. `warm_workload_verified=true` chỉ nghĩa là **client PUT/HEAD/GET đã hoàn tất và mẫu SHA khớp manifest**; không chứng minh domain reset, cache/cost accounting, R1/R2/Cold hoặc không lỗi ở phiên khác.

**Phân biệt clock:** Report gốc vẫn báo một số trial `OK` nhưng `window_end_utc` của telemetry summary sớm hơn `finished_at` trong `result.json` (ví dụ một số run #86); raw summary lưu đồng thời `reported_verified` và `window_encloses_run=false`. Không tự nâng thành **clock verified bởi bộ đối chiếu mới**; cần đối chiếu chứng từ đồng bộ/đầu-cuối recorder của phiên nếu dùng clock làm acceptance gate. Các trial có summary `UNVERIFIED` vẫn giữ nguyên.

Cách tái tạo trên **operator host được phép đọc**, không AWS API:

```bash
python3 -B scripts/reuse_aws_evidence.py \
  --selection benchmarks/evidence/aws-history-selection.json \
  --root pr76-early=/private/owned/pr76-first-session \
  --root pr76-mid=/private/owned/pr76-second-session \
  --root pr76-late=/private/owned/pr76-third-session \
  --root pr86=/private/owned/pr86-session \
  --out /private/owned/new-sanitary-summary.json
```

Các đường dẫn là placeholder do operator tự thay, không phải thư mục thực được đăng công khai. Output phải chưa tồn tại, được tạo quyền 0600. **Không** chạy lệnh này trên AWS host để tạo lại tài nguyên hoặc phục hồi credentials. Không commit raw result, per-file names/hashes, credentials, Terraform state hay node telemetry. Kiểm tra `artifact_sha256` khi đối soát với bản raw được bảo quản; SHA nguồn không tự chứng minh attestation AWS độc lập. Tham chiếu sample giữa raw dataset manifest và object logic đã kiểm; file payload gốc không được regenerate/re-hash trong bước import.

## Nguồn và khả năng dùng lại

| Nguồn và thời điểm UTC | Runtime / trial | Quan sát có thể dùng lại | Không được kết luận |
|---|---|---|---|
| [AWS Edge recheck 11/11, phiên đầu](../docs/testing/edge-private-aws-recheck.md#results-collected), PR [#76](https://github.com/KinasTomes/miles/pull/76) | Edge `328b700`, C1 và C4 replay, mỗi ca 90/90 PUT, 90/90 HEAD, 360/360 GET SHA-256, clock verified; `backup30` 754,994,670 B, manifest SHA-256 được công bố | Edge Warm protocol + integrity trên đúng release/cấu hình, C1/C4 đơn lẻ, verified clock windows | Chưa có cặp direct S3 cùng cửa sổ, 3 độc lập/lựa chọn, cache cold/warm chứng minh, hay tất cả workload W7 |
| [AWS Edge recheck 11/11, phiên tiếp](../docs/testing/edge-private-aws-recheck.md#latest-run-recovery-alarms-and-shutdown), PR [#76](https://github.com/KinasTomes/miles/pull/76) | Edge `1ad93fe` C1 90/90/360 PASS; C4 fail, `0ee6d2f` C1 GET stream fail; epoch 0 sealed 934 events, độc lập VerifyCut; 8 alarm transitions PASS | C1 verified window, âm tính clock/streaming, seal/cut và alarm đúng phiên; không xóa failed trials | Không coi C4 fail là PASS, không suy từ 8 alarm sang benchmark cost, không thay full R1/R2/GC acceptance |
| [AWS clock/recovery 11/11](../docs/testing/aws-clock-recovery-validation.md), PR [#76](https://github.com/KinasTomes/miles/pull/76) | Edge two-attempt burst trên `ad31768`: C1/C4/C1 mỗi ca 90/90/360, strict clock OK; C4 thứ hai fail setup; Edge `783ace0` C1 chỉ 320 GET thành công, 1 GET fail; final four-attempt config chưa chạy | Bằng chứng functional C1/C4 và clock theo **two-attempt**, lỗi CreateBucket/Get và Outbox throttles 518 events; hiện tượng shutdown timeout | Không tính những trial này làm vòng final four-attempt, không gộp C1 các commit/profile thành 3 repeats |
| [AWS focused recheck 12/11](https://github.com/KinasTomes/miles/blob/a0a61b59959b95b532ba8190e69031eceb364ae5/docs/testing/focused-clock-shutdown-aws-validation.md), PR [#86](https://github.com/KinasTomes/miles/pull/86) | Edge `376d130`, four-attempt burst C1 và C4 đều 90/90/360, strict clock OK; baseline C1 90/90/360 nhưng clock UNVERIFIED; cycle thứ hai bị dừng sau 89 PUT; shutdown Edge vượt 8 phút | Một cặp C1/C4 cùng final runtime/profile, SHA-256 toàn phần, 225 throttle responses trong native Outbox pulse, historical sealed epoch/S3 independent cut và unsuccessful shutdown | Không có hai cycle đầy đủ; peer export sau lỗi không chứng minh Edge shutdown PASS; không thay matched direct S3 hay phép đo Cloud workload |
| [AWS Cloud/direct 08/11](../docs/testing/live-acceptance-20261108-report.md), PR [#71](https://github.com/KinasTomes/miles/pull/71) | Cloud C1 PUT 4,56 MB/s so direct C1 78,22 MB/s (~5,83%); dedup 1,94×; Cloud C4/Direct C4 có kết quả đo | Cặp performance **lịch sử theo release và dataset được báo cáo**, cảnh báo BEN-01/BEN-03 dưới target | Không chuyển kết quả Cloud cũ thành threshold của Edge source mới; không coi một trial là full benchmark |

**C1/C4 = concurrency của client**, không phải hai Cloud node. Trong các báo cáo #76/#86, hai Cloud node hiện diện và được giám sát, nhưng các bảng C1/C4 nêu trên là **Edge workload**. Không đánh dấu các hàng `warm-cloud-c1/c4` trong [matrix](workload-matrix.json) là đã được benchmark chỉ vì Cloud khỏe hoặc được dùng cho control-plane.

## Coverage có thể thừa hưởng và gap còn lại

| Phần benchmark | Có thể trích từ report | Còn thiếu trước khi kết luận |
|---|---|---|
| Warm Edge C1/C4, SHA-256 | C1/C4 đạt 90 PUT, 90 HEAD, 360 GET trong các phiên liệt kê; thất bại cũng được ghi | Pin raw `result.json`, đúng toolchain, matching direct S3, isolation, p95/s, phase timing và repetitions trên cùng final source |
| R1/R2, Cut/GC, alarms | Seal + S3 independent VerifyCut và 8 alarm cases từ một phiên #76; native R1/R2/GC fixtures có thực thi AWS backend nhưng dùng modeled Clock/in-process nodes | Chỉ kết luận theo fixture/epoch/phiên đã thực hiện; không thay full distributed recovery/cost window/Edge stressed shutdown; #86 vẫn lỗi flush |
| Dedup, nén, physical bytes, cache | Cloud snapshot lịch sử 08/11 có ratio; Edge C1/C4 GET SHA không đo cache hit tỷ lệ riêng | Per-domain full physical inventory, reset verified, cache hit/miss numerator+denominator, cùng cohort default/separated và read state |
| Multipart, overwrite, Cold IR/Flexible, clone Edge cross-domain | Các PR này không báo cáo một benchmark đối chứng đầy đủ cho các ca này | Runner, matched input, hashes, API dispatch, restore/revival, hướng network/cost riêng |
| BEN-03 throughput và BEN-04 PUT reduction | 08/11 có Cloud vs direct S3 được báo cáo; #76/#86 có số PUT/GET hoàn thành và clock | #76/#86 thiếu paired direct-S3 throughput cùng cấu hình/release; BEN-04 thiếu chunk-per-object comparator và complete async requests |
| Chi phí và savings | #76/#86 công bố **ước tính chi phí của toàn phiên AWS** (~4,74 USD và ~3,89 USD, trước thuế) | Không phải bill cuối, không phải chi phí theo workload hay matched direct-vs-Miles; thiếu billed inventory/accounting window/route cho net saving |

## Quy tắc nhập raw evidence nếu được cấp quyền đọc

1. **Chỉ đọc:** xác định run ID, UTC window, tested runtime SHA/binary SHA, Chrony profile hash, target Cloud/Edge/direct, manifest hash, class, concurrency và actual SDK/checksum settings. Không khôi phục token/session/ARN riêng, không copy raw artifacts vào repository.
2. Đối chiếu full `result.json` và native observation với báo cáo: `planned/dispatched/completed/skipped`, 90 PUT/HEAD và 360 verified GET cho mỗi trial thành công, explicit failed/partial, p95/N/elapsed, route counters, process freshness, all-node snapshots, cleanup. Không tự suy `read-repeats=3` là cache-hit 80%.
3. Gom theo **same binary + policy + Chrony profile + dataset + comparator topology + isolation + interval**. Không coi hai phiên khác release/profile, trial bị fail hoặc session-level cost là repetitions của cùng benchmark. Giữ `reported`, `raw-verified`, `matched`, `incomplete`, `failed` khác nhau.
4. Khi thiếu artifact, ghi `INCOMPLETE` và liệt kê chính xác số đo còn thiếu; không chạy AWS mới chỉ để lấp bảng. Việc dùng kết quả AWS lịch sử không cho phép tự phê duyệt threshold, phí AWS mới hoặc đóng task.

**Kết luận:** Đã có 16 raw client runs được đối soát ở mức manifest/sample; thừa hưởng được nhiều **bằng chứng Warm Edge, integrity, clock, recovery và lỗi thực**. Chưa đủ điều kiện chốt các ngưỡng tiết kiệm, throughput so direct S3, default-vs-separated domains và cross-domain/Cold. Mọi kết luận phải kèm đúng source/phiên và limitations.
