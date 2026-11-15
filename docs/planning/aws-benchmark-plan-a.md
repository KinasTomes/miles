# Plan A — Non-Cold benchmark AWS, 47 slots

## Lệnh bàn giao cho agent

> Thực thi Plan A này trên workspace Miles. Đọc các nguồn bắt buộc, pin source
> và tạo private run mới. Dựng một deployment cho các slot non-Cold, chạy từng
> slot và kiểm evidence trước slot tiếp theo, teardown/audit rồi báo cáo để
> Lead review. Không chạy Cold hoặc Plan B. Không coi smoke/local tests là slot
> benchmark; không tuyên bố Done nếu thiếu slot, comparator hoặc teardown proof.

Đọc [quyết định audit và phạm vi chung](remaining-aws-benchmark-slots.md),
[runbook G1–G5](full-aws-benchmark-execution-plan.md),
[operator guide](../testing/focused-aws-operator-guide.md),
[native evidence](../testing/benchmark-native-evidence.md),
[matrix](../../benchmarks/workload-matrix.json) và
[thresholds](../../benchmarks/thresholds.md) trước khi thao tác.

## 1. Phạm vi và ranh giới

47 slots = 11 Warm + 3 overwrite + 6 domain + 9 multipart + 6 clone + 12 Recovery.
Có **23 paired Direct S3 legs** cho các case comparator `direct-s3`; chúng thuộc
slot tương ứng, không cộng thành 23 slots mới. Giữ historical Warm C1 rep1 riêng.

Dựng một lần cho Plan A. Namespace kit là installation disposable của mình,
không phải `miles-chien`; đọc đúng config/state/ownership từ inventory. Các
quyết định v4/concurrent resources trong overview đã chốt, không hỏi lại hoặc
sửa IAM để xử lý drift lịch sử đó. G1 mới vẫn phải kiểm quyền thực tế.

## 2. Source, SSH và login

- Kiểm `git status`, branch, HEAD, CI của source chọn; không dùng worktree bẩn.
  Không đổi/pull/hotpatch source giữa deployment. Dùng worktree riêng khi cần.
- Shell tools của agent phải truyền `workdir` trỏ đúng checkout/source; không
  dựa vào `cd` ở một tool call để thay cwd của call sau. Các biến `RUN`, `CONFIG`,
  `RUN_ID` phải được thiết lập lại trong từng shell call nếu session không giữ env.
- Chạy trên máy SSH điều phối. Khi cần `aws login --remote`, **in URL đầy đủ**
  trong code block để người dùng copy sang browser cá nhân; không yêu cầu mở
  browser trên server và không xin authorization code trong chat.
- Root/IAM profile `miles-iam-bootstrap` nhận mã từ
  `$HOME/.local/state/miles/iam-login-code.txt`; deploy profile `miles-dev` nhận
  từ `$HOME/.local/state/miles/cli-login-code.txt`. File 0600, không symlink,
  không echo mã/ghi vào log/Git; tắt TTY echo nếu đưa mã vào prompt. Xóa nội dung
  mã đã dùng sau thành công. Không ghi đè root profile thành deploy identity.
- Sau login, STS root/IAM và assumed deploy role phải cùng approved account.
  Root chỉ làm phần IAM/audit được phép; runtime/benchmark dùng đúng role.

## 3. Chuẩn bị offline và G1

Chọn đường dẫn mới bên ngoài Git, không dùng lại run đã đóng. Các lệnh dưới giả
định `workdir` là checkout sạch đã pin; profile/account/AMI do configure xác minh.

```bash
umask 077
export RUN="$HOME/.local/state/miles/manual-aws/plan-a-$(date -u +%Y%m%dT%H%M%SZ)"
export CONFIG="$HOME/.local/state/miles/plan-a-config-$(date -u +%Y%m%dT%H%M%SZ).json"
python3 deploy/acceptance/run.py --run-dir "$RUN" configure --config "$CONFIG"
```

Review config và đặt `hours` phù hợp **trước init**; tối đa 12 giờ theo kit,
không dùng default 4 giờ rồi tự gia hạn khi đang chạy. `max_usd` không vượt
20 USD tổng envelope A+B. Không cần bảng dự toán chi tiết. Reuse assets chỉ khi
RPM/helper cache đúng hash; không reuse script, plan, token, TLS key của run cũ.

```bash
python3 deploy/acceptance/run.py --run-dir "$RUN" init --config "$CONFIG"
kit() { python3 "$RUN/source/deploy/acceptance/run.py" --run-dir "$RUN" "$@"; }
export RUN_ID="$(python3 -c 'import json,os; print(json.load(open(os.environ["RUN"]+"/run.json"))["run_id"])')"
kit assets
kit preflight
kit plan foundation
kit direct-plan
kit iam-review
```

