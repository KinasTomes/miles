# Miles — Edge clock & remaining AWS acceptance follow-up

**Trạng thái:** Kế hoạch triển khai, CHƯA phê duyệt AWS live run
**Workspace:** `/home/ubuntu/miles-worktrees/edge-acceptance-followup`
**Branch:** `feat/edge-acceptance-followup` — base `main` @ `7f6533e5640703b63b67f5d920f0fb510d1b953a` (squash merge PR #73)
**Ngày lập:** 2026-11-08 · **Account/Region dự kiến:** `499388132734` / `ap-southeast-1`
**Nguồn:** PR [#71](https://github.com/KinasTomes/miles/pull/71), [#73](https://github.com/KinasTomes/miles/pull/73); [live run report](../testing/live-acceptance-20261108-report.md); [run plan đã thực thi](aws-live-acceptance-run-plan.md); [acceptance follow-up lịch sử](aws-live-acceptance-follow-up.md).

## 1. Baseline và mục tiêu còn lại

Đã hoàn thành/được duyệt:
- #71: Phase A/B offline, Phase C preflight, Cloud C1/C4 và direct S3 thành công; Phase E teardown, 41/41 kiểm tra absence **trong phạm vi sở hữu, tại thời điểm phiên 2026-11-08**; không khẳng định inventory AWS hiện tại chỉ dựa vào báo cáo cũ.
- #73: P0 sửa timestamp từng node, tách SSM/SSH RTT khỏi host-`chronyc` elapsed, xác minh schema/returncode/UTC provenance, fail-closed khi evidence không đáng tin; 86 Python tests và kiểm thử offline PASS.
- `W05-HUNG-03 = Done` theo quyết định owner. **Không đồng nghĩa Phase D đã nghiệm thu toàn bộ.**

Chưa hoàn thành:
- Edge C1: 46/90 PUT thành công, 44/90 HTTP 503 `clock_uncertainty_exceeded`. Edge C4 chưa chạy theo stop-the-line.
- Nguyên nhân gốc Edge: *chưa chứng minh*. CPU credits/scheduling/NTP polling là các giả thuyết cần evidence; snapshot ngày 08/11 có tuổi mẫu âm vì collector cũ, không phải chứng cứ về NTP age thực tại lúc PUT #47.
- Phase D3: R1 lease/fencing; R2 outbox export/seal/cut; checkpoint/GC native AWS **UNVERIFIED / DEFERRED**. DDB tables ACTIVE, PITR enabled, physical stats đã PASS ở phiên trước.
- D4: 8 CloudWatch shallow alarms (4/profile × Cloud/Edge), `OK → ALARM → OK` bằng ngưỡng thật **CHƯA CHẠY**.

**Goal:** làm phép đo/runner đúng và có bằng chứng trước, sau đó nghiệm thu phần còn thiếu bằng một phiên AWS đã được phê duyệt riêng. Không chạy lại Cloud C1/C4 nếu thay đổi chỉ thuộc Edge/test harness và không tác động tới Cloud; cần đánh giá impact trước khi quyết định.

## 2. Quy tắc chung / hard stops

1. **OFFLINE cho tới khi owner duyệt kế hoạch AWS riêng.** Không gọi AWS API (kể cả read-only inventory, nếu chưa được phép), không Terraform apply/destroy, không lấy lại credentials cũ, không dùng root. Ngân sách $10/3h ở run plan cũ **không tự động được tái duyệt**.
2. Giữ nguyên `internal/coordination/chrony.go`, policy fail-closed R1, `minpoll 4 maxpoll 4` và `makestep` hiện trạng. Không dùng retry/nới ngưỡng để che 503; chỉ cân nhắc chỉnh Chrony sau khi có RCA và review độc lập.
3. Không đưa raw request IDs, object paths, AWS ARNs nhạy cảm, token, Terraform state, SSH/private logs hay file `~/.local/state/miles/dynamodb-pilot/` vào Git. Chỉ lưu sanitized/pseudonymous artifacts và đường dẫn tương đối.
4. Stop-the-line: dừng **dispatch mới** ngay khi nhận sample lỗi; không tự ý hủy in-flight PUT vì có thể *unknown commit*. Chờ các in-flight hoàn tất trong deadline cho phép, ghi toàn bộ outcomes, nếu chưa chắc chắn phải đánh dấu `indeterminate` và reconcile durable state trước khi retry/xóa.
5. Gate thất bại → ngừng các trial tiếp theo, persist failed-window evidence rồi drain/teardown theo phiên đã duyệt, kể cả khi cần RCA sau đó. Không tự giữ hạ tầng qua đêm.
6. CI xanh không phải bằng chứng AWS PASS; estimate chi phí không phải billed cost; absence 41/41 là scope-bound, pending deletion cần ghi rõ.

## 3. Workstream F — warmbench per-request fail-fast (P1 code, offline trước)

**Vị trí:** `internal/benchmark/run.go` (hiện `filePhase` phát tất cả jobs và chỉ `finishPhase` mới báo fail), `cmd/warmbench/main.go`, unit/integration tests.

- [x] F1. Định nghĩa option CLI `--fail-fast`, default `false` để không thay đổi baseline hiện hành; thêm `FailFast bool` vào `benchmark.Options` và test CLI/config validation.
- [x] F2. Bounded worker/dispatcher: khi một worker hoàn tất sample lỗi (`Error != ""`, gồm HTTP 500/503, checksum/size mismatch), latch stop reason, ngừng enqueue **các job chưa dispatch**; không cancel các request đang in-flight bằng context chung.
- [x] F3. Worker đã nhận job trước stop tiếp tục hoàn tất/timeout độc lập; `WaitGroup` kết thúc trước save artifact. Không để race, deadlock (đặc biệt C1/C4, channel blocked, failure at first/last job, producer exits), không ghi vào output khi worker còn mutate.
- [x] F4. Result schema: nêu rõ `planned/dispatched/completed/skipped`, `stop_trigger` (sanitized), phase/trial status và preserved outcome của mọi dispatched request; skipped ≠ failed; giữ phase statistics nhất quán, bảo toàn `result.json` atomic mode 0600. Nếu cần đổi schema, version và cập nhật toàn bộ consumers/tests.
- [x] F5. Khi PUT không có kết quả rõ do timeout/transport, gắn `indeterminate` / needs-reconciliation, không retry bừa hay coi là clean failure. Bảo toàn owned prefix để kiểm tra; tránh auto-delete khi chưa đối soát.
- [x] F6. Regression: deterministic mock backend C1/C4/C8; first/middle/last failure; success đang in-flight sau stop; timeout/SIGINT; GET mismatch; no extra dispatch; no hangs; race detector; default `--fail-fast=false` giữ behavior cũ; sanitation trong evidence. Tích hợp với `execute_workload_and_capture_on_failure`.
- [x] F7. Chạy `make check`, `make test-local`, Go race của `internal/benchmark` và `cmd/warmbench`, smoke + `git diff --check`; ghi acceptance matrix thực tế vào PR.

**Gate F:** Error được quan sát → không dispatch job mới; các job đã dispatch có kết cục được lưu (hoặc `indeterminate` minh bạch); không unknown commit bị che; artifacts hợp lệ cả PASS và FAIL; mode mặc định không regression. Code review/CI PASS trước khi merge.

## 4. Workstream R — Edge clock RCA & offline correlation (P0/P1)

**Vị trí dự kiến:** `scripts/` module phân tích offline (`scripts/edge_clock_rca.py`) + `scripts/tests/test_edge_clock_rca.py`; báo cáo `docs/testing/edge-clock-rca-followup.md` (chỉ sanitized evidence). Sử dụng PR #73 telemetry contract, không sửa R1 runtime.

- [x] R1. Inventory dữ liệu sẵn có **offline** từ phiên 2026-11-08: `edge-c1-t1-result.json`, `edge-c1-t1-failed-window.json`, gateway journal, chrony tracking/sources/sourcestats, OS telemetry, CPU metrics nếu có. Bảng `observed / correlated / hypothesized / missing`; tuyệt đối không suy đoán metrics không tồn tại.
- [x] R2. Map stable pseudonymous request IDs → UTC timestamps / stage `writer_admission` / HTTP 503; kiểm tra 46 successes + 44 failures. Chuẩn hóa timestamp/provenance/timezone và uncertainty của từng nguồn. **Không tái dùng tuổi mẫu âm từ collector cũ để xác nhận NTP age.**
- [x] R3. Script đối chiếu timeline có kiểm tra schema, ordering, missing-data và granularity: error onset, host `chronyc` sample age, poll interval, last reference UTC, stratum/leap/offset/drift, CPU utilization & credits (`CPUCreditBalance` / `CPUCreditUsage` khi có), CPU steal/run queue, memory pressure và SSH/SSM RTT. Độ phân giải CloudWatch 1 phút không chứng minh jitter ở mức một request; không dựng causal claim từ tương quan thô.
- [x] R4. Lập bảng phân biệt tối thiểu `sample_age_exceeded`, `chrony_command_duration_exceeded_1s`, `horizon_uncertainty_exceeded_2s`, NTP unsync/leap/offset và `ErrClockUnavailable`. Log AWS gateway hiện chỉ có `clock_uncertainty_exceeded` chung nên nguyên nhân con chưa được xác nhận.
- [x] R5. Thiết kế safe diagnostic sampling cho phiên mới (nếu cần) để đọc source reach/poll/sourcestats, OS scheduler and credit metrics *before/during/after* Edge load, với artifact host timestamp và monotonic duration đúng. Không gọi AWS ở giai đoạn này. Chỉ thêm instrumentation nếu offline tests chứng minh không gây nhạy cảm/overhead cao.
- [x] R6. Deterministic simulation: missed NTP updates ở chu kỳ 16s, CPU scheduling delay, jitter, SSH/SSM slow RTT với remote chronyc nhanh, stale→fresh, partial telemetry, evidence capture failure. Verify fail-closed, không tự chỉnh NTP threshold.
- [x] R7. RCA output phân cấp: `Confirmed` (có native evidence), `Correlated`, `Hypothesis`, `Unverified`; kết luận không vượt quá bằng chứng. Báo cáo hoàn chỉnh: `docs/testing/edge-clock-rca-followup.md`.

**Gate R:** ĐẠT (PASS) — Có thể diễn giải đúng nguồn gốc/độ tin cậy của mỗi timestamp; không false-positive do remote transport hoặc frozen T0; không lộ private identifiers; biết chính xác còn thiếu bằng chứng gì trước live Edge C1. Safe diagnostic sampling đã được thiết kế sẵn sàng cho phiên AWS tiếp theo.

## 5. Workstream L — AWS follow-up acceptance (chỉ viết plan, CHƯA THỰC THI)

**Bản nháp riêng:** [edge-acceptance-aws-run-plan-draft.md](edge-acceptance-aws-run-plan-draft.md). Phải cập nhật release SHA, hashes, IAM, dataset, resource quota, spend và owner approval theo thời điểm thực thi. Không tái sử dụng old signed plan/approval/keys.

- [ ] L0 / Gate C: xác nhận đúng account `499388132734` + Region `ap-southeast-1` bằng scoped non-root IAM; read-only inventory mới; Terraform fmt/validate/test & new saved plans; estimate chi phí, timeout gồm teardown, owner approval mới. **Cấm tự apply trước Gate C.**
- [ ] L1 / Gate D0: fresh install theo scoped plan; chứng minh 2 Cloud+1 Edge (nếu cần), ALB, DDB, synthetic dataset, SSM/SSH, Chrony verified per-host/probe, server readiness. Bật host diagnostic sampling trước workload (không dùng hidden retries).
- [ ] L2 / Gate D2: Edge C1 trước, 90/90 PUT, 90/90 HEAD, 360/360 GET SHA-256, 0 lỗi 500/503, clock source verified; nếu fail → fail-fast, capture all dispatched outcomes, stop and teardown. **Edge C4 chỉ chạy khi C1 PASS**, cùng manifest/release/config.
- [ ] L3 / Gate D3: kiểm thử **native** R1 lease/fencing/closed writer, R2 outbox export/seal/cut, checkpoint/GC, DDB WCU/RCU/throttles/PITR, logs/outcome; test fixture cô lập không làm sai trạng thái benchmark. Chứng minh R1/R2 bằng native evidence, không dùng “20 ACTIVE tables” làm substitute.
- [ ] L4 / Gate D4: 8 shallow CloudWatch alarms (Cloud/Edge × `recovery_cut_age`, `integrity`, `throttling`, `telemetry_age`). Thu native alarm history thể hiện `OK → ALARM → OK` dựa **metric thực + threshold thật**, không dùng `SetAlarmState` hay publish giá trị 0 giả. `deep_observation=false`; event integrity/throttling phải từ fixture cô lập và an toàn, không cố tình làm hỏng dữ liệu chung.
- [ ] L5 / Gate E (luôn chạy, kể cả D2/D3/D4 FAIL): chụp sanitized metrics/native evidence, drain, Terraform destroy theo owned resources, verify direct absence + workload state = 0, IAM/SLR/policy restoration, retained bootstrap/state và pending deletion, reconcile **estimated vs observed vs later billed**.
- [ ] L6. Báo cáo `PASS / FAIL / DEFERRED` từng gate, per-request errors + reason, 8 alarm histories, cost/teardown proof. Không tuyên bố 100% nếu bất kỳ D3/D4 nào chưa chạy.

**Gate L:** Không được suy luận từ prior PASS của Cloud hay previous 41/41 để cấp phép AWS. Mọi tài nguyên, chi phí và thay đổi IAM đều cần approval mới. Nếu Edge chưa ổn định, tách D3/D4 sang phiên riêng **chỉ khi được owner duyệt**, không tự bỏ stop-the-line.

## 6. Trật tự thực thi / PR slicing

| Lượt | Phạm vi | Deliverables | Điểm chặn |
|---|---|---|---|
| 0 | Workspace và plan (đang chuẩn bị) | File này + AWS draft; nhánh mới từ merged #73 | Chưa chạy AWS |
| 1 | P1 fail-fast | `internal/benchmark/*`, `cmd/warmbench/*`, tests, CI; PR riêng | Gate F |
| 2 | P0/P1 RCA offline | Analyzer, sanitized report, tests; PR riêng | Gate R |
| 3 | AWS approval package | Cập nhật bản nháp run plan, release/dataset, budget, IAM, stop/teardown | Owner quyết định |
| 4 | AWS D2/D3/D4/E | Edge C1→C4, native R1/R2, 8 alarms, teardown, RCA | Gate L |

**Điều kiện chuyển sang AWS:** (i) Gate F + R PASS hoặc ghi rõ evidence gap/diagnostic-only objective; (ii) PR liên quan được review/merge; (iii) full offline suite & release pin PASS; (iv) owner duyệt mới bằng văn bản; (v) teardown và chi phí có guardrails khả thi.

## 7. Handoff/checklist để bắt đầu

- [x] PR #73 đã squash merge thành `7f6533e`.
- [x] `main` local cập nhật; worktree `feat/edge-acceptance-followup` tạo từ merged `main`.
- [x] Bắt đầu F1–F3 (không apply AWS).
- [x] Kiểm thử race/stop/reconcile và cập nhật contract/docs cho F4–F7. PR #74 đã merge vào `main`.
- [x] Đối chiếu evidence offline (R1–R7); không xuất private telemetry vào repo. Báo cáo RCA tại `docs/testing/edge-clock-rca-followup.md`.
- [ ] Review/merge các PR kế tiếp.
- [ ] Xin phê duyệt phiên AWS sau khi hoàn thành offline (đặc biệt D3/D4 không được tuyên bố PASS trước).

**Lưu ý:** Bản kế hoạch này là tài liệu định hướng công việc, không phải quyền truy cập AWS hay giấy phép tiêu tiền.
