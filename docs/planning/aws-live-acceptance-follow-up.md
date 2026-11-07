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

### Phase A — Offline incident evidence & runner hardening (P0, bắt đầu ngay)

- [ ] **A0 — Khóa baseline.** `git status --short`, `git rev-parse HEAD`, `git log -1`, đối chiếu `origin/main` trước từng thay đổi; xác nhận binary/release/SHA từng run cũ. Ghi bản đồ evidence có/không có và phân biệt observed / correlated / hypothesis / unverified.
- [ ] **A1 — Bảng lỗi sanitized.** Parse các run artifacts đã lưu: từng phase, ordinal, node, HTTP status, SDK code, client `X-Amz-Request-Id`, latency, bytes, request/correlation ID, thời điểm UTC, after-window metrics, source of error (gateway hoặc upstream). Không giả tạo mốc/gauge khi thiếu dữ liệu.
- [ ] **A2 — Sửa đường chụp bằng chứng khi FAIL.** Audit private `recheck/live-run-20261106-1255/run-gateway.py`: đường `snapshot()`, `node_journals`, SSM SessionManager `Standard_Stream not found`, fallback RunCommand, timeout/partial node failure. Chuyển phần helper **không chứa bí mật** vào test/tooling có version; không commit nguyên script private. Test cả `before`/`after`/journal và lỗi collect; thêm `captured_at`, `source`, `status`/missing reason; fail-closed khi Chrony mất `now_unix`.
- [ ] **A3 — Fault/correlation regression.** Trong fixture hai gateway, gây có kiểm soát một HTTP 500, một 503, một post-header stream abort, một no-gateway/upstream failure. Verify client request ID ghép đúng 1 bản ghi diagnostic (node/stage/class) hoặc nhãn `upstream_no_gateway_request`; trước và sau snapshot giữ trong artifact kể cả lỗi; không leak token, payload, Object key, signed URL.
- [ ] **A4 — Điều tra readiness.** Đọc Chrony sample/tracking, R1 gate status, process restart/ALB health tại cùng timestamp; nếu không có independent time evidence, báo **unproven**. Phân biệt client disconnect, object read, backend slow/timeout và R1 fail-close.

**Gate A:** test offline cho lỗi chủ động có correlation đầy đủ; failure artifacts vẫn được lưu và có nhãn missing/partial chính xác. Không chuyển sang AWS nếu còn đường thu log/snapshot im lặng thất bại.

### Phase B — Offline runtime correctness & release (P0/P1)

- [ ] Tái hiện các lỗi còn nghi ngờ với DynamoDB Local/Moto và multi-node fixtures; cover R1 leases, stop/close writer, R2 outbox/cut/seal, checkpoint/GC, S3 GET stream-abort; lưu regression fail-before/pass-after nếu tìm được bug thật. Không tự tuyên bố RCA khi chưa có evidence.
- [ ] Rà `scripts/dynamodb_pilot.py`: từ chối khi bất kỳ PUT/GET hash sai/thất bại, release/config lệch, metrics node/after-window thiếu, counters reset, physical stats stale/partial. Test report cả PASS và FAIL bằng fixtures.
- [ ] Chạy **offline** tại HEAD mới (ghi log, SHA, lệnh, thời gian):
  ```bash
  make check
  make test-local
  go test -race ./internal/api/s3 ./internal/object ./internal/benchmark ./internal/metadata/dynamodb ./internal/coordination ./internal/recovery ./cmd/miles -count=1
  terraform -chdir=infra/terraform/cloud/environments/dev fmt -check -recursive
  terraform -chdir=infra/terraform/cloud/environments/dev validate -no-color
  terraform -chdir=infra/terraform/cloud/environments/dev test -no-color
  ```
  Nếu Terraform cần init/plugin mới, chỉ dùng local/mock/backend=false và ghi rõ; tuyệt đối không backend remote/plan/apply AWS tự động.
- [ ] Build release từ **commit đã test**; pin binary/manifest/hash/architecture, config, dataset `backup30` 90 files (~720 MiB) và `warmbench`. Kiểm test script không dùng hardcoded DNS/IP ALB hoặc TLS insecure.

**Gate B:** check/race/contract sạch; có ma trận benchmark và artifact schema reproducible; không còn lỗi chẩn đoán quan trọng. Commit tách `runner/tests`, `runtime-regression` (nếu cần), `docs`.

### Phase C — AWS preflight / chỉ mở sau approval mới

**Đây là hard stop.** Chủ account phải phê duyệt bằng văn bản: account + commercial Region, source SHA/release, IAM role (không dùng root cho workload/agent), tài nguyên tối đa, CIDR allowlist, thời gian hoạt động **bao gồm teardown**, ngân sách mềm USD, loại dữ liệu synthetic và phạm vi xóa/giữ. Ngân sách JSON/budget alarm **không phải hard spending cap**, AWS billing có độ trễ.

