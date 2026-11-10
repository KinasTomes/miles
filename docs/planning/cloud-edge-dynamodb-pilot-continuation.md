# Kế hoạch tiếp tục DynamoDB Cloud/Edge pilot — PR #65

> **Bàn giao Agent | 2026-11-05 | Status: CHƯA NGHIỆM THU, GIỮ DRAFT**
> Repo: `KinasTomes/miles` · PR: https://github.com/KinasTomes/miles/pull/65
> Worktree: `/home/ubuntu/miles-worktrees/dynamodb-pilot-observability`
> Branch: `feat/dynamodb-pilot-observability` · Baseline HEAD: `0edbe2666f06f93bfb7cef9a327ffa32081b186d`
> Task: Cloud/Edge DynamoDB pilot AWS acceptance (`Doing`, chưa đủ 3 checklist). Đây là **kế hoạch thực hiện**, không phải báo cáo PASS.

## 0. Phạm vi và các quyết định bắt buộc

**Mục tiêu:** tìm và sửa *nguyên nhân đã được chứng minh* của PUT HTTP 500/503 trong pilot DynamoDB Cloud/Edge; đảm bảo đường thu log và metrics hoạt động cả khi workload FAIL; sau đó hoàn thành ba nhóm acceptance hiện có của PR #65 (DDB/telemetry; Warm Cloud/Edge/direct; alarms/cost/cleanup). Chỉ chuyển PR khỏi Draft sau khi bằng chứng đáp ứng đầy đủ các gate.

**Không được suy đoán:** đã tái hiện lỗi **phân loại** khi credential/authorization backend lỗi bị trả `InternalError` 500; **chưa chứng minh** đó là nguyên nhân 2 PUT lỗi trên AWS. Đừng tăng DDB capacity, thêm retry, đổi 500→503 hoặc sửa R1/R2 chỉ để làm test xanh mà chưa có evidence + contract review. PUT có thể ở trạng thái *unknown commit*: không retry tùy tiện làm mất idempotency, fence hoặc accounting.

**Phạm vi không được tự làm:** merge/mark Done; thay contract S3, IAM boundary, shared storage semantics mà không review; tạo EC2/ALB/endpoints/tables, chạy Cost Explorer tốn tiền hay tái dựng AWS chưa được duyệt mới; dùng lại AWS credentials, certificate, secret, saved `.tfplan` hoặc resource IDs của lần trước; xóa private evidence/retained Terraform bootstrap. Không đưa token, key, tên object nhạy cảm, raw journal, config credentials hay ARN riêng tư vào Git/PR. Trước mọi cloud run phải có approval **mới** cho ngân sách, thời hạn và cleanup.

### Nguồn cần đọc theo thứ tự

1. `CONTRIBUTING.md`, `docs/development/implementation-workflow.md`, PR #65 body/checklist, `docs/testing/dynamodb-pilot-validation.md`, `benchmarks/dynamodb-pilot.md`.
2. `docs/testing/cloud-edge-live-validation.md`, `docs/testing/cloud-edge-deployment-validation.md`, `docs/contracts/r1-and-cache.md`, `docs/contracts/r2-recovery.md`, `docs/contracts/closed-writer-reconciliation.md`, `docs/contracts/dynamodb-adapter.md`.
3. Code: `internal/api/s3/{router,diagnostics,sigv4,authz,object}.go`; `cmd/miles/s3.go`; `internal/object/service.go`; `internal/metadata/dynamodb/`; `internal/benchmark/{client,run}.go`; `scripts/dynamodb_pilot.py`; `infra/terraform/cloud/`.
4. **Evidence riêng trên operator VM, chỉ đọc, không commit:** `/home/ubuntu/.local/state/miles/dynamodb-pilot/recheck/logging-rerun-20261105-2057/` — `cloud-c1-t1-result.json`, `cloud-c1-t1-failed-window.json`, `handoff-current.md`, `retained-metadata-evidence/`, `native-final-cleanup-proof.json`; so với lần trước tại `final-run-20261105/` và `cloud-c1-root-cause-analysis.md` ở đó. Có thể đọc file dưới quyền operator; không sao chép vào repo.

