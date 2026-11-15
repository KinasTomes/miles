# Plan B — Cold lifecycle AWS, 12 slots

## Lệnh bàn giao cho agent

> Thực thi riêng Plan B sau khi Lead đã review và giao chạy tiếp sau Plan A.
> Đọc report A và ngân sách còn lại; tạo deployment Cold mới, chạy 12 slots theo
> từng phase có receipt, giữ protection/R1 nguyên vẹn, teardown/audit và báo cáo.
> Không chạy lại Plan A, không tự báo đầy đủ 60 slots cùng source và không bật
> bypass để rút ngắn thời gian chờ.

Nguồn bắt buộc: [quyết định và phạm vi chung](remaining-aws-benchmark-slots.md),
[Plan A, gồm source/login/G1/G2/G5](aws-benchmark-plan-a.md),
[runbook Cold](full-aws-benchmark-execution-plan.md),
[native evidence](../testing/benchmark-native-evidence.md),
[matrix](../../benchmarks/workload-matrix.json),
[thresholds](../../benchmarks/thresholds.md).

## 1. Preconditions và deployment mới

Plan A phải có report, teardown/ownership evidence và disposition cho các vấn đề
còn lại; Lead đã review trước khi giao B. Chưa đạt điều đó thì B ở pending,
không launch trước để chờ A. Nếu A còn workload/outcome unknown, không dùng
chung namespace hoặc state cho B.

Tạo RUN/CONFIG mới ngoài Git với tên `plan-b-<UTC>`. Làm source/SSH/login và G1/G2
như Plan A; đổi đường dẫn, không dùng lại token, plan, TLS material hoặc run
state A. Dùng cùng source đã pin ở A nếu không có thay đổi cần review. Nếu code
đổi, ghi source/cohort khác; không hotpatch A/B để gộp kết quả.

`hours` chọn trước init, tối đa 12 giờ theo kit, luôn giữ 90 phút cleanup.
`max_usd` chỉ lấy **phần ngân sách A+B còn lại** đã được review, không tự reset
20 USD cho B. Không yêu cầu dự toán chi tiết; không cam kết restore sẽ kịp window.
Các quyết định v4/concurrent resources lịch sử đã chốt trong overview, không
cần xử lý lại; G1 B vẫn phải capture baseline/account/IAM hiện hành.

Có thể bỏ `direct-provision` khi chỉ xác minh lifecycle và không chạy comparator
Direct S3. Phải giữ `direct-plan` vì approval artifact của `benchmark-case` đang
cần nó. Nếu đã provision direct bucket thì cleanup/absence của bucket vẫn bắt
buộc như A. `policy-control` là comparator riêng: thiếu đối chứng thì không
claim cost/performance comparison PASS dù flow Cold đã PASS.

## 2. Danh sách 12 slots

Mỗi số repetition là một slot riêng, không đếm seed/status/poll thành slot.

| Case ID | Repetitions cần chạy | Slots |
|---|---|---:|
| `cold-cloud-ir` | 1, 2, 3 | 3 |
| `cold-cloud-flexible` | 1, 2, 3 | 3 |
| `cold-edge-ir` | 1, 2, 3 | 3 |
| `cold-edge-flexible` | 1, 2, 3 | 3 |

Ưu tiên verify một Cloud IR, một Edge IR, một Cloud Flexible và một Edge Flexible
trước khi mở rộng repetitions. Seed các slots dự định làm trong window khi
cluster quiescent, trước các restore jobs dài. Seed nhiều slots giúp chồng thời
gian chờ protection thật; không chạy foreground trials song song hoặc bỏ guards.

## 3. Kiểm readiness và lưu policy gốc

Xác minh Cloud ALB/Edge frontend, source, trusted clock, fixed SSM document,
metrics/OS/Chrony đủ từng node. Edge EC2 trong kit là Edge profile AWS, không
phải on-prem WAN. Không giữ sampler liên tục qua nhiều giờ chờ Glacier.

