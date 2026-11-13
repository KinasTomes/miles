# Tự chạy AWS để kiểm tra GET và shutdown

Dùng [bộ chạy trong repository](../../deploy/acceptance/README.md), không dùng lại
script, state, plan, token hoặc chứng chỉ của lượt đã đóng. Bộ chạy chuẩn bị PKI,
IAM review/rollback, dựng AWS, bootstrap, benchmark, telemetry, snapshot và
cleanup. Những gì anh cung cấp là phiên AWS đăng nhập và quyết định duyệt gói
quyền/chi phí mới; không phải tự viết script hay policy.

## 1. Chạy ở đâu và cần gì?

**Chạy các lệnh bên dưới trên máy điều phối, từ gốc worktree chứa bản sửa.**
Cloud/Edge/client chạy trên AWS; Oracle chỉ điều phối. Runner tự SSH/SSM sang
đúng máy. Không cần tự copy lệnh test vào từng host nữa.

```bash
cd /home/ubuntu/miles-worktrees/clock-journal-reliability
export REPO="$PWD"
git branch --show-current
git status --short
python3 deploy/acceptance/run.py --run-dir /tmp/unused --help
```

Source phải sạch và đã commit trước `init`. Nếu dùng máy khác, thay đường dẫn
worktree. Không pull/đổi branch giữa phép so sánh; sau init dùng runner đã freeze
trong thư mục lượt mới.

Cần Python3.12+, Go theo `go.mod`, Terraform/provider theo lockfiles, Docker ARM64,
AWS CLI hỗ trợ `aws login`, Session Manager plugin, OpenSSH, OpenSSL và `uvx`.
`uvx` chạy IAM Policy Autopilot0.4.0, không tự upload/attach policy.
Account cần bootstrap deploy role, runtime boundary và state backend như
[bootstrap operator](../../infra/terraform/cloud/bootstrap/operator/README.md).
Đây là prerequisite account, không phải gói private của một lượt chạy.

## 2. Đăng nhập rồi tự tạo cấu hình

**Terminal điều phối, nhập code trực tiếp vào CLI khi được hỏi:**

```bash
aws login --profile miles-dev --remote
aws login --profile miles-iam-bootstrap --remote
```

Với deploy: CONSOLE đăng nhập IAM user bằng MFA, Switch back rồi Switch role mới
sang deploy role; chọn đúng role trên trang authorization của URL vừa in. Account
hiện tại dùng `hung-deploy` → `MilesDevDeploy`; account khác dùng danh tính của
account đó. Lấy code trả về và dán vào prompt CLI, không vào chat/Git.

Với IAM: browser session riêng đăng nhập root/MFA, dùng URL của phiên
`miles-iam-bootstrap`. Phiên này chỉ IAM/audit; workload không chạy bằng root.
Nếu nhờ agent tạo phiên, agent gửi link và chờ code như quy trình trước.

```bash
umask 077
export RUN="$HOME/.local/state/miles/manual-aws/$(date -u +%Y%m%dT%H%M%SZ)"
export CONFIG="$HOME/.local/state/miles/acceptance-config-$(date -u +%Y%m%dT%H%M%SZ).json"
python3 deploy/acceptance/run.py --run-dir "$RUN" configure --config "$CONFIG"
```

`configure` chỉ đọc: xác nhận cùng account, đúng deploy role/root, state bucket
và boundary có sẵn; chọn AMI AL2023 ARM64 mới nhất và **pin ID vào config**.
Đọc config trước khi dùng. Region mặc định Singapore; dùng `--region`,
`--deploy-profile`, `--iam-profile`, `--backend-bucket`, `--boundary-arn` nếu account
có cấu hình khác. Config không có secret. Có thể tự điền
[config example](../../deploy/acceptance/config.example.json) thay vì configure.

## 3. Chuẩn bị offline, không tạo AWS

```bash
python3 deploy/acceptance/run.py --run-dir "$RUN" init --config "$CONFIG"
kit() { python3 "$RUN/source/deploy/acceptance/run.py" --run-dir "$RUN" "$@"; }
kit status
kit assets
```

`init` build release/công cụ benchmark, tạo CA TLS và CA Roles Anywhere riêng,
SSH key mới, private Terraform copy và provider configuration. Nó từ chối
worktree bẩn và run directory đã tồn tại. `assets` build RPM offline bundle bằng
Docker image pin digest, kiểm chữ ký và tải signing helper pin SHA; có thể mất
vài phút và cần Internet trên máy điều phối, chưa có chi phí AWS.

Có thể đặt `assets_dir` trong config để reuse **chỉ** bundle RPM/helper đã kiểm
hash. Không trỏ tới script/plan/token cũ. Mọi lần chạy vẫn tạo identity riêng.