- [ ] Xác minh PR #59 (installer account cá nhân) đã được review/merge hoặc chọn **một** runbook đã được duyệt; không coi PR #59 ở trạng thái OPEN/merge conflict là công cụ đã sẵn sàng.
- [ ] Kiểm tra AWS native **read-only** inventory cho bootstrap/state còn lại, KMS/Secrets pending deletion, SYSTEM backups/orphans, account/Region/quota/cost và permission boundaries; chỉ tiến hành sau approval phù hợp.
- [ ] Review chính xác Terraform saved plan mới, pinned AMI/binary, subnet/ALB/ASG/Edge/SSM/endpoint/PITR và rollback; không tái sử dụng plan, certificate hoặc resource ID cũ. Không mở SSH/Admin/health công khai, giới hạn `client_cidrs` theo operator IP.
- [ ] Chốt telemetry: `monitored_profiles` chỉ gồm publisher thật; shallow `deep_observation=false` có **4 alarms/profile** (Cloud+Edge = 8), không emit zero cho deep-only metric; chỉ bật deep nếu cả Cloud/Edge publisher đã cấu hình tương ứng (**5/profile = 10**), scans bounded và chi phí được duyệt.
- [ ] Dry-run độc lập luồng teardown/cleanup, preservation of synthetic evidence và grant expiry; đảm bảo tài khoản không bị treo tài nguyên sau khi hết session.

**Gate C:** approved run plan + saved plan hash + teardown proof plan + IAM/quota/budget. Thiếu bất kỳ mục nào thì duy trì **AWS State 0**.

### Phase D — Staged live acceptance (AWS, chưa được phép chạy hiện tại)

- [ ] **D0 Fresh install:** triển khai từ release đã duyệt, xác thực 2 Cloud nodes ở hai AZ và Edge (nếu trong scope), SSM/ALB healthy, chrony bounds, R1 lease and export/cut readiness; capture native inventory và metrics `before` cho từng node. Test một lỗi diagnostics có kiểm soát trên fixture trước workload lớn (không inject vào shared production state).
- [ ] **D1 Cloud C1 first:** `backup30`, concurrency=1, **90/90 PUT + 90/90 HEAD + 90/90 GET hash PASS**, per-request IDs và per-node logs + after metrics đầy đủ. **Bất kỳ** 500/503, truncation, hash mismatch, unknown commit hoặc missing crucial snapshot: **STOP**, collect failed-window/SSM/ALB/DDB/R1/native evidence; không tự retry/cứ chạy C4.
- [ ] **D2 only if D1 PASS:** Cloud C4, Edge C1/C4 và direct S3 với **cùng dataset/manifest, binary SHA, encryption/TLS, logical payload, retention, concurrency và số lần lặp**; giữ owned prefixes cho after stats. Chạy nhiều repeat matched theo budget đã duyệt, không gộp trial khác phiên/binary; so latency/throughput, cache, DDB calls, logical route bytes và gateway↔S3 physical transfer.
- [ ] **D3 Recovery/telemetry:** R1 leases/fence/writer, closed writer, R2 outbox/export/seal/cut age, checkpoint/GC, DDB call/consumed units/transaction/throttling + native PITR, metadata vs object integrity; Cloud/Edge metrics trước/sau với process lifetime nhất quán.
- [ ] **D4 Alarms:** thí nghiệm riêng cho `OK → ALARM → OK` bằng **threshold thật**, đủ `profile` và `installation`, xác minh missing-data/freshness, đánh giá deep alarms **chỉ khi deep publisher có dữ liệu**. Không coi `SetAlarmState` đơn lẻ là nghiệm thu. Không phá dữ liệu shared chỉ để kích alarm.

**Gate D:** chỉ ghi PASS hạng mục có chứng cứ của **đúng release + installation + run window**. Nếu FAIL/hết ngân sách/hết thời hạn: dừng thử bổ sung, lưu evidence, thực hiện cleanup theo plan được duyệt.

### Phase E — Teardown, chi phí và handoff

- [ ] Trước cleanup: capture tất cả FAIL/PASS artifacts, run IDs, release SHA, SSM/ALB/CloudWatch snapshots, native DDB/PITR inventory, route bytes và billing observations có timestamp; redact.
- [ ] Pause/drain → verify jobs/recovery → fresh destroy plan với exact resource IDs → cleanup synthetic buckets (bao gồm versions/delete markers/multipart), tables/deletion protection, fleet/ALB/endpoints/alarms/EBS và IAM grants. Verify native resource absence **và** Terraform workload state rỗng; nêu rõ bootstrap/state được giữ, KMS/Secrets scheduled deletion và SYSTEM backups theo retention.
- [ ] Thu toàn thời gian hoạt động, quan sát EC2/EBS/public IPv4/ALB/endpoints/NAT/DDB/PITR/S3/KMS/Secrets/logs/metrics, phân biệt estimated vs observed vs billed; không tuyên bố hóa đơn cuối khi billing chưa cập nhật.
- [ ] Tổng hợp PASS / FAIL / UNVERIFIED, nguyên nhân có chứng cứ và link sanitized evidence trong `docs/testing/dynamodb-pilot-validation.md`; cập nhật PR follow-up, bàn giao owner review. Để `W05-HUNG-03` Done, không sửa checklist hoặc mở chặn task W6.

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