Trên installation acceptance riêng, lưu policy/revision gốc vào file 0600.
Tắt **tiering.enabled** và **tiering.auto_restore** bằng Admin policy CAS theo
runbook. Không dùng `MILES_BYPASS_PROTECTION`, không sửa `ProtectedUntil`, reset
R1 gate hoặc xóa unfinished jobs. Không thay đổi policy của installation khác.

Có thể build `dsgctl` cho máy operator từ chính frozen source (tool `workdir`
trỏ `$RUN/source`), rồi gọi Admin qua tunnel đã xác thực của kit:

```bash
go build -trimpath -o "$RUN/dsgctl" ./cmd/dsgctl
kit tunnel
"$RUN/dsgctl" --endpoint http://127.0.0.1:19001 --token-file "$RUN/system-admin-token" policy get > "$RUN/policy-original.json"
```

Ghi hash binary, đọc policy/revision hiện tại; tạo patch 0600:
`{"tiering":{"enabled":false,"auto_restore":false}}`. Gọi `policy set --file`
qua cùng endpoint/token, `--expected-revision` đúng giá trị vừa đọc. Giữ intent,
ACK và revision sau CAS. Không in bearer ra chat/log/argv. Không dùng revision 1
cứng. Mất ACK policy thì inspect, không gửi lại dựa trên phỏng đoán.

## 4. Seed và transition

Với từng case/repetition trong bảng:

```bash
kit approve --action benchmark-case --confirm-run "$RUN_ID"
kit benchmark-case --case-id cold-cloud-ir --repetition 1 --confirm-run "$RUN_ID"
kit cold-status --case-id cold-cloud-ir --repetition 1
```

Kết quả lúc này chỉ **prepared/waiting-protection**, chưa PASS. Xem snapshot và
native deadline; chờ tới `transition_ready` bằng UTC thật. Không chờ bằng một
shell sleep dài. Có thể seed slot tiếp theo nếu admission/reset cho phép.

Mẫu dispatch một phase; thay case/repetition/stage cho đúng slot:

```bash
kit cold-status --case-id cold-cloud-ir --repetition 1
kit cold-plan --case-id cold-cloud-ir --repetition 1 --stage transition
# Review exact generations/policy và cold-plan.json.
kit approve --action cold-step --confirm-run "$RUN_ID"
kit cold-step --case-id cold-cloud-ir --repetition 1 --stage transition --confirm-run "$RUN_ID"
kit cold-status --case-id cold-cloud-ir --repetition 1
```

`transition` cần status mới theo guard của kit; status/plan quá cũ thì đọc và
review lại trước khi gửi, không sửa timestamps. Tiering jobs dùng revision được
pin; policy bật tạm đã CAS về disabled trước enqueue. Chỉ đi tiếp khi native
S3 class và metadata đúng pack/generation. Có intent/unknown thì reconcile,
không reseed. Nếu policy tạm còn owned, dùng `policy-restore` với receipt đúng
revision theo runbook; không ghi đè concurrent update.

## 5. Chuỗi phase và điều kiện PASS

Mọi phase mutation đều theo `cold-plan` → review → approve `cold-step` →
`cold-step`; không chỉ sửa stage trong một approval cũ. Data/job telemetry có
window hẹp của phase, phải collect/verify đủ fleet. Raw result/receipts ở slot.

| Variant | Sau transition | Phải chứng minh |
|---|---|---|
| IR | `put-cold` → `part-cold` → `read` → `finish` | Đọc ngay không restore; PUT/UploadPart tái dùng đúng readable location; full GET SHA |
| Flexible | `put-cold` → `part-cold` → `restore-request` → status ready → `read` → `put-restored` → `part-restored` → `protection` → `finish` | Cold GET 403 đúng code, asynchronous restore, physical+metadata expiry, body revival, retained archive và protection |

Flexible `restore-request` phải trả WAITING ngay. Đọc `cold-status` bằng lệnh
riêng theo trạng thái, không gửi lại restore request hoặc treo request chờ hàng
giờ. Chỉ interleave các slots khi admission/quiescence cho phép; hoàn tất các
transition/Cold write đã định trước khi tạo restore jobs dài. Không coi concurrent
jobs là các phép đo latency độc lập hoặc nới gate để chạy nhanh hơn.