**Mốc kiểm tra đầu tiên:** `git status --short`, `git rev-parse HEAD`, `git fetch origin`, xác minh PR branch/head và diff chưa merge; bảo toàn mọi thay đổi hiện có của người khác. Tiếp tục làm ngay trong worktree này; dùng commit nhỏ, tách implementation/test/docs. Nếu HEAD đã thay đổi, rà lại kế hoạch dựa trên code mới, không reset/force push.

## 1. Incident baseline — kết quả đã biết, không được diễn giải quá mức

| Lần thử | Kết quả | Evidence có / thiếu |
| --- | --- | --- |
| Lượt đầu, Cloud C1 `backup30` | **75/90 PUT**, 15 `InternalError`; 59 PUT đầu PASS, sau đó lỗi gần xen kẽ | 75 ObjectHeads commit; 14/15 PUT lỗi không có writer lease; hai Cloud node vẫn commit được |
| Lượt sau trên commit `0edbe26` | **88/90 PUT**; request #72 HTTP **503** `ServiceUnavailable` (~8,885 ms), #74 HTTP **500** `InternalError` (~233 ms) | Client có HTTP status và `X-Amz-Request-Id`, nhưng `after_nodes` rỗng; SSM Session Manager báo `Standard_Stream not found`; `node_journals` không thu được; fallback SSM RunCommand không tìm thấy sự kiện lỗi cần ghép |
| Đánh giá hiện tại | **FAIL** C1; không đủ bằng chứng xác định tác nhân backend | Hai Cloud đang chạy đúng binary logging SHA; thiếu correlation request→server stage. Không suy lỗi 503 do R1 gate hay 500 do DDB nếu không có log |

Trạng thái AWS tại lần teardown cuối: **Terraform workload state = 0**; 20 DDB tables, 2 data buckets, 4 EC2/ALB/ASG/endpoints/alarms và role tạm đã dọn. KMS `PendingDeletion` 30 ngày, secret được lên lịch xóa; giữ lại *chỉ* bucket Terraform bootstrap/state và evidence VM. Có 60 bản backup DynamoDB `SYSTEM` tự hết hạn; không khôi phục chúng chỉ để thử lại.

## 2. Workstream A — điều tra và sửa đường chẩn đoán (P0, làm offline trước)

- [ ] **A1. Ghép evidence và xác định tầng trả lỗi.** Parse JSON benchmark thành bảng 90 request có `phase`, thứ tự, size, latency, HTTP status, request ID, SDK code; đối chiếu S3 response header, tầng ALB/proxy/runtime (nếu có dữ liệu), DynamoDB ObjectHeads/Leases và metric window. Tạo một báo cáo *sanitized* `docs/testing/...` cho các quan sát có thể công khai; không phát tán request ID thật nếu không cần. Ghi rõ tín hiệu nào không có và điều gì chỉ là giả thuyết.
- [ ] **A2. Sửa đường thu log trong test runner.** Trong operator tooling, tìm lý do `snapshot()`/`node_journals` thất bại khi workload FAIL; kiểm tra phương thức SSM Session Manager, output schema và `journalctl -u miles.service` (định dạng thực tế). Có fallback qua SSM **RunCommand** hoặc logging sink đã được chứng minh, retry bounded, timestamp UTC, từng node riêng, timeout/bị từ chối/thiếu dữ liệu ghi thành trạng thái rõ ràng. Đường lỗi phải lưu client result + *after metrics* + log an toàn **trước teardown**, kể cả khi 1 node không truy cập được. Không âm thầm coi log rỗng là không có lỗi.
- [ ] **A3. Bảo đảm 500/503 có correlation end-to-end.** Kiểm tra `internal/api/s3/router.go` có log `request_id`, `node_id`, `operation`, `stage`, `failure_class` đúng lúc lỗi phát sinh; xử lý cả credential lookup, authorization, object factory, object lookup, writer admission, reserve, prepare/commit, writer cleanup và lỗi sau khi response đã bắt đầu. Xác định log đi stdout/stderr→systemd journal/CloudWatch theo config thực, và header `X-Amz-Request-Id` đến benchmark không đổi qua ALB. Nếu 503 do ALB chứ không phải S3, phải phân biệt nguồn; không gán stage giả.
- [ ] **A4. Test âm đường quan sát.** Fault-injection local gây **một 500 và một 503** trên hai gateway; bắt buộc artifact client có status/request ID, mỗi lỗi có đúng một record chẩn đoán server (node + stage + class) hoặc được phân loại rõ `upstream_no_gateway_request`. Test runner phải capture được cả trước/sau và log trong lúc thất bại. Bắt lỗi giả lập SSM `Standard_Stream not found` và verify fallback; xác minh không leak secrets/object keys/headers/signed URL. Mọi mismatch phải làm test FAIL.