`kit status` in `run_id`. Ghi biến cho các lệnh cần xác nhận đúng lượt:

```bash
export RUN_ID="$(python3 -c 'import json,os;print(json.load(open(os.environ["RUN"]+"/run.json"))["run_id"])')"
```

## 4. Read-only preflight, plan và review

```bash
kit preflight
kit plan foundation
kit iam-review
```

Preflight kiểm AMI/account/state, chụp account/EBS baseline trước mọi IAM mutation.
State workload phải rỗng, không có data bucket/runtime roles của installation
khác. Bootstrap/state, role ASG có sẵn và key/secret pending của lượt trước được
preserve. Không dùng namespace `miles-dev` cho hai lượt song song cùng account/Region.

Plan ghi saved plan, JSON và baseline Autopilot. IAM review render expiry mới,
đọc default policy versions để rollback, validate/simulate native scopes và lấy
regional prices. **Chưa attach grant, apply hoặc tạo VM.**

Đọc trước khi chốt:

```bash
cat "$RUN/approval-review.md"
cat "$RUN/cost-review.json"
cat "$RUN/foundation-plan.json"
```

Review cả plan JSON, `iam/*.json`, validation/simulation và baseline Autopilot
trong `plans/`. Gói mô tả tối đa5 VM/root disposable, hai Cloud + Edge + client,
private ALB/endpoints, shared S3/20tables, cap Chunks512/Outbox và bảng khác128,
write128. Mặc định tối đa4giờ và envelope10USD trước thuế gồm cleanup, **không phải
hard cap billing**. Lượt cũ không cấp phép cho lượt mới.

Chỉ sau khi chủ tài khoản duyệt gói cụ thể mới chạy các mục mutate tiếp theo.
`--confirm-run` buộc lệnh bind đúng lượt; nó không thay bước đọc và duyệt gói.

## 5. Dựng foundation, Edge/client, rồi Cloud

```bash
kit iam-activate --confirm-run "$RUN_ID"
kit apply foundation --confirm-run "$RUN_ID"
kit plan hosts
# Đọc hosts-plan.json và saved plan JSON trước apply.
kit apply hosts --confirm-run "$RUN_ID"
kit install --confirm-run "$RUN_ID"
kit material --confirm-run "$RUN_ID"
kit bootstrap --confirm-run "$RUN_ID"
kit plan active
# Đọc active-plan.json và saved plan JSON trước apply.
kit apply active --confirm-run "$RUN_ID"
```

Lần tạo resource tính phí đầu tiên ghi T0/deadline trong `paid-window.json`.
Runner giữ90phút cuối cho cleanup; trial cần thêm25phút trước reserve. Theo dõi
`kit status` và nhật ký trong lúc chạy. Không để lệnh lỗi rồi sang bước kế.

`install` cài RPM/Chrony/TLS và dataset trên AWS trong khoảng chờ S3 versioning.
`material` tự đợi đủ15phút theo mốc `versioning.json` trước lần ghi đầu; không
tạo secret mới khi retry cùng token.

Bootstrap chỉ một lần trên installation rỗng. Operator AWS credentials đi stdin
vào memfd trên client; runtime Edge sau đó dùng Roles Anywhere. Source/backend
phải khớp cả ba gateway; Active đợi hai Cloud target healthy, giữ mọi địa chỉ
private và đọc lại caps native. Proxy byte/request-ID log bật sẵn trước GET.

## 6. Các kịch bản cần chạy

### A. GET correlation: C1, C4

```bash
kit trial --concurrency 1 --confirm-run "$RUN_ID"
# Chỉ chạy C4 nếu C1 đạt, còn đủ deadline và đã xem artifact.
kit trial --concurrency 4 --confirm-run "$RUN_ID"
```

Mỗi trial có user/domain/bucket mới, fail-fast, không client retry; dùng backup30,
90PUT/90HEAD/360GET đúng SHA, giữ object cho bài shutdown. Runner tự khởi động
recorder/probe trước tải, lấy response window từ result và thu raw Chrony,
gateway/proxy logs. Tên trial in cuối lệnh; artifact nằm trong `trials/<tên>/`.

Client PASS và clock PASS là hai verdict riêng. Ba log canceled của lượt cũ
chưa chứng minh mất bytes; lượt mới dùng request-ID/status/bytes để đối chiếu.
Khi có lỗi, dừng bài phụ thuộc, thu evidence và reconcile outcome; không lặp
đến khi một trial khác PASS để xóa kết quả cũ.

### B. Tạo history/current phù hợp cho shutdown

Chỉ vài PUT trên epoch rỗng không tương đương lỗi cũ. Sau tải ban đầu, nếu chưa
có epoch cũ, có thể đợi cut tự nhiên rồi tạo thêm current events:

```bash
kit wait-cut
kit trial --concurrency 1 --confirm-run "$RUN_ID"
kit freeze
kit snapshot
```

`wait-cut` chỉ đọc gate, có deadline và dừng trước cleanup reserve; không sửa
clock/gate để ép cut. `freeze` bind state + native IDs + baseline hashes, kể cả
máy/đĩa đã thay và resource ngoài Terraform. `snapshot` ghi số events/pending,
source payload SHA và inventory S3 trước pulse. Ghi rõ nếu history nhỏ hơn lượt
trước (1.242 historical /405 current events), không tuyên bố cùng điều kiện tải.

### C. Chọn bài shutdown

**Baseline, không gây tải đọc:**

```bash
kit shutdown --confirm-run "$RUN_ID"
```

**Hoặc bài cần ưu tiên, native throttling:**

```bash
kit shutdown --native-throttle --confirm-run "$RUN_ID"
```

Ngừng mọi client mutation trước lệnh này. Native pulse chỉ Scan đúng Outbox trên
AWS client: bốn worker,120giây, page1024/AWS1MiB, không SDK retry. Giữ cap128,
không tăng timeout8phút. Mỗi worker dừng khi gặp lỗi ngoài throttle.

Shutdown proof phải thuộc đúng invocation trước stop: exit0 và
`shutdown complete`. Native pressure còn cần có native throttle; zero throttle
là chưa exercise, không phải đã chứng minh chịu tải. Lỗi vẫn giữ FAIL dù Cloud
peer ghi hết progress sau đó. Stage/attempt/error-class/timing nằm trong journal;
work_failure và cleanup_failure được phân loại riêng, không có raw secret/keys.

Muốn chạy baseline rồi pressure trong **cùng hạ tầng**, giữ proof đầu rồi restart
có chủ đích, tạo thêm current events và snapshot mới:

```bash
kit restart --confirm-run "$RUN_ID"
kit trial --concurrency 1 --confirm-run "$RUN_ID"
kit snapshot
kit shutdown --native-throttle --confirm-run "$RUN_ID"
```

Restart giữ nguyên installation/release/profile, chuyển proof cũ sang
`previous-stops/`. Không tự chạy thêm vòng clock hoặc full GC/recovery/alarm.

## 7. Evidence và cleanup bắt buộc

Kể cả bài test lỗi, giữ evidence trước khi xóa:

```bash
kit collect
# Nếu SSH đứt sau trial, thu lại bằng tên đã dispatch, không replay writes:
kit collect --trial get-c1-REPLACE_WITH_TRIAL_SUFFIX
# freeze chỉ chạy nếu chưa có frozen manifest từ mục6.
kit freeze
kit snapshot
kit cleanup-review
```

Nếu đã freeze thì không chạy lại; manifest immutable. Đọc `cleanup-review.json`,
`iam/cleanup.json` và validation; quyền chỉ bind resource IDs đã freeze. Cần phiên
IAM/deploy còn hợp lệ cho cả cleanup và audit, reauth bằng `kit login` hoặc
`kit login --iam` nếu lệnh thật báo hết phiên.

```bash
kit cleanup-grant --confirm-run "$RUN_ID"
kit unprotect --confirm-run "$RUN_ID"
kit cleanup-plan
# Đọc destroy-plan.json/destroy-reviewed.json: chỉ delete đúng managed graph.
kit cleanup --confirm-run "$RUN_ID"
kit finish --confirm-run "$RUN_ID"
kit status
kit logout
```

`unprotect` chỉ đổi deletion protection của bảng thuộc manifest. Cleanup không
apply cấu hình Active để bật lại node; dùng exact saved **destroy plan** đã
refresh sau unprotect. Cloud min/desired về0 và drain tự nhiên; Edge stop; trước
purge phải chứng minh process đã dừng/client không còn ghi. Xuất metadata và
payload proof lần nữa, giữ snapshot lỗi cũ riêng, rồi xóa synthetic versions,
delete markers/multipart và runtime graph/root disks không backup đã được duyệt.
Không force-delete hoặc ABANDON để tính là clean stop.

`finish` khôi phục IAM versions/defaults, gỡ cleanup grant/auditor, dọn đúng SLR
lượt tạo rồi chạy final audit root read-only. KMS/secret mới chờ xóa30ngày theo
manifest; bootstrap và baseline giữ nguyên. Role ELB có thể cần chờ AWS bỏ tham
chiếu stale sau khi ALB đã mất; role này không tính phí.

Chỉ hoàn tất khi `final-absence-summary.json` có `accepted=true`, không có
unapproved residual/error/baseline missing. `PASS_WITH_PENDING` chỉ cho đúng key/
secret mới đã duyệt; `UNVERIFIED` không phải PASS. `logout` chặn nếu audit chưa
đạt, dừng đúng tunnel và xóa cache auditor của lượt, không đụng process/cache khác.

