# Kế hoạch benchmark AWS toàn ma trận trong một installation

**Trạng thái: OFFLINE PREPARED — CHƯA ĐƯỢC PHÉP CHẠY AWS.** Đây là runbook bàn giao cho agent, không phải recipe đã có thể chạy 20/20 ca. Các bước IAM, Terraform apply, workload và cleanup đều cần approval riêng. Nguồn chuẩn: [acceptance kit](../../deploy/acceptance/README.md), [process](../testing/aws-acceptance-process.md), [operator guide](../testing/focused-aws-operator-guide.md), [matrix](../../benchmarks/benchmark-matrix.md), [thresholds](../../benchmarks/thresholds.md).

## Trạng thái offline và phạm vi

- Mọi trial dùng **một source commit sạch**, được git archive khi kit init; không ghép binary/telemetry từ release khác.
- Template hiện có focused-get-shutdown.json đã bảo vệ IAM, plan, GET Edge C1/C4, shutdown native-throttle và teardown. Nó **chưa** là full-matrix recipe.
- Ma trận: **20 cases × 3 repetitions = 60 slots**. Có 12 cases / 36 slots đã có warmbench (chưa được nối vào process); 8 cases / 24 slots Cold/Recovery còn requires-runner. Có ít nhất **24 direct-S3 paired legs** bổ sung, chưa kể domain contrast, clone control và policy/fault comparators.
- Offline planner ở deploy/acceptance/matrix_plan.py xác thực matrix và focused recipe, tạo 60 slot cố định cùng argv templates không thể thực thi, input hashes và danh sách evidence. Manifest luôn ghi: full_run_executable=false, aws_approval=false và benchmark_executed=false.
- BEN-01…BEN-05 và net savings đang ở trạng thái candidate/chưa được duyệt. Không chuyển INCOMPLETE thành PASS.

### Sinh manifest offline (chỉ sau khi commit và worktree sạch)

    cd /path/to/miles
    git status --short --branch
    make check
    python3 -B scripts/validate_benchmark_matrix.py benchmarks/workload-matrix.json
    umask 077
    mkdir -p -m 0700 "$HOME/.local/state/miles/matrix-plan"
    export OFFLINE="$HOME/.local/state/miles/matrix-plan/$(date -u +%Y%m%dT%H%M%SZ)"
    python3 -B deploy/acceptance/matrix_plan.py --out-dir "$OFFLINE"

Đầu ra nằm tại $OFFLINE/offline-execution-manifest.json, mode 0600 bên trong thư mục 0700. Đây là **kế hoạch**, KHÔNG được truyền trực tiếp làm process recipe. Không commit tài liệu private chứa AWS account ID, ARN, user, key/token, state hoặc raw artifacts nhạy cảm.

## Các phần agent phải hoàn thiện trước khi mở G1

### A. Tích hợp thực thi 12 cases Warm (blocker)

1. Bổ sung subcommand exact case ID + repetition trong deploy/acceptance/run.py, allowlist explicit trong process.py, orchestration trong workload.py, client staging trong client.py/hosts.py và source/dataset pin trong prepare.py. Mutations phải bind run ID + approval checkpoint. Đưa mọi benchmark vào tập TESTS để failed trial không thể reconcile thành PASS.
2. Đọc case từ chính workload-matrix.json đã hash (không tự suy từ chuỗi); 3 reps mỗi case, không dispatch duplicate case/rep. Tạo backup30 và boundary-8mib trên host client riêng; SHA-256 toàn file/manifest được verify trước mạng.
3. Sử dụng các argv template trong offline manifest, bind private config 0600, HTTPS, pinned release/SDK/CA, read-repeats=3, fail-fast, bounded timeout, no SDK retry và output directory mới. Retain objects chỉ khi có approved physical accounting và cleanup.
4. Provision per-trial identity/bucket/scope có audit. Domain cases cần hai credentials/buckets độc lập, signed debug quyền hợp lệ, header domain runtime và counter vật lý cho từng PUT, đối chiếu inter-tenant vs intra-tenant; không coi declared domain là bằng chứng backend. Clone cần source prefix đã seed, destination độc lập, GET→PUT streaming và hash receipt; same access key alias phải FAIL.
5. Tạo paired direct-S3 baseline: tối thiểu 24 legs, với S3 identity/bucket được phê duyệt IAM và ghi vào inventory/teardown; phải trùng dataset/Region/SDK/checksum/encryption/concurrency/transport/window. Với clone cần matched client-mediated GET+PUT, không lấy CopyObject làm đối chứng. Direct S3 không thay chunk-per-object comparator BEN-04.
6. Mỗi repetition phải chứng minh fresh backend/domain hoặc reset thực được verify; new prefix/user/bucket **không reset dedup/cache**. Chụp per-node Cloud/Edge metrics, R1/R2 state, physical inventory before/after, async debt, integrity và request IDs. Không đủ proof => INCOMPLETE.
7. Regression offline: thiếu comparator, thiếu physical counters, wrong domain, same access key alias, source prefix chưa seed, stale/mixed manifest SHA, unknown-commit PUT, duplicate repetition, timeout, missing cache reset, file output bị ghi đè, không còn cleanup reserve. Process không chấp nhận arbitrary shell/auto-approval.