**Gate A:** kiểm chứng tự động correlation 100% lỗi do gateway sinh ra trong các kịch bản fault injection; nếu lỗi do ALB/upstream thì phải có phân loại riêng, không được mất dấu. `after` metrics không được giả tạo hoặc ghi `before` thành `after`.

## 3. Workstream B — RCA và sửa lỗi runtime có bằng chứng (P0)

- [ ] **B1. Reproduce nhiều tầng có kiểm soát.** Với DynamoDB Local/Moto/composed Cloud+Edge, tiêm lỗi đúng vào `Credentials.GetCredential`, authorization snapshots, object policy, DDB `GetObject`, `writer_admission`, `Reserve`, `Prepare`, `Commit`, R1 gate/fencing và lost ACK/transaction timeout; quan sát response code, latency, stage/class, durable ObjectHead/lease, quota/fence, idempotency. Chạy cả concurrency **1 và 4**, nhiều node, interleave/failover; seed cố định để tái lập. So với hai request #72/#74, báo rõ match/khác.
- [ ] **B2. Khoanh vùng và sửa đúng lớp.** Chỉ khi repro chỉ ra nguyên nhân: sửa lỗi backend/read/recovery đúng semantic. Với `storage.ErrUnavailable` đi qua `sigv4.go`, `authz.go`, `objectError`, kiểm tra API error contract trước khi đổi HTTP 500→503; trường hợp đổi status phải test S3 XML/headers/request-id, SDK retry classification, no body leak và backwards compatibility. Nếu lỗi write path, chứng minh unknown commit không sinh duplicate heads, mất receipt, giải phóng quota sớm hay phá fencing. Giữ retries bounded, jitter/backoff có ngữ nghĩa, không che test FAIL.
- [ ] **B3. Regression cho đúng lỗi gốc.** Có test fail **trước** patch, pass **sau** patch; assertion không chỉ dựa vào HTTP code mà còn kiểm tra durable state và metrics/logs. Thêm stress/race cho nhiều node và test endpoint/DDB throttling độc lập nếu thấy bằng chứng. Khi không tái hiện được, **không báo RCA đã xong**; bàn giao chứng cứ thiếu + test cần thu thêm.

**Gate B:** fault được tái hiện và giải thích nhất quán ở request→gateway→metadata/state; lỗi được sửa với regression chứng minh, không thay contract không được duyệt. 100% PUT/GET/hash trong local fixture không có silent commit/failure mismatch.

## 4. Workstream C — kiểm thử và chuẩn bị acceptance (P1, vẫn offline)

