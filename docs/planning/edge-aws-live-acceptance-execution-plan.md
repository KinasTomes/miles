# Miles — Edge / recovery / alarms AWS live acceptance execution plan

**Status:** DRAFT / NOT APPROVED / DO NOT EXECUTE

**Prepared:** 2026-11-10
**Scope:** kế hoạch triển khai theo [runbook](edge-acceptance-aws-run-plan-draft.md) và [approval package](edge-aws-approval-package.md). File này là checklist điều phối, **không phải** bằng chứng đã chạy AWS hoặc giấy phép tạo tài nguyên có phí.

## 1. Quyết định hiện tại và phạm vi

- **Mục tiêu:** nghiệm thu Edge C1 → C4; native recovery R1/R2; 8 CloudWatch shallow alarms; teardown và native absence verification của toàn bộ tài nguyên thuộc ownership.
- **Còn mở:** nguyên nhân của `clock_uncertainty_exceeded` ở phiên trước chưa được chứng minh; Gate D3/D4 chưa có native live evidence; kết quả absence lịch sử không được thay cho kiểm chứng lần mới.
- **Điểm chặn hiện tại:** gói code/diagnostics phải được reviewer độc lập chấp nhận và pin vào baseline; owner chưa ban hành **Decision A** (read-only Gate C) hoặc **Decision B** (paid live run). Không được dùng quyền/phê duyệt/budget/release artifact của phiên cũ.
- **Không bao gồm:** chạy lại Cloud C1/C4 nếu impact analysis không yêu cầu; thay ngưỡng fail-closed clock/R1; tối ưu Chrony không có RCA; mở rộng thành benchmark S3 Glacier độc lập.
- **Người chịu trách nhiệm (chưa chỉ định):** owner phê duyệt, operator triển khai, reviewer kế hoạch/teardown, người dự phòng teardown. Ghi tên/định danh và chữ ký trong hồ sơ **private** theo từng run; không đưa vào Git.

## 2. Chuỗi gate, việc cần làm và bằng chứng