Sau ready, làm Restored PUT/part và protection gần nhau khi còn protection
window. Proof đòi native tiering job deferred, exact cancellation, pack vẫn
Warm; không dùng “không thấy lỗi” thay bằng chứng. UploadPart proof phải có
trước Complete, sau đó GET xác minh cả object. Giữ nguồn Cold cũ và mọi retained
versions/sidecars; không xóa để giảm physical bytes. Không download/restore để
thay client-body revival. Fixture near-expiry không chứng minh đã chờ AWS expiry.

`finish` chỉ PASS khi đủ phase receipts, `case-verdict.json`, source/manifest
binding, per-node telemetry, native class/location/CAS/protection và SHA-256.
Chưa có verdict hoặc chỉ GET/restore xong thì vẫn INCOMPLETE. Không dùng metric
orphan=0 một mình để kết luận terminal/isolation/protection an toàn.

## 6. Resume, deadline và dừng sớm

Mất SSH ACK: `cold-reconcile --case-id ... --repetition ...` của đúng slot;
`cold-status` chỉ đọc, không tự giải quyết unknown writes. Đừng gọi lại
`benchmark-case` cho slot đã seed. Session refresh được, nhưng original
installation cleanup deadline/source/slot/bucket không đổi. Native credentials
exported cho phase phải còn đủ expiry; không đổi role hay dùng static fallback.

Nếu restore chưa ready trước reserve hoặc ngân sách gần hết: ghi WAITING hoặc
INCOMPLETE, giữ native inventory và chạy cleanup đã review. Không để EC2/ALB
chạy vô hạn. Slot chưa hoàn tất không được chuyển state tùy ý sang deployment
mới, và không được tính là PASS để đủ 12.

Dừng slot mới khi hash/auth/domain/R1 lỗi, policy drift, unknown mutable request,
clock/telemetry thiếu, source/fleet đổi hoặc IAM scope khác. Không force unlock,
reset gate, bỏ protection, mở rộng quyền hoặc hotpatch production để qua test.

## 7. Teardown và report để review

Khi các jobs của B đã terminal/quiescent, phục hồi policy gốc bằng CAS đúng
revision nếu deployment còn phục vụ pha kiểm cuối. Unknown policy outcome phải
reconcile trước, không ghi đè. Sau đó G5 như Plan A: collect, shutdown, freeze,
snapshot, cleanup review/grant/unprotect/plan/destroy, finish IAM, native audit.
Chỉ chạy Direct cleanup nếu B thực sự đã tạo direct bucket. Không thay đổi
retained Cold lifecycle/protection để xóa nhanh; cleanup vẫn theo exact manifest.

Bàn giao `plan-b-report.md` private gồm:

- Bảng **12 slot IDs** và trạng thái thật; source/run/config/build/dataset hashes.
- Policy trước/sau, UTC protection/transition/restore times, request/job IDs,
  source/destination pack/generation/version/location revision, CAS và terminal
  evidence; IR reuse khác Flexible revival phải được phân biệt.
- Full GET SHA, UploadPart trước Complete, Cold/Restored negative cases, native
  retained physical inventory/bytes; cả archives cũ, minimum-retention và restore
  costs quan sát/unknown. Không tự tuyên bố savings nếu thiếu policy-control.
- Per-node clock/OS/metrics và window từng data/job phase, hash raw artifacts.
- Failed/unknown/waiting outcomes và missing coverage; không bịa AWS expiry wait.
- Stop/cleanup/absence/IAM evidence, approved pending/retained và baseline drift;
  usage/cost cho B và tổng A+B, phần unknown được ghi rõ.

**Kết thúc tại report để Lead/reviewer đánh giá. Không tự approve/merge hoặc báo
60 slots đã đạt chỉ vì A và B kết thúc thao tác.**