- [ ] Chạy `make check`, `make test-local`, `go test -race ./internal/api/s3 ./internal/object ./internal/benchmark ./internal/metadata/dynamodb ./cmd/miles -count=1` và các test recovery/coordination liên quan (đúng package list thực tế), Terraform `fmt/validate/test` theo tài liệu repo. Ghi command, commit SHA, thời điểm, PASS/FAIL, platform/fixture; không suy local PASS thành AWS PASS.
- [ ] Kiểm tra `warmbench` result schema: status, HTTP status, request ID, failed samples, GET SHA, retention, declared release/environment và `dynamodb_pilot.py` từ chối report khi bất kỳ PUT/GET nào lỗi hoặc thiếu sau-window/stats. Bảo toàn binary/manifest/config/encryption, dataset `backup30` (90 file, ~720 MiB) và native ALB DNS (không pin IP).
- [ ] Rà runbook/cleanup: lỗi đầu vào/SSM/tunnel/SDK phải giữ evidence, không restart ẩu hoặc retry PUT chưa rõ commit; teardown luôn làm từ **fresh plan và exact current resource IDs**, không dùng lại `.tfplan` cũ. Có dry-run fail scenario offline kiểm chứng cleanup guard, thay đổi IAM có TTL và rollback tự động.
- [ ] Cập nhật tài liệu sự cố/validation phản ánh chính xác hành vi sau patch, nhưng không đánh dấu live acceptance đã đạt khi chưa có AWS evidence.

**Gate C:** local CI/test xanh trên commit cuối; toolchain sinh evidence bất kể PASS/FAIL, không log private data; API contract và security regressions đầy đủ. Tách commits theo `diagnostics/runner`, `runtime-fix`, `tests/docs` nếu thay đổi thực tế tương ứng.

## 5. Workstream D — AWS rerun có điều kiện (P2, CHỜ DUYỆT MỚI)

**Không bắt đầu ở bước này.** Chỉ mở sau Gate A–C PASS và người phụ trách cấp approval **mới** cho tài khoản, region, tài nguyên tối đa, ngân sách mềm, thời hạn, quyền IAM và synthetic cleanup. Approval **5–10 USD / 3 giờ đã dùng ở lượt trước không tái sử dụng**. Giá và billing trễ; ngân sách mềm không phải hard cap. Kiểm tra đầu tiên là state cũ rỗng bằng AWS native, KMS/secret cũ đang chờ xóa và Terraform backend/bootstrap không thay đổi.

- [ ] **D0 – Preflight:** approved fresh release/manifest/binary hashes trên 2 Cloud AZ + Edge, benchmark client; verified `RequestID` trace sink bằng một lỗi chủ động **offline hoặc fixture nhỏ có kiểm soát** trước `backup30`; TLS private CA/SigV4 strict; dynamic native ALB DNS; IAM baseline + expiry; backup evidence + exact rescue/cleanup plan đã review.
- [ ] **D1 – Trial tối thiểu để chẩn đoán:** Cloud **C1 (concurrency=1)** trên một namespace/dataset fresh, PUT 90/90, full GET hashes, after metrics và per-node logs. Nếu bất kỳ PUT/GET lỗi: **STOP**, thu correlation AWS per-request + DDB/R1/cut/ALB metrics và trạng thái unknown commit, không báo PASS/không tự chuyển C4. So sánh với #72/#74 và report nguyên nhân thực tế hoặc thiếu evidence.
- [ ] **D2 – Chỉ nếu D1 PASS:** Cloud C4, Edge C1/C4 và direct S3 matched trials (mỗi target/mức concurrency cùng binary, manifest, host, route/encryption, config); theo `benchmarks/dynamodb-pilot.md`, lý tưởng ba run/cấp nếu budget cho phép. Yêu cầu 0 request lỗi, tất cả GET checksum đúng, retained prefixes và stats refresh *sau workload*. Cấu hình capacity/throttle là điều kiện được ghi nhận, không tự điều chỉnh để che lỗi.
- [ ] **D3 – Recovery/telemetry:** R1 writer lease/fence, R2 outbox/export/cut seal, checkpoint/GC overhead, retry/throttle/consumed units, read/write capacity/PITR; 2 Cloud nodes + Edge metrics before/after, không restart counter, không thiếu/stale gauge. Thực hiện alarm threshold thực `OK→ALARM→OK` (không chỉ `SetAlarmState`), kiểm tra 10 alarms theo dimension, threshold, freshness. Kiểm kê endpoint/ALB/EC2/EBS/S3/DDB/PITR/KMS/secret/metrics cost, phân biệt observed/estimated/billed.
- [ ] **D4 – Teardown bắt buộc:** capture fail/pass evidence trước pause; xóa đúng fleet/endpoint/alarm, export synthetic DDB metadata, xóa version/marker/multipart chỉ bucket test, tắt deletion protection đúng 20 bảng, **fresh** destroy plan, KMS/secret scheduled deletion, ACM/SSM/SLRs ngoài Terraform, restore IAM default versions và xóa grant tạm. Verify native AWS absence + Terraform state 0; chỉ giữ bootstrap/state và evidence. Thu billing sau khi AWS cập nhật, không đoán hóa đơn.