### B. Xây 8 runner Cold/Recovery chuyên biệt (blocker)

Giữ requires-runner cho đến khi code+test thật tồn tại. Warmbench hiện từ chối cold/recovery; **không** sửa validator để cho runner=warmbench giả.

- **Cold 4 cases (Cloud/Edge × IR/Flexible):** lấy class/version backend thực tế sau policy transition, thời gian lưu tối thiểu và bill, IR GET ngay không ép RestoreObject, Flexible GET từ archive bị từ chối rồi RestoreObject202/ongoing/completed, expiry/renewal, revival, GET full SHA và inventory/retained bytes. Không tua clock giả lập thành AWS proof. Lifecycle có thể dài hơn một phiên 4 giờ.
- **Recovery 4 cases (Cloud/Edge × R1/R2):** fault scope approved, history/epoch/lease/fence, durable journal, sealed-cut, checkpoint restore/rebuild, idempotent replay, PUT/GET SHA và native readback. Focused shutdown --native-throttle **không** thay thế 4 recovery scenarios. Failure của invocation ban đầu vẫn FAIL nếu peer hoàn thành muộn.
- Cần harness bounded, negative tests với fake AWS, artifact trên lỗi, stopped/quiesced checks, exact resource scope, no blind retry và IAM minimum permissions.

### C. Sửa giới hạn lifetime/cost/topology và bảo vệ cleanup (blocker)

- Kit trong common.py hiện khóa tối đa **4 giờ và 10 USD** (trước thuế) với **90 phút reserve cleanup**; còn nhiều nhất 150 phút gồm cả provisioning và workload. **Không đủ cơ sở** để cam kết 60 slots + 24 direct-S3 legs + Cold async. Đừng chỉ đổi JSON config. Agent phải thiết kế lại và review common.py, pricing.py, iam.py, process.py, IAM expiry, budget, timeout, stop logic và teardown reserve nếu muốn chạy dài hơn.
- Bất cứ chi phí mới nào cũng cần dự toán Singapore tại ngày chạy: EC2/EBS/ALB/endpoints/NAT/IPv4, S3 state/versions/Cold min-retention/restore/get/put, DDB/PITR, KMS, logs, internet transfer và unexpired resources. Budgets là alert trễ, không phải hard cap. Phải có owner phê duyệt scope, thời gian, rollback, phí còn lại; approval cũ không tái sử dụng.
- Nếu yêu cầu **một installation qua nhiều approved paid windows**: phải có pause/quiesce, IAM renewal/expiry, no in-flight writes, exact source/ownership, billing khi paused và deadline cleanup cuối. Kit hiện không chứng minh hỗ trợ; không tự mở thêm phiên. Nếu không đủ an toàn, phải chia nhiều lượt độc lập được duyệt.
- AWS EC2 chạy Edge chỉ chứng minh Edge profile ở AWS, **không** thay Edge on-prem. Muốn claim Edge/on-prem cần host ngoài AWS và matching network/cost/cert/clock proof riêng.
- Bootstrap operator/deploy role, boundary và remote state là prerequisite. Không dùng root thực thi Terraform/workload. Giữ private profiles riêng IAM và deploy.

