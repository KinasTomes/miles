# Ma trận workload cho benchmark chính thức

Đây là **bản cấu hình đề xuất để nhóm review**, không phải kết quả benchmark hay sự phê duyệt chi phí AWS. Ma trận máy đọc được nằm tại [workload-matrix.json](workload-matrix.json), kiểm tra offline bằng `python3 -B scripts/validate_benchmark_matrix.py benchmarks/workload-matrix.json`. Các ca `requires-runner` **chưa có runner đo benchmark hoàn chỉnh**; test chức năng của service không thay thế kết quả thời gian/chi phí của ca đó.

## Phương pháp so sánh và run config

- Bắt đầu từ [baseline](baseline.md), dùng `backup30` (30 version mỗi nhóm low-entropy/compressed-identical/random-distinct), manifest SHA-256 và full-file SHA-256. `smoke` chỉ kiểm protocol. Không tạo dataset lớn trên operator machine.
- Mỗi cặp đối chứng dùng client, manifest, thứ tự thao tác, Region, cấu hình checksum/SDK, SSE-KMS, class, concurrency, vị trí mạng, thời gian và cửa sổ accounting tương đương; pin binary hash, Go/SDK, release và policy revision thực tế. Direct S3 là baseline tổng, chunk-per-object **chỉ** đối chứng PUT reduction.
- Warm đo C1 (single-PUT, **không multipart**) và C4 riêng cho Cloud, Edge; `read_repeats=3` là 1 GET verify sau write + 3 GET lặp. Không coi chúng là cache cold/warm nếu thiếu purge và hit/miss counters. Mỗi cấu hình đề xuất 3 trial **độc lập**; số lượt thực tế có thể ít hơn khi bị stop-the-line, phải ghi thiếu coverage.
- Trial reset bằng fresh backend/domain có kiểm chứng hoặc công bố rõ retained roots, existing pack/cache, policy và physical inventory; prefix/bucket mới không phải domain reset. Giữ snapshot trước/sau, metrics **từng node** (không scrape ALB thay cả fleet), R1/R2 backlog, outbox/seal/checkpoint/GC và cửa sổ charging sau foreground.
- Cùng file payload nhưng mapping owner/bucket/domain nằm trong config **private**, không đưa user, bucket, endpoint, token, account hoặc hostname vào artifact public. Edge trên EC2 xác minh Edge profile, **không đại diện đường mạng on-prem**.
- Khi một trial lỗi, ghi cả failed/partial/cancelled sample, request correlation đã redacted, hậu quả commit còn chưa rõ và ngừng trial kế tiếp theo stop-the-line. Không retry PUT unknown-commit tùy tiện, không tính benchmark PASS khi thiếu GET hash/R1/R2/cost window.

## Lệnh Warm pilot chuẩn (chỉ chạy trên host test riêng sau approval)

Tạo các file 0600 theo [warm harness](warm-harness.md) với `target=direct-s3` hoặc `target=miles-warm`, cùng manifest, toolchain, client và cấu hình encryption thực tế. Ví dụ một **cặp Cloud C1** (các tên đường dẫn chỉ là placeholder private, không phải bằng chứng đã có dữ liệu):

```bash
./bin/warmbench --config /private/pilot/direct-cloud.json --manifest /private/pilot/dataset/manifest.json --out /private/pilot/trial-1-direct-cloud-c1 --concurrency 1 --read-repeats 3 --retain-objects --fail-fast
./bin/warmbench --config /private/pilot/cloud.json --manifest /private/pilot/dataset/manifest.json --out /private/pilot/trial-1-miles-cloud-c1 --concurrency 1 --read-repeats 3 --retain-objects --fail-fast
```

Các ca C4 đổi `--concurrency 4` ở **cả hai**; Edge dùng client/route và config được pin riêng, không mượn direct baseline Cloud nếu khác topology. Mỗi trial phải có output directory mới, fresh/isolation verification và cleanup riêng. `--retain-objects` giữ keys để đọc physical stats sau thời gian collector làm mới; cần teardown được duyệt, không để lại resource vĩnh viễn. Tham số trên không áp dụng cho multipart/restore/clone đang chờ runner riêng.

## Coverage và chủ sở hữu phép đo