**Gate D:** chỉ coi pass các hạng mục có bằng chứng native tương ứng. Nếu hết hạn hoặc quá ngân sách, ưu tiên shutdown/cleanup thay vì chạy thêm C4, Edge hay alarm; hạch toán mọi chi phí đã phát sinh, dừng process khác có thể gây nhiễu.

## 6. PR review / Definition of Done

Giữ ba checklist gốc của PR #65, mỗi mục phải có evidence gắn **đúng release + commit + window**:

- [ ] **DDB/PITR + lease/gate/export/cut/retry/bytes/network:** native capacity và throttles, actual R1/R2 cut/seal/checkpoint/GC overhead; không suy zero throttle từ metric thiếu.
- [ ] **Matched Warm Cloud/Edge/direct S3:** không PUT/GET 500/503, 90/90 PUT và full-file hashes, C1/C4, post-workload physical stats đầy đủ, mọi node/window hợp lệ, không trộn lần chạy/route.
- [ ] **Alarms + pricing + cleanup:** 10 probe states threshold thực, current regional price/source/date, resource inventory và whole-window costs; AWS teardown native absence, IAM rollback. Vấn đề billing lag phải nêu rõ.

Agent cập nhật `docs/testing/dynamodb-pilot-validation.md` và PR #65 với phần **passed/failed/unverified** thực tế; chèn private evidence path nếu thích hợp nhưng không chép file bí mật. PR vẫn **Draft** cho đến khi lead review và tất cả gate đạt; không tự đánh dấu task `Done`, không merge/bypass. Khi không đủ điều kiện chạy AWS, dừng ở C và báo blockers cụ thể.

## 7. Bàn giao đầu ra Agent cần trả

1. **RCA note**: timeline 2 request lỗi, request↔node↔stage mapping hay lý do không thể mapping, giả thuyết bị loại, lỗi gốc đã chứng minh/chưa chứng minh.
2. **Các commit độc lập trên branch hiện tại**: instrumentation + fault-injection, runtime fix chỉ khi được chứng minh, benchmark/runner/test/docs; `git diff --check`, tests/cấp độ proof và CI head.
3. **Checklist/decision log**: từng Gate A/B/C/D và checklist PR pass/fail/not-run, chi phí/permission nếu có, blockers; đề nghị review/duyệt AWS riêng trước khi dựng.
4. **AWS evidence + full teardown proof** nếu có lượt được phê duyệt mới; không xóa retained operator evidence; không khẳng định hoàn thành khi state hoặc IAM chưa clean.

### Prompt ngắn để giao Agent

> Tiếp tục PR #65 ở worktree `/home/ubuntu/miles-worktrees/dynamodb-pilot-observability`. Đọc kế hoạch `docs/planning/cloud-edge-dynamodb-pilot-continuation.md` và các tài liệu nguồn đã dẫn. Bắt đầu Workstream A→B→C hoàn toàn **offline**, ưu tiên khôi phục correlation request ID/node/stage cho HTTP 500/503 và fault-injection có durable-state assertions. Báo RCA xác nhận vs giả thuyết; commit từng phần trên branch hiện tại, giữ PR Draft. **Không tạo resource AWS hoặc dùng lại approval cũ**. Chỉ xin approval AWS mới khi gate offline đã PASS; mọi rerun phải có ngân sách/thời hạn/teardown và native evidence.