| Gate / thứ tự | Checklist hành động | Điều kiện chuyển gate / bằng chứng | Quyền thực hiện |
|---|---|---|---|
| **P0 — Offline readiness** | [ ] Chốt code baseline đã review/merge; recheck diagnostics/verifier, chạy `make check`, `make test-local`, Python regressions, Terraform tests, format/whitespace; xác nhận runbook nhất quán. [ ] Build linux/arm64, pin binary SHA-256. [ ] Tạo lại synthetic `backup30` (90 object × khoảng 8 MiB), pin manifest hash. | Local test report, release provenance, source/binary/dataset checksums, reviewer verdict. CI xanh không tương đương AWS PASS. | Offline, **không gọi AWS** |
| **A — Read-only authorization** | [ ] Owner ký riêng phạm vi account/Region, scoped non-root role, IAM boundary, operator, mục đích và expiry UTC; quyền chỉ cho inventory/preflight/saved plans. | Hồ sơ Decision A có `authorized=true`, còn hạn và nêu rõ *no apply*. Nếu thiếu → **NO-GO**. | Owner quyết định; chưa có |
| **C — Read-only preflight** | [ ] Xác minh STS caller identity, role session/MFA/expiry đúng phạm vi. [ ] Chụp fresh owned inventory và exclusions (bootstrap/state, shared IAM, KMS/Secrets pending). [ ] Kiểm tra quotas, network, Chrony/SSM readiness, current pricing. [ ] Tạo/review Terraform saved plans mới, destroy route, tags, IAM, endpoints, ALB, DDB/PITR, 8 alarms; freeze plan hashes. | `gate-c-preflight-summary.json`, inventory & plan checksum, resource/cost worksheet, operator/reviewer GO. Không tái sử dụng kế hoạch saved cũ. | Chỉ sau Decision A |
| **B — Paid-run authorization** | [ ] Owner phê duyệt riêng saved-plan hashes, source/release/dataset hashes, max resource counts, cost cap USD, stop-new-work threshold, start/stop UTC, latest workload start và absolute teardown deadline; chỉ định cleanup + fallback. [ ] Chọn rõ `diagnostic-only` hoặc `full-acceptance`. | Decision B ký riêng, đúng scope và còn hạn; mọi mục TBD hoàn tất; không coi billing alert là hard cost cap. | Owner quyết định; chưa có |
| **D0 — Provision & telemetry readiness** | [ ] Chỉ apply reviewed plan trong approved scope. [ ] Verify tối đa số node/resource đã duyệt (đề xuất: 2 Cloud, 1 Edge, 1 client; 0 NAT), encrypted EBS, private ALB/SSM, DDB/PITR, synthetic fixtures. [ ] Chạy Chrony preflight và host recorder **trước** workload; xác nhận UTC/monotonic timestamps, sampling, dropped-record counters và quyền file 0600. | `gate-d0-readiness.json`, native service/host state, collector coverage. Không có clock evidence đáng tin → **NO-GO** cho kết luận Edge RCA. | Chỉ sau Decision B |
| **D2-A — Edge C1** | [ ] Chạy `warmbench --fail-fast --concurrency=1 --read-repeats=3` với pinned fixture/binary. [ ] Correlate client, gateway và host clock/CPU evidence. | **90/90 PUT + 90/90 HEAD + 360/360 SHA-256 GET**, không 500/503/checksum/clock failure/indeterminate write; `edge-c1-result.json`. | Chỉ sau D0 GO |
| **D2-B — Edge C4** | [ ] Chỉ chạy khi Edge C1 PASS; cùng manifest, source, config và fail-closed policy; đổi concurrency thành `4`. | Cùng acceptance matrix **90/90 + 90/90 + 360/360**; `edge-c4-result.json`. | Chỉ sau C1 PASS |
| **D3 — Native recovery** | [ ] Fixture cô lập: R1 lease/fencing/closed-writer; R2 outbox export/seal/cut, checkpoint/GC, native DDB/PITR và throttle evidence. | `gate-d3-native-evidence-summary.json` với state transition, request/response và timestamp hợp lệ; DDB `ACTIVE` riêng lẻ không đủ. | Chỉ sau D2 PASS theo runbook |
| **D4 — Alarm transitions** | [ ] Xác minh 8 alarms: Cloud/Edge × `recovery_cut_age`, `integrity`, `throttling`, `telemetry_age`. [ ] Dùng tín hiệu/threshold thật và fixture an toàn; chờ đủ evaluation periods để quan sát hồi phục. | 8 native histories `OK → ALARM → OK` kèm dimension, period, threshold và metric provenance; `gate-d4-alarm-evidence-summary.json`. Không dùng `SetAlarmState` hoặc fake healthy-zero. | Chỉ sau D3 PASS theo runbook |
| **E — Mandatory teardown** | [ ] Dừng dispatch mới, drain/reconcile `indeterminate` writes; giữ evidence riêng trước destroy. [ ] Freeze authoritative **pre-teardown manifest** từ live Terraform state/outputs + SHA-256 trước destroy. [ ] Destroy **chỉ owned resources** theo reviewed plan; giữ bootstrap/state và shared resources theo exclusion. [ ] Chạy native absence verifier live với frozen manifest và đối soát counts/24 types gồm NAT/EIP. | `gate-e-absence-summary.json`, destroy proof, pending deletion & exceptions, cost ledger. `UNVERIFIED`/resource leak → **OPEN / ATTENTION REQUIRED**, không tuyên bố State 0. | Bắt buộc kể cả khi D0/D2/D3/D4 FAIL |
| **Closeout** | [ ] Báo `PASS / FAIL / DEFERRED / UNVERIFIED` riêng cho từng gate; RCA `confirmed/correlated/hypothesis/unverified`. [ ] Reconcile estimated → observed → later billed costs; owner acknowledge. | Báo cáo redacted và private evidence references; chỉ đóng công việc khi acceptance evidence đầy đủ. | Sau E, không mặc định Done khi merge |

## 3. Hard stops và phương án thoát

1. **Trước A/B:** thiếu chữ ký hoặc giới hạn hành động → không gọi AWS / không tạo billable resources. Không dùng root, không reuse credentials và saved plans cũ.
2. **Trong C/D0:** sai role/account/Region, plan khác SHA, thiếu permission/quota/clock source, thiếu collector hoặc bằng chứng bảo mật → **NO-GO**; nếu đã provision thì bắt đầu E.
3. **Trong D2:** HTTP 500/503, clock uncertainty, checksum/size mismatch, lease failure → ngừng **dispatch mới**, không hủy bừa in-flight PUT; drain trong giới hạn, đánh dấu/reconcile unknown commit, giữ logs rồi E. Không âm thầm chạy C4/D3/D4.
4. **Trong D3/D4:** native state không khớp, alarm không đủ history hoặc test có nguy cơ phá dữ liệu → dừng, lưu bằng chứng, E; đánh dấu `FAIL/DEFERRED` không bịa PASS.
5. **Chi phí/deadline:** đạt threshold stop-new-work hoặc latest trial start, hoặc không còn đủ thời gian dành cho teardown → dừng thử, E ngay. Monitoring/billing báo chậm không bảo đảm mức trần.
6. **Trong E:** manifest thiếu/không hợp lệ, duplicate IDs, unsupported/unchecked resources, NAT/EIP cardinality mismatch, AWS permission error, pending deletion ngoài hợp đồng, chi phí không rõ → `UNVERIFIED`; escalate tới owner và theo dõi bằng hồ sơ riêng.