| Nhóm | Scenario trong ma trận | Cần quan sát | Trạng thái thực thi |
|---|---|---|---|
| WL-01/02 | Cloud/Edge × C1/C4 | PUT/s, p95/N, full GET SHA, cache hit/miss, before/after snapshots, direct S3 matched | `warmbench` có luồng Warm; **chưa chứng minh cold cache** |
| WL-03 | Overwrite Cloud | head revision, quota live vs historical, GET SHA, retained bytes và retries | `requires-runner` |
| WL-04 | Default/shared vs separated domains | owner/domain isolation, unique raw/stored, physical allocation, cross-owner auth | `requires-runner`; smoke service/artifacts khác benchmark |
| WL-05 | Threshold 8MiB−1 / 8MiB / 8MiB+1 | operation dispatch, multipart part count, full checksum, part size, PUT controls | `requires-runner`; không mở UploadPartCopy |
| WL-06 | IR và GLACIER Flexible riêng | thực tế transition, restore, expiry, `Restored` deadline, revive, retention và chi phí | `requires-runner`; Clock/simulator không thay AWS restore |
| WL-06 Edge | Cross-domain clone-download và clone-upload | separate source/destination owner/domain, verified GET/download và PUT/upload, cold/restore state, hướng mạng S3→Edge và Edge→S3 | `requires-runner`; không được coi là same-domain metadata CopyObject |
| Reliability | R1/R2 Cloud/Edge | lease/fence, epoch cut/seal, checkpoint/GC, orphan/retained storage, PITR và errors | `requires-runner`; không tự thay acceptance recovery |

**Clone-download/upload Edge:** Với hai miền thật sự độc lập, snapshot chính xác manifest + hash; clone-download lấy từng file qua GET từ domain nguồn xuống Edge (đo S3→gateway→client và cache misses/hits); clone-upload PUT byte đã verify từ Edge sang domain đích, sau đó GET và kiểm SHA-256, đo Edge→gateway→S3 và physical destination. Đối chứng direct S3 phải dùng lịch GET/PUT tương đương, không gộp thành CopyObject server-side. So sánh thêm cùng-domain server-side CopyObject trong kết quả *khác loại phép thử*, không dùng p95 của CopyObject để kết luận client clone nhanh/chậm hơn. Không dùng `aws s3 cp` high-level nếu tự dispatch P2 GetObjectTagging/UploadPartCopy; pin S3 API primitive được phép và ghi dispatch thật.

**Accounting:** Phân biệt logical payload với HTTP body, wire bytes và billed transfer; S3 PUT/COPY/MP/GET/restore/transition/DELETE, DDB consumed/billed, ALB/NAT/endpoints, Edge VM/disk, PITR, KMS, overhead/dead/retained versions và asynchronous writes. Bất kỳ rate/route/bill thiếu phải hiện rõ `unknown`, không biến thành chi phí 0 hay tuyên bố tiết kiệm. Mọi ca benchmark thật cần run record theo [baseline §6](baseline.md) và ngưỡng theo [thresholds](thresholds.md).

## Gap và trình tự đóng

1. **Có thể làm offline ngay:** xác minh manifest, matrix/validator, review fairness, negative tests, source refs và reconciliation pilot đã công bố.
2. **Cần implementation độc lập:** runner có reset/purge được chứng minh cho cache; overwrite, multipart boundaries, default/separated domains, restore và Edge clone, có capture các request/cost route tương ứng. Không tuyên bố những thao tác này chạy được chỉ từ `warmbench`.
3. **Cần approval AWS riêng:** tạo môi trường, chạy pilot và thu thập immutable raw runs, budget/stop/teardown; không tái sử dụng credentials hoặc approval cũ.
4. **Freeze sau pilot:** nhóm review matrix, sample duration/repetitions, ngưỡng [thresholds](thresholds.md) và cả workload không tiết kiệm. Benchmark chính thức chỉ bắt đầu khi runner/contract và environment gate đáp ứng.

Các phép đo từng có được mô tả tại [live acceptance report](../docs/testing/live-acceptance-20261108-report.md) và [pilot validation](../docs/testing/dynamodb-pilot-validation.md). Chúng thuộc đúng commit, client, sample và topology khi đó, **không** là bằng chứng mọi hàng trong ma trận đã hoàn thành.