Đọc saved plan JSON, IAM validation/simulation, rollback snapshot, envelope và
ownership. G1 phải chụp baseline mới chứa tài nguyên Chiến đang tồn tại; giữ
chúng nguyên vẹn. Không copy old exceptions sang baseline mới hoặc xóa baseline
policy versions để giải phóng chỗ nếu chưa được duyệt cụ thể.

## 4. G2 — Một deployment cho cả Plan A

Thực thi từng action sau khi gói tương ứng đã được duyệt, giữ hash-bound receipt:

```bash
kit approve --action iam-activate --confirm-run "$RUN_ID"
kit iam-activate --confirm-run "$RUN_ID"
kit approve --action apply:foundation --confirm-run "$RUN_ID"
kit apply foundation --confirm-run "$RUN_ID"
kit plan hosts
# Review hosts plan trước apply.
kit approve --action apply:hosts --confirm-run "$RUN_ID"
kit apply hosts --confirm-run "$RUN_ID"
kit approve --action install --confirm-run "$RUN_ID"
kit install --confirm-run "$RUN_ID"
kit approve --action material --confirm-run "$RUN_ID"
kit material --confirm-run "$RUN_ID"
kit approve --action bootstrap --confirm-run "$RUN_ID"
kit bootstrap --confirm-run "$RUN_ID"
kit plan active
# Review active plan trước apply.
kit approve --action apply:active --confirm-run "$RUN_ID"
kit apply active --confirm-run "$RUN_ID"
kit approve --action direct-provision --confirm-run "$RUN_ID"
kit direct-provision --confirm-run "$RUN_ID"
```

Trước slot đầu, kiểm hai ALB targets healthy, Edge/client ready, HTTPS/SigV4,
clock, source/build hash và SSM fixed document version/hash/ENV_VAR support.
Smoke là phụ trợ, không thay slot. Không chạy full local test suite lại nếu
không có code change hoặc failure cụ thể cần chứng minh.

## 5. Danh sách 47 slots

Mỗi số repetition là một slot riêng. Đừng dispatch một slot đã có intent.

| Case ID | Repetitions cần chạy | Slots |
|---|---|---:|
| `warm-cloud-c1` | 2, 3 | 2 |
| `warm-cloud-c4` | 1, 2, 3 | 3 |
| `warm-edge-c1` | 1, 2, 3 | 3 |
| `warm-edge-c4` | 1, 2, 3 | 3 |
| `overwrite-cloud` | 1, 2, 3 | 3 |
| `domain-shared-default` | 1, 2, 3 | 3 |
| `domain-separated` | 1, 2, 3 | 3 |
| `multipart-threshold-minus-one` | 1, 2, 3 | 3 |
| `multipart-threshold-equal` | 1, 2, 3 | 3 |
| `multipart-threshold-plus-one` | 1, 2, 3 | 3 |
| `edge-clone-download` | 1, 2, 3 | 3 |
| `edge-clone-upload` | 1, 2, 3 | 3 |
| `recovery-cloud-r1` | 1, 2, 3 | 3 |
| `recovery-cloud-r2` | 1, 2, 3 | 3 |
| `recovery-edge-r1` | 1, 2, 3 | 3 |
| `recovery-edge-r2` | 1, 2, 3 | 3 |

Thứ tự mở đầu: `warm-cloud-c1-rep2`, `warm-edge-c1-rep1`,
`recovery-cloud-r1-rep1`, `recovery-cloud-r2-rep1`. Chỉ khi adapter tương ứng
đạt mới làm thêm reps. Sau đó ưu tiên một lượt mỗi case còn lại để phát hiện
lỗi sớm; shared/separated cùng repetition chạy gần nhau. Cuối cùng hoàn tất
các reps còn thiếu, dựa vào tracker chứ không chạy lại slot mở đầu.

Mẫu cho một slot (thay case/repetition đúng bảng, không có full-run loop):

```bash
kit approve --action benchmark-case --confirm-run "$RUN_ID"
kit benchmark-case --case-id warm-cloud-c1 --repetition 2 --confirm-run "$RUN_ID"
kit status
```

Approval hết hạn sau 45 phút hoặc input drift thì review/ghi receipt mới theo
ủy quyền hiện có. `benchmark-case` tự collect/verify khi thành công; không chạy
lại workload để sửa lỗi collect. Mất ACK thì kiểm intent, tiến trình remote và
collect/reconcile đúng slot. Không ghi đè `result.json` hoặc bỏ failure samples.