### D. Chốt full recipe sau khi A–C có code/tests

Agent tạo deploy/acceptance/templates/full-benchmark.json **mới** khi mọi command được whitelist và có implementation. Giữ các checkpoint:
approve-foundation → approve-hosts → approve-active → approve-cleanup-grant → approve-destroy.
Thêm review gate matrix/source/time/cost/fault/Cold nếu scope thay đổi. Không shell tùy ý, tự approve, bỏ step cleanup, tăng timeout mù hoặc sửa manifest planner thành ready không có code.

**Definition of offline-ready:** 20/20 case có runner thực và negative tests; 60 unique slots + comparators được binding; full recipe validate trong process; test race/expiry/fail-closed; IAM/cost/time/route và cleanup được review; make check và CI trên cùng HEAD PASS. Agent chưa được mở G1 nếu các blocker ở trên vẫn tồn tại.

## Trạng thái hardening sau HEAD 1bad2b8 (chưa phải full GO)

Các thay đổi bổ sung ở branch nhằm đóng lỗ hổng **PASS giả**, không khẳng định
AWS benchmark đã chạy:

- Cloud: đọc private ALB DNS từ Terraform output, bind hostname có TLS/SNI
  certificate verification. Fail nếu ALB DNS không trỏ RFC1918 hoặc TLS không
  xác thực. Cloud per-node telemetry vẫn cần nghiệm thu.
- Cold/Recovery: xác minh SHA-256 dataset trước khi seed, upload với đúng class
  (GLACIER_IR, GLACIER, STANDARD), HEAD readback class/length, giữ
  receipts trên từng PUT. Unknown PUT không tự replay. Cold Flexible restore
  vẫn phụ thuộc native transition/async window, không được giả lập.
- coldbench: tách GET logical bytes khỏi retained physical. Không có native
  inventory provider => artifact incomplete/fail-closed, không PASS.
- recoverybench: DynamoDB dùng AWS STS session riêng, verify expected
  account và expiry; không dùng DSG key. Checkpoint phải là native
  RecoveryEpochs ở trạng thái Sealed (không lấy genesis làm proof).
  Epoch tăng không tự chứng minh lease fence/seal/replay; thiếu native verifier
  => FAIL. Client chặn trước mọi mutation nếu chưa có reviewed fault injector
  + proof provider.
- Direct-S3: require bucket hiện có thuộc đúng account và có Project/Environment/
  AcceptanceRun tags của lượt hiện tại; thiếu bucket/tag/paired result =>
  fail-closed. Kit chưa tự provision/teardown bucket đối chứng này.

**VẪN BLOCKED TRƯỚC G1 MUTATION:** (1) triển khai và review native
Recovery fault injector và verifier đầy đủ ba chứng cứ, (2) provider inventory
physical bytes được pin và snapshot hai thời điểm cho Cold, (3) approved
provision + teardown direct-S3 bucket hiện tại, (4) đầy đủ Cloud per-node
telemetry và proof fresh/reset trước trial, (5) chi phí mới cho 60 slots
và async Glacier restore/minimum duration. Bộ lọc fail-closed có thể khiến
một số slot dừng nhưng không được xử lý bằng cách nới pass conditions.
Chỉ chuyển offline-ready khi code/tests/native evidence contracts và
approval riêng đáp ứng tất cả gate.

## Thứ tự triển khai AWS sau offline-ready