## 4. Đầu vào chưa chốt (không điền giá trị giả)

- [ ] Review/merge baseline code + independent replay cho diagnostics/teardown verifier; ghi chính xác release source SHA và linux/arm64 binary SHA-256.
- [ ] Signed Decision A: operator, role, account/Region, thời hạn và read-only permissions.
- [ ] Fresh Terraform inventory, saved plan hashes, owned/excluded resources, ước tính chi phí từng service và khoản dự phòng.
- [ ] Signed Decision B: approved USD cap, stop threshold, giờ bắt đầu/dừng UTC, teardown deadline, người thay thế, chế độ `diagnostic-only` hoặc `full-acceptance`.
- [ ] Dataset manifest hash; Chrony source/host collector verification; cô lập test fixture; retention/permissions cho raw evidence.
- [ ] Post-run reconciliation của actual resource state và billed charges (hậu kiểm theo khi billing cập nhật).

**Current decision: NO-GO.** Việc tạo checklist này không thay đổi trạng thái approval hoặc cấp quyền AWS.

## 4.1. Kiểm tra offline ngày 2026-11-10

- Branch PR #76 tại `05d6fa78559437520ec325f327e3f7bc14f67a1f`: `make check`, `make test-local`, benchmark race tests, Terraform format và Terraform mock tests (43/43) đều PASS. Các fixture local đã được dọn; đây không phải bằng chứng AWS live.
- Baseline trong approval package `1dcc352` đã cũ: `origin/main` đã có thêm `a74f144` (PR #78), thay đổi runtime đọc object. Phải chọn lại baseline và chạy lại các kiểm tra trên source cuối cùng; không coi binary build thử là release đã pin.
- PR #76 vẫn draft; review độc lập gần nhất kiểm tra `23edd24`, chưa bao phủ head `05d6fa7`. Chưa thể coi verifier và telemetry đã được reviewer độc lập chấp nhận.
- Manifest teardown và các file tổng hợp Gate D0/D3/D4 trong kế hoạch là đầu ra dự kiến; cần chuẩn bị và kiểm chứng cách tạo từ nguồn thực trước live run. Không suy ra có sẵn automation chỉ từ tên file.
- Dataset benchmark sẽ tạo trên máy AWS dành riêng cho lượt thử theo yêu cầu vận hành; chưa tạo dataset trên Oracle. Chưa có plan AWS mới, dự toán, cửa sổ chạy hay bằng chứng live.
- Đã merge `origin/main` `4b8edc4` vào branch chuẩn bị (`163f20b`); release cuối vẫn chưa pin và cần reviewer độc lập kiểm tra các sửa đổi telemetry/teardown. Trên source này `make check`, `make test-local`, Terraform mock tests (43/43), benchmark race tests và format đều PASS.

## 5. Hướng dẫn lưu evidence và liên kết

- File này và tài liệu trong repository chỉ ghi **kế hoạch**, tổng hợp đã khử nhạy cảm và tham chiếu tương đối. Không commit raw AWS identifiers, credentials, Terraform state/plan chứa secrets, run-specific signed authorization, hoặc request IDs.
- Run evidence lưu ở thư mục private theo từng session với quyền `0600`, đường dẫn và hashes kiểm soát bởi operator. Ledger tách `dry-run` và `live`; `dry-run PASS` không dùng để chứng nhận live.
- Đối chiếu chi tiết với [runbook](edge-acceptance-aws-run-plan-draft.md), [approval package](edge-aws-approval-package.md), [Edge clock RCA](../testing/edge-clock-rca-followup.md) và [live report lịch sử](../testing/live-acceptance-20261108-report.md). Nếu có sai khác, trình reviewer/owner chốt trước khi thực thi.