Giữa hai slots: domain mới có native emptiness scan; purge ACK đủ từng node;
fleet/incarnation không đổi; cache/OS/clock đủ window; background jobs quiescent
cho reset tiếp theo; còn đủ credential/deadline và 90 phút cleanup reserve.
Bloom retained phải được công bố, không gọi fresh prefix là reset dedup.

Recovery chỉ synthetic `lease-revoke` và `checkpoint-cut`, native stale-fence
rejection, sealed chain, replay hai lần cùng genesis, seed objects và full GET
SHA-256. Không kill node, sửa raw DynamoDB/gate hoặc claim node-crash/PITR.
Domain cần runtime domains, physical counters và cross-owner denial. Multipart
cần actual part/API dispatch tại ba boundaries. Clone cần source/destination
khác domain, byte pipeline và SHA receipt; seed của clone không phải slot mới.

Clone comparator `matched-get-put` chưa được ghép tự động như `direct-s3`:
runner PASS chỉ chứng minh flow, không tự thành paired cost/performance PASS.
Nếu không có baseline đối chứng matched, giữ benchmark verdict INCOMPLETE cho
phần so sánh và báo thiếu; không tự thêm tooling hoặc đổi matrix để che gap.

## 6. Điều kiện dừng

Hash mismatch, auth/domain leak, unknown commit/job, IAM scope sai, source/fleet
đổi, clock/telemetry thiếu, reset không chứng minh được, deadline/envelope gần
hết: dừng slot mới, giữ evidence và đối soát. Không retry mutation mù, nới R1,
đổi clock hoặc sửa code trên host đang chạy. Chỉ fix code ở source reviewable;
source mới cần run/cohort mới. Metric dưới target vẫn phải báo trung thực.

## 7. G5 — Teardown một lần và dừng bàn giao

Ngừng dispatch mới, collect và shutdown không thêm fault/throttle ngoài scope:

```bash
kit collect
kit approve --action shutdown --confirm-run "$RUN_ID"
kit shutdown --confirm-run "$RUN_ID"
kit direct-cleanup-plan
# Review exact owned versions, delete markers và quiescence.
kit approve --action direct-cleanup --confirm-run "$RUN_ID"
kit direct-cleanup --confirm-run "$RUN_ID"
kit freeze
kit snapshot
kit cleanup-review
# Review owned IDs, rollback/cleanup scope.
kit approve --action cleanup-grant --confirm-run "$RUN_ID"
kit cleanup-grant --confirm-run "$RUN_ID"
kit approve --action unprotect --confirm-run "$RUN_ID"
kit unprotect --confirm-run "$RUN_ID"
kit cleanup-plan
# Review delete-only saved destroy plan.
kit approve --action cleanup --confirm-run "$RUN_ID"
kit cleanup --confirm-run "$RUN_ID"
kit approve --action finish --confirm-run "$RUN_ID"
kit finish --confirm-run "$RUN_ID"
kit audit
```

Nếu freeze/cleanup đã có intent hoặc frozen manifest, reconcile thay vì chạy
lại lệnh tạo mới. Audit native phải tách ABSENT/PENDING_DELETION/RETAINED và
baseline drift. Concurrent resources mới chỉ ngoài scope khi có exact native
ownership evidence; không sửa raw FAIL thành PASS. Sau audit được chấp nhận,
`kit logout`; nếu strict audit fail, giữ raw ledger và trình disposition cụ thể,
không tự bỏ guard chỉ để logout. Không để workload chạy trong lúc chờ review.

Bàn giao một report private `plan-a-report.md` gồm:

- Full source/run/config/build/dataset hashes, topology, SDK và thời gian.
- Bảng **47 slot IDs**: passed/failed/incomplete/pending, run/evidence paths;
  tách flow PASS, metric/threshold verdict và comparator completeness.
- 23 paired Direct S3 legs; phase N/p50/p95/throughput, full GET SHA, per-node
  hit/miss + clock/OS; R1/R2 native receipts và giới hạn fault scope.
- Failures/unknown outcomes, commands/config đủ tái lập; không biến không chạy
  thành PASS. Historical rep1 không gộp ngầm vào source mới.
- Usage/chi phí quan sát, phần unknown và ngân sách A+B còn lại cho Plan B.
- Stop proof, direct absence, destroy log/state, IAM rollback và final audit;
  retained/pending/deviations được liệt kê rõ.

**Kết thúc Plan A tại đây. Không chạy Plan B. Chờ Lead/reviewer đánh giá report.**