| Gate | Bước chạy | Điều kiện sang gate tiếp theo |
|---|---|---|
| **G1 — Preflight** | Đăng nhập hai profile qua aws login --remote; configure, init, assets, preflight, plan foundation, iam-review. | Account/Region/role/state/AMI sạch, source+recipe hash, IAM simulation/Access Analyzer và cost/plan approved; không adopt resources cũ. |
| **G2 — Deploy + smoke** | iam-activate, apply foundation, plan+approve+apply hosts, install, material, bootstrap, plan+approve+apply active, C1 smoke. | Cloud healthy, Edge+client private TLS/SigV4, clock, backend domain header/counters và GET SHA256 verified. |
| **G3 — Warm + comparator** | 12 cases × 3 reps, 24 direct-S3 paired legs, domain/clone baselines. | Raw samples, per-node metrics, verified isolation/reset, no false PASS. |
| **G4 — Cold/Recovery** | 4 Cold + 4 Recovery × 3 reps bằng runner thực; fault/revival/restore/epoch proof. | 24 slots có source-bound, native real AWS evidence, không dùng simulator thay nghiệm thu. |
| **G5 — Teardown** | collect, freeze một lần, snapshot, cleanup-review, cleanup-grant, unprotect, cleanup-plan, duyệt exact destroy.tfplan, cleanup, finish, final audit, logout. | final-absence-summary.json accepted=true; IAM rollback; chỉ pending 30 ngày của đúng KMS/secret được duyệt và ghi phí còn lại. |

**Stop the line:** AccessDenied, IAM/drift mismatch, stale plan, thiếu debug telemetry, SHA mismatch, unknown PUT commit, clock bất ổn, DDB read pressure ngoài dự toán, chi phí vượt mức hoặc gần 90 phút reserve, cleanup ownership uncertain => dừng, giữ receipts, đối soát rồi vào guarded cleanup path. Không force unlock/delete, -target/refresh=false, auto-retry mutation hoặc tự gỡ policy bảo vệ.

## Lệnh dùng cho agent khi full recipe đã được review và chủ account duyệt

    cd /path/to/miles
    aws login --profile miles-dev --remote
    aws login --profile miles-iam-bootstrap --remote

    umask 077
    export RUN="$HOME/.local/state/miles/manual-aws/$(date -u +%Y%m%dT%H%M%SZ)"
    export CONFIG="$HOME/.local/state/miles/acceptance-config-$(date -u +%Y%m%dT%H%M%SZ).json"
    python3 deploy/acceptance/run.py --run-dir "$RUN" configure --config "$CONFIG"
    python3 deploy/acceptance/run.py --run-dir "$RUN" init --config "$CONFIG"

    kit() { python3 "$RUN/source/deploy/acceptance/run.py" --run-dir "$RUN" "$@"; }
    proc() { python3 "$RUN/source/deploy/acceptance/process.py" --run-dir "$RUN" "$@"; }
    export RUN_ID="$(python3 -c 'import json,os;print(json.load(open(os.environ["RUN"]+"/run.json"))["run_id"])')"
    # Chỉ khi full-benchmark.json tồn tại và đã được review:
    proc init --template "$RUN/source/deploy/acceptance/templates/full-benchmark.json"
    proc status
    proc run --confirm-run "$RUN_ID"

Process code 20 = needs_approval, 21 = needs_auth, 22 = needs_reconciliation, 1 = failed, 0 = finished (không tương đương benchmark PASS). Đọc pending-review.json, IAM/saved plan và cost; chỉ duyệt từng checkpoint sau review. Nếu lỗi thì giữ FAIL và dùng proc run --cleanup --confirm-run "$RUN_ID" có giám sát, không dispatch lại unknown AWS mutation.

## Evidence bàn giao và verdict

Cần lưu source/release hash, pin AMI/toolchain, 60 slots (PASS/FAIL/SKIPPED/INCOMPLETE), 24 paired direct-S3 receipts, 3 độc lập repetitions, per-case full SHA256, Cloud/Edge real topology, backend domain/counter, Cold class/restore/revival/retention, R1/R2 fault/cut/checkpoint, per-node logs/clock, S3/DDB/KMS/cost windows, shutdown proof, frozen manifest, IAM rollback, final native absence audit và phí pending sau cleanup.

**Không claim full acceptance** nếu thiếu runner/approval/samples/metric scope/comparator/clean teardown. Ngưỡng candidate cần review độc lập; đối với dữ liệu cost thiếu, verdict phải UNKNOWN, không giả 0.
