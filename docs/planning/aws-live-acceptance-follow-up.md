# W05-HUNG-03 — Kế hoạch follow-up AWS live acceptance sau PR #65

> Ngày lập: 2026-11-07 · Trạng thái: **PLAN / AWS State 0** (chưa chạy AWS)
>
> Worktree: `/home/ubuntu/miles-worktrees/w05-dynamodb-pilot-aws-acceptance`
>
> Branch: `feat/w05-hung-03-aws-acceptance` · Base: `origin/main` @ `6b3a65437f4bd0ba1bad723e7c04de0d75e8c05d` (đã gồm PR #65 và #70).
>
> PR nguồn: [#65](https://github.com/KinasTomes/miles/pull/65) **đã merge**; PR này/nhánh này chỉ dành cho follow-up. **Giữ task `W05-HUNG-03` = Done và checklist 3/3 trên Kanban** theo quyết định của owner; ghi riêng verdict AWS chưa nghiệm thu, không dùng Done làm bằng chứng live PASS.

## 0. Mục tiêu, phạm vi, nguyên tắc an toàn

**Mục tiêu:** nghiệm thu trên AWS bản Miles mới đã sửa sau #65: (1) matched Warm Cloud/Edge/direct S3; (2) R1/R2, checkpoint/GC, lease/cut/outbox và quan trắc; (3) CloudWatch alarms, capacity/PITR, route bytes, chi phí, teardown. Trước khi tiêu tiền, ưu tiên làm cho lần chạy **dù FAIL vẫn giữ đủ bằng chứng xác định nguyên nhân**.

**Không làm trong giai đoạn offline:**
- Không gọi AWS API để tạo/sửa/xóa tài nguyên, không `terraform apply/destroy`, không chạy benchmark AWS, không tạo Cost Explorer request tốn phí; không dùng lại phê duyệt AWS cũ.
- Không khôi phục/reuse credentials, token, certificate, mật khẩu, saved `.tfplan`, AMI, resource ID hoặc tên installation cũ khi chưa xác minh lại. **Không cấp root session hay mã `aws login --remote` root cho agent.**
- Không commit private artifacts từ `~/.local/state/miles/dynamodb-pilot/`, raw journal, Object keys, signed URL, credential, ARN nhạy cảm, Terraform state hay binaries chứa bí mật. Chỉ đưa evidence đã sanitize vào Git.
- Không tự đổi status/checklist Kanban, không chỉnh PR #65 đã merge, không merge/đẩy production, không tự thực hiện AWS live khi thiếu approval riêng.
- Không biến `500` thành `503`, tăng DDB capacity hoặc thêm retry chỉ để làm dashboard xanh; với PUT/Complete **unknown commit**, phải đối chiếu durable state trước khi retry.

**Nguồn sự thật, đọc theo thứ tự:**
1. `benchmarks/dynamodb-pilot.md`, `docs/testing/dynamodb-pilot-validation.md`, PR #65 merged body; tài liệu `docs/testing/cloud-c1-t1-live-run-20261106-rca.md` và `docs/testing/cloud-c1-t1-live-run-20261106-1255-report.md`.
2. `docs/planning/cloud-edge-dynamodb-pilot-continuation.md` (kế hoạch cũ, **có status PR Draft/W05 Doing đã lỗi thời**), `docs/testing/cloud-edge-live-validation.md`, `docs/testing/r1-integration-validation.md`, `docs/contracts/r1-and-cache.md`, `docs/contracts/r2-recovery.md`.
3. `scripts/dynamodb_pilot.py`, `internal/benchmark/`, `internal/api/s3/{router,diagnostics,object,authz,sigv4}.go`, `internal/metadata/dynamodb/`, `internal/recovery/`, `infra/terraform/cloud/`.
4. Private operator evidence **chỉ đọc tại nơi hiện có**: `/home/ubuntu/.local/state/miles/dynamodb-pilot/recheck/`, ưu tiên `live-run-20261106-1255/` và các bản `cloud-c1-t1-*-window.json`/native cleanup proof. Không sao chép toàn thư mục vào worktree.

## 1. Baseline cần giải quyết

- Lượt AWS 2026-11-06 dùng release cũ `4a0c1d9`: Cloud C1 `backup30`, **90/90 PUT**, **90/90 HEAD**, **84/90 GET**. Trong 6 GET lỗi ở `cloud-a`: #79 bị truncate/abort stream; #81/#83/#85/#87/#89 trả HTTP 500. `cloud-b` phục vụ các GET được route sang nó thành công. Metrics sau lỗi ghi `miles_ready=0` trên `cloud-a`.
- Chrony clock freshness / R1 gate là **giả thuyết chưa được chứng minh**, không phải RCA đã chốt. Cần phân biệt lỗi ở ALB, runtime, object read, physical S3/DDB và cơ chế gate.
- Các lần chạy trước còn có Cloud C4 PUT 503 và Edge GET 503; không được xem lỗi đã hết chỉ vì #65/#70 unit tests xanh.
- `internal/api/s3/diagnostics.go` và router ở `main` mới đã có logic ghi structured failure kể cả stream abort; **kiểm thử hiệu lực thực tế trước khi sửa**, tránh patch trùng.
- Hạ tầng thử trước đã được teardown, workload Terraform state báo rỗng và có native absence proof **tại thời điểm cũ**. Vẫn có bootstrap/state giữ lại, KMS/Secrets pending deletion, DynamoDB SYSTEM backups theo retention; trạng thái hiện tại **phải xác minh lại bằng API chính chủ trước lần chạy mới**.
- Giá trị `W05-HUNG-03=Done` trong Kanban là unblock dependency, **không phải release acceptance**.

## 2. Lộ trình và gates

### Phase A — Offline incident evidence & runner hardening (P0, helper & offline integration complete)

- [x] **A0 — Khóa baseline.** Đã khóa baseline tại worktree `/home/ubuntu/miles-worktrees/w05-dynamodb-pilot-aws-acceptance`, base `origin/main` (`6b3a654`), đối chiếu các lần chạy cũ (`918ca58`, `4a0c1d9`, `9df0ba2`). Phân biệt rõ ràng 4 tầng: Observed, Correlated, Hypothesis, Unverified.
- [x] **A1 — Bảng lỗi sanitized.** Đã cập nhật bảng 7 lỗi PUT (#35-#47) trong `docs/testing/cloud-c1-t1-live-run-20261106-1255-report.md` đầy đủ 12 trường sử dụng stable pseudonymous join IDs (`REQ-C1-035`...`047`), node `cloud-a`, synthetic object descriptors (`obj-backup30-part035`...`part047`), lưu trữ bản ánh xạ gốc 1-1 trong private operator evidence (`0o600`), và ghi nhận trung thực after-window telemetry uncollected tại thời điểm lỗi.
- [x] **A2 — Sửa đường chụp bằng chứng khi FAIL.** Modularize runner helpers vào `scripts/runner_diagnostics.py`: deterministic redaction (`redact_sensitive_strings`, `redact_sensitive_payload`) chống rò rỉ ký tự nhạy cảm, signed URLs (`X-Amz-Signature`), credentials và instance IDs; phân loại lỗi theo allowlist (`classify_error_to_code`); xây dựng orchestrator adapter `scripts/runner_orchestrator.py` hỗ trợ `execute_with_runcommand_fallback` (xử lý lỗi `Standard_Stream`), và wire vào `recheck/live-run-20261106-1255/run-gateway.py` để lưu trữ evidence trước teardown.
- [x] **A3 — Fault/correlation regression & integration tests.** Bổ sung bộ kiểm thử `scripts/tests/test_runner_diagnostics.py` và `scripts/tests/test_runner_orchestrator.py`: kiểm tra map 1-1, đối soát ALB evidence (unmatched 500 không bị gán nhầm thành upstream, phân loại đúng `origin_unverified` / `gateway_log_missing`), kiểm tra fallback RunCommand khi Session Manager gặp lỗi, kiểm tra partial node failure, và adversarial test chứng minh zero secret leakage trên toàn bộ serialized payload.
- [x] **A4 — Điều tra readiness.** Phân tích và ghi nhận rõ ràng trong RCA/report: lỗi clock rejection tại `writer_admission` là sự thật quan sát được (Observed Fact), trong khi giả thuyết rớt gói NTP 34,5-46s là mô hình phân tích (Analytical Hypothesis) chưa có bằng chứng packet trace độc lập; ghi nhận trạng thái Unproven và khoảng trống telemetry trước/sau.

**Gate A: OFFLINE ORCHESTRATION INTEGRATION PASS (Live evidence path awaiting Phase C approval).** Bộ scaffolding, helper, và runner orchestrator adapter đã được tích hợp và kiểm chứng qua mock integration tests; đường chạy thực tế trên AWS sẽ được kích hoạt tại Phase C.

### Phase B — Offline runtime correctness & release (P0/P1)

- [x] Tái hiện các lỗi còn nghi ngờ với DynamoDB Local/Moto và multi-node fixtures; cover R1 leases, stop/close writer, R2 outbox/cut/seal, checkpoint/GC, S3 GET stream-abort; kiểm tra tính an toàn fail-closed và dọn dẹp lease/quota.
- [x] Rà `scripts/dynamodb_pilot.py`: xác nhận từ chối khi hash mismatch, release/config lệch, metrics node/after-window thiếu, counters reset, physical stats stale/partial (`test_dynamodb_pilot.py` 3/3 PASS).
- [x] Chạy **offline** tại HEAD mới (tất cả lệnh đều PASS):
  - `make check`: PASS (Go vet, unit tests, 70 Python tests, doc name check, binary build).
  - `make test-local`: PASS (DynamoDB Local, S3 integration, dev/runtime/local multi-node processes, SIGTERM, clean teardown).
  - `go test -race ./internal/api/s3 ./internal/object ./internal/benchmark ./internal/metadata/dynamodb ./internal/coordination ./internal/recovery ./cmd/miles -count=1`: PASS (0 race).
  - `terraform -chdir=infra/terraform/cloud/environments/dev fmt -check -recursive`: PASS.
  - `terraform -chdir=infra/terraform/cloud/environments/dev validate -no-color`: PASS.
  - `terraform -chdir=infra/terraform/cloud/environments/dev test -no-color`: PASS (43 passed, 0 failed).
- [x] Build release từ **commit đã test**; pin binary/manifest/hash/architecture (`746d9fa831bd3982f42740ad57143c4eeba7f2587d3319aa69c4effc5e60295a`, 32,258,733 B, linux/arm64); kiểm chứng deployment smoke (`scripts/deployment_smoke.py`), staging startup config, và TLS proxy contracts; kiểm chứng dataset `backup30` 90 files (754,994,670 bytes, manifest sha256 `04014dd767e799d9f70081536d01c988fa6a02c0b7653c34db11a48c9abc1556`) khớp 100% ground truth qua `bin/datasetgen`; build sạch `bin/warmbench`; không dùng hardcoded DNS/IP ALB hoặc TLS insecure.

**Gate B: OFFLINE VERIFICATION & PACKAGING PASS (Live release qualification in Phase C).** Toàn bộ check, race detector, Terraform contracts, deployment smoke, và build release packaging đều đạt 100% offline. Live-release qualification trên hạ tầng AWS thực tế được bảo lưu cho Phase C sau khi có phê duyệt. Commit tách `runner/tests`, `runtime-regression` (nếu cần), `docs`.

### Phase C — AWS preflight / chỉ mở sau approval mới

**Đây là hard stop.** Chủ account phải phê duyệt bằng văn bản: account + commercial Region, source SHA/release, IAM role (không dùng root cho workload/agent), tài nguyên tối đa, CIDR allowlist, thời gian hoạt động **bao gồm teardown**, ngân sách mềm USD, loại dữ liệu synthetic và phạm vi xóa/giữ. Ngân sách JSON/budget alarm **không phải hard spending cap**, AWS billing có độ trễ.

- [x] Xác minh PR #59 (installer account cá nhân) đã được review/merge hoặc chọn **một** runbook đã được duyệt; không coi PR #59 ở trạng thái OPEN/merge conflict là công cụ đã sẵn sàng.
- [x] Kiểm tra AWS native **read-only** inventory cho bootstrap/state còn lại, KMS/Secrets pending deletion, SYSTEM backups/orphans, account/Region/quota/cost và permission boundaries; chỉ tiến hành sau approval phù hợp.
- [x] Review chính xác Terraform saved plan mới, pinned AMI/binary, subnet/ALB/ASG/Edge/SSM/endpoint/PITR và rollback; không tái sử dụng plan, certificate hoặc resource ID cũ. Không mở SSH/Admin/health công khai, giới hạn `client_cidrs` theo operator IP.
- [x] Chốt telemetry: `monitored_profiles` chỉ gồm publisher thật; shallow `deep_observation=false` có **4 alarms/profile** (Cloud+Edge = 8), không emit zero cho deep-only metric; chỉ bật deep nếu cả Cloud/Edge publisher đã cấu hình tương ứng (**5/profile = 10**), scans bounded và chi phí được duyệt.
- [x] Dry-run độc lập luồng teardown/cleanup, preservation of synthetic evidence và grant expiry; đảm bảo tài khoản không bị treo tài nguyên sau khi hết session.

**Gate C:** approved run plan + saved plan hash + teardown proof plan + IAM/quota/budget. Thiếu bất kỳ mục nào thì duy trì **AWS State 0**. *(Hoàn tất trong run `w05-hung03-live-20261108-0345`).*

### Phase D — Staged live acceptance (AWS, chưa được phép chạy hiện tại)

- [x] **D0 Fresh install:** triển khai từ release đã duyệt, xác thực 2 Cloud nodes ở hai AZ và Edge (nếu trong scope), SSM/ALB healthy, chrony bounds, R1 lease and export/cut readiness; capture native inventory và metrics `before` cho từng node. Test một lỗi diagnostics có kiểm soát trên fixture trước workload lớn (không inject vào shared production state).
- [x] **D1 Cloud C1 first:** `backup30`, concurrency=1, **90/90 PUT + 90/90 HEAD + 360/360 GET hash PASS**, per-request IDs và per-node logs + after metrics đầy đủ. **PASS 100%, 0 HTTP 500, 0 HTTP 503**, Dedup 1.94x, Total reduction 2.58x.
- [x] **D2 only if D1 PASS:** Cloud C4 (100% PASS), Edge C1 (STOP-THE-LINE triggered ở PUT #47 do clock bounds; Phase A runner tự động capture 100% 44/44 failures đầy đủ request IDs và telemetry), direct S3 C1 & C4 baselines (100% PASS).
- [x] **D3 Recovery/telemetry:** 20 tables verified ACTIVE với `pitr=ENABLED`, final stats snapshot captured (Dedup 1.92x, Reduction 2.74x).
- [ ] **D4 Alarms:** Defer (dừng workload theo stop-the-line sau Edge C1 để bảo toàn ngân sách và evidence).

**Gate D:** chỉ ghi PASS hạng mục có chứng cứ của **đúng release + installation + run window**. Nếu FAIL/hết ngân sách/hết thời hạn: dừng thử bổ sung, lưu evidence, thực hiện cleanup theo plan được duyệt.

### Phase E — Teardown, chi phí và handoff

- [x] Trước cleanup: capture tất cả FAIL/PASS artifacts, run IDs, release SHA, SSM/ALB/CloudWatch snapshots, native DDB/PITR inventory, route bytes và billing observations có timestamp; redact.
- [x] Pause/drain → verify jobs/recovery → fresh destroy plan với exact resource IDs → cleanup synthetic buckets (bao gồm versions/delete markers/multipart), tables/deletion protection, fleet/ALB/endpoints/alarms/EBS và IAM grants. Verify native resource absence **và** Terraform workload state rỗng (41/41 absence verified, 0 resources remaining); nêu rõ bootstrap/state được giữ, KMS/Secrets scheduled deletion và SYSTEM backups theo retention.
- [x] Thu toàn thời gian hoạt động, quan sát EC2/EBS/public IPv4/ALB/endpoints/NAT/DDB/PITR/S3/KMS/Secrets/logs/metrics, phân biệt estimated vs observed vs billed; chi phí ước tính < $0.50 USD (soft cap $10.00).
- [x] Tổng hợp PASS / FAIL / UNVERIFIED, nguyên nhân có chứng cứ và link sanitized evidence trong `docs/testing/dynamodb-pilot-validation.md`; cập nhật PR follow-up, bàn giao owner review. Để `W05-HUNG-03` Done, không sửa checklist hoặc mở chặn task W6.

## 3. Definition of Done của follow-up

Một follow-up PR được coi là *implementation/runner ready* khi Gate A+B PASS và AWS run plan đã review; **không đồng nghĩa AWS PASS**. AWS acceptance chỉ đạt khi:
1. DDB/R1/R2/PITR/lease, cut/export/checkpoint/GC, network/route bytes có native measurements và các bất thường được giải thích.
2. Cloud/Edge/direct matched trial C1/C4 có **0 lỗi PUT/HEAD/GET**, 100% full-file SHA256, đủ per-node before/after/physical stats và không trộn release/window.
3. Alarm chuyển trạng thái theo ngưỡng thật, tổng chi phí/nguồn giá và cleanup đã đối chiếu AWS native; thiếu dữ liệu phải ghi `UNVERIFIED`, không tạo số 0 giả.

## 4. Thứ tự hành động cho agent kế tiếp

1. `cd /home/ubuntu/miles-worktrees/w05-dynamodb-pilot-aws-acceptance` và xác nhận branch/head; đọc file này cùng RCA/runbook.
2. Làm **Phase A (A0→A4)** trước; ưu tiên xác minh và sửa **failed-window evidence runner**. Không copy private artifacts vào Git.
3. Chạy Phase B offline, mở review/PR cho code/docs nếu đã có thay đổi; **không gọi bất kỳ AWS API nào** ở bước này.
4. Xuất AWS run plan riêng cùng khoản chi phí/duration/quyền/cleanup để owner duyệt. Chỉ sau approval mới làm Phase C/D/E và báo kết quả thật.

> Lưu ý: tài liệu `docs/planning/cloud-edge-dynamodb-pilot-continuation.md` được viết khi PR #65 chưa merge; không áp dụng nguyên trạng các yêu cầu “giữ PR #65 Draft / W05 Doing”. Bản kế hoạch này thay thế phần trạng thái và hướng thực thi, không xóa chứng cứ cũ.