## 8. Khi gặp lỗi

| Hiện tượng | Cách xử lý |
|---|---|
| Login400 | URL mới đầy đủ từ CLI, đúng browser session. Nếu link bị biến dạng, dùng raw URL CLI. Không dùng code/link cũ. |
| Login xong vẫn hết phiên | Switch back/đăng nhập MFA rồi Switch role mới. Chọn role thay vì IAM user. Access credential15phút tự xoay khác với hạn console role. |
| AccessDenied | Giữ error receipt, kiểm action/resource/boundary/endpoint policy/expiry; không dùng root chạy workload hoặc cấp AdministratorAccess. |
| Preflight báo có installation | Không adopt. Đối chiếu state/bucket/roles và dọn/giữ môi trường cũ theo ownership trước khi mở lượt mới. |
| IAM review/cost không đạt | Chưa chạy activation/apply. Xem native findings, missing action/context, region prices và scope; review sửa gói trước. |
| Assets lỗi | Chưa có AWS. Giữ asset-build.log; kiểm Docker ARM64/Internet/disk. Chỉ xóa thư mục `assets` thuộc run chưa có proof để rebuild, không xóa thư mục historical. |
| Apply mất response | `kit recover-apply foundation --confirm-run "$RUN_ID"` (hoặc hosts/active) kiểm đủ state/proof trước nhận outputs. Không replay apply khi graph partial/unknown. |
| Host staging mất response | `kit recover-host edge` hoặc client kiểm completion marker và source; `kit install` sau đó bỏ qua host đã có ready receipt. Partial stage chưa có marker phải reconcile, không chạy lại installer mù. |
| Bootstrap failed/unknown | Giữ `bootstrap-intent.json`, token và log/status trên client; không xóa metadata hay chạy bootstrap lại. Dừng trước benchmark, reconcile hoặc cleanup disposable có bằng chứng. |
| ASG thay node liên tục | Kiểm source/VersionId/SHA, catalog/secret/provider/clock/readyz; không sửa readyz thành200. |
| Dataset/output lỗi | User benchmark sở hữu dataset0700; `--out` là thư mục mới; CLI đã sinh config0600 đúng schema. Không chmod world-readable. |
| Recorder exit1/UNVERIFIED | Xem khoảng giữa sample/cuối window và native measurement logs, không tự coi chronyd crash. |
| GET đủ SHA nhưng canceled | Ghép client sample requestID với proxy bytes/status và journal. Không thêm blind retry hoặc suy mất dữ liệu. |
| Shutdown hết8phút | Giữ FAIL và classified attempt logs/snapshot. Không tăng timeout để coi đã sửa. |
| Cleanup bị block | Giữ state/inventory, kiểm client/process/gate/session và manifest. Partial graph/host không liên lạc phải reconcile; không force-delete để vượt guard. Không để tài nguyên chạy mà mở lượt khác. |
| IAM restore concurrent change | Dừng, giữ phiên và đối chiếu document/version; không ghi đè thay đổi của người khác. |
| Final audit lỗi/timeout | Giữ failed receipt, kiểm auth/network rồi chạy `kit audit` lại; không sửa verifier để bỏ resource. |

Nếu phiên đang trả ExpiredToken thì login mới; không xin thêm mã chỉ vì một lần
export credential sắp hết hạn. Giữ90phút reserve, tránh thử thêm bài khi deadline
không còn đủ test+cleanup.

Nếu Terraform destroy bị gián đoạn sau khi purge đã hoàn tất, đăng nhập lại rồi
chạy `kit cleanup-plan`, xem plan delete-only còn lại, sau đó chạy lại
`kit cleanup --confirm-run "$RUN_ID"`. Runner dùng receipt purge đã bind với
manifest để không SSH vào host đã xóa. Plan mới chỉ được xóa đúng identity còn
sống trong graph đã freeze; audit cuối vẫn kiểm tra toàn bộ IDs ban đầu.


## 9. Bàn giao để review

Gửi đường dẫn `$RUN`, source/release/profile hashes và các verdict. Cần tối thiểu:
`run-log.jsonl`, `trials/`, raw telemetry/Chrony/proxy/journal, `stop-proof.json`,
`stop-journal.log`, `pulse-result.json`, snapshots/payload proof, frozen manifest,
IAM rollback receipts, final audit và cost review. Request/AWS correlation còn
trong runtime; PUT409 không cần điều tra lại trong lượt này.

Không commit token, credential JSON, key, certificate private, state/plan hoặc raw
sensitive artifacts. Khi cần tôi đọc, để bộ private trong workspace được phép;
máy khác thì chuyển bản evidence đã che secret. Chi phí Pricing/envelope không
phải hóa đơn cuối; đối soát billing khi AWS cập nhật.
