# Chạy AWS acceptance bằng process và template

Bộ điều phối dùng lại [các lệnh kit](../../deploy/acceptance/README.md). Một
process chạy nhiều bước, ghi tiến trình/log ra disk và chỉ trả quyền điều khiển
khi cần duyệt, cần đăng nhập, có lỗi hoặc hoàn tất. Không cần agent dựng lại các
lệnh SSH/Terraform hoặc đọc mọi dòng log trong lúc AWS đang xử lý.

**Phạm vi:** kit và orchestration độc lập với các sửa runtime trong PR #86 và
workload của PR #88. `finished` chỉ nói các bước đã thoát thành công; không tự
chứng minh toàn bộ benchmark đạt. Audit đọc nguyên phản hồi AWS. Thay đổi đồng
thời của môi trường khác phải được đối soát bằng ownership/baseline; controller
không lọc theo tên người hoặc dựng lại trust policy để làm kiểm định đạt.

## Chuẩn bị một lượt

Chạy từ gốc worktree/checkout chứa bộ kit và runtime cần kiểm thử. Khi tích hợp
bản sửa benchmark, merge các thay đổi đã review vào một source sạch **trước**
`init`; không đổi source trong thư mục lượt đang chạy. Không mượn binary từ PR
khác rồi ghi nhận bằng chứng cho source của process.

Đăng nhập, tạo config và `RUN` mới theo [operator guide](focused-aws-operator-guide.md).
Các prerequisite IAM/backend vẫn như kit hiện có. Ví dụ phần offline sau khi có
`CONFIG`:

```bash
umask 077
export RUN="$HOME/.local/state/miles/manual-aws/$(date -u +%Y%m%dT%H%M%SZ)"
python3 deploy/acceptance/run.py --run-dir "$RUN" init --config "$CONFIG"

kit() { python3 "$RUN/source/deploy/acceptance/run.py" --run-dir "$RUN" "$@"; }
proc() { python3 "$RUN/source/deploy/acceptance/process.py" --run-dir "$RUN" "$@"; }
export RUN_ID="$(python3 -c 'import json,os;print(json.load(open(os.environ["RUN"]+"/run.json"))["run_id"])')"

proc init --template "$RUN/source/deploy/acceptance/templates/focused-get-shutdown.json"
proc status
```

`init` của process chỉ copy/kiểm tra recipe vào `RUN/process/`, không AWS API.
Recipe, source, config và run ID được bind bằng hash. Không lấy thư mục của lượt
cũ, kể cả lượt cũ đã đóng. Mỗi process là một lượt, không có daemon/service mới.

## Chạy và chờ kết quả

```bash
proc run --confirm-run "$RUN_ID"
```

Giữ lệnh ở foreground, hoặc để công cụ quản lý process của agent giữ session và
chờ exit. Không dùng vòng lặp agent liên tục đọc log. Caller chỉ cần nhận exit
code rồi đọc `proc status` và `process/summary.json`; mở log của đúng bước lỗi
khi cần điều tra. Worker in một dòng lúc bắt đầu mỗi bước và báo cáo ngắn khi
dừng. `proc status` đọc trạng thái khi worker vẫn đang chạy, không chờ khóa worker.

Template hiện có bao gồm:

1. Assets, preflight/baseline, plan foundation và IAM/chi phí.
2. Duyệt foundation → IAM activation/apply → plan hosts.
3. Duyệt hosts → apply, install, material và bootstrap → plan active.
4. Duyệt active → apply, C1, C4, freeze/snapshot và native-throttled shutdown.
5. Nếu các bước trên đạt, tự chuyển sang đường cleanup; dừng để duyệt quyền
   cleanup và saved destroy plan, rồi destroy, finish IAM/audit và logout.

Mỗi lệnh vẫn thực hiện guard vốn có của kit: native identity, saved plan,
ownership, clock/fencing, time budget và quiescence trước purge. Không shell tùy
ý, không đổi ngưỡng/retry để làm bài test đạt. Nếu bài test lỗi, process dừng để
giữ cơ hội debug; người vận hành phải vào đường cleanup khi hết thời gian được duyệt.

## Các điểm dừng có chủ đích

| Exit code | Trạng thái | Việc tiếp theo |
|---|---|---|
| `0` | `finished` | Xem verdict/evidence và audit của từng bài; không suy thành full benchmark PASS. |
| `20` | `needs_approval` | Đọc `process/pending-review.json` và toàn bộ artifact được hash, duyệt checkpoint rồi chạy tiếp. |
| `21` | `needs_auth` | Bước chưa dispatch. Kiểm tra đúng identity/phiên, login lại, rồi chạy tiếp. |
| `1` | `failed` / `finished_with_failures` | Đọc log được chỉ ra; giữ outcome, reconcile hoặc cleanup. |
| `22` | `needs_reconciliation` | Có bước đã dispatch nhưng failed/không rõ outcome. Không tự replay; dùng recovery và proof của kit. |

### Duyệt checkpoint

Ví dụ process đang chờ `approve-foundation`:

```bash
cat "$RUN/process/pending-review.json"
cat "$RUN/approval-review.md"
# Đọc cả proposal IAM, native validation/simulation, plan JSON và chi phí.
# Chỉ sau khi chủ account đồng ý đúng gói quyền/chi phí/data cleanup:
proc approve --checkpoint approve-foundation --confirm-run "$RUN_ID"
proc run --confirm-run "$RUN_ID"
```

Các checkpoint sau dùng đúng tên trong `current_step`: `approve-hosts`,
`approve-active`, `approve-cleanup-grant`, `approve-destroy`. Không tự approve
trong một vòng shell. Mỗi approval bind recipe, source, config và nội dung review;
plan receipt còn bind saved plan/JSON/vars. Nếu artifact thay đổi, approval cũ
không còn dùng được. Guard tuổi plan và expiry IAM của kit vẫn áp dụng.

### Hết phiên hoặc bị gián đoạn

```bash
kit login
kit login --iam
proc run --confirm-run "$RUN_ID"
```

Chỉ đăng nhập profile thực sự thiếu/sai, không tạo hai phiên mới ở mỗi checkpoint.
Runner không ghi code/MFA vào recipe hoặc log của controller, không tự làm mới
quyền IAM. Nếu session hết giữa một mutation, kết quả bước đó là chưa rõ cho đến
khi đối soát; login mới không tự cho phép chạy lại mutation.

Ví dụ apply đã mất response, dùng native recovery có sẵn trước:

```bash
kit recover-apply hosts --confirm-run "$RUN_ID"
# Sau khi recovery xác nhận state/outcome, ghi nhận proof để bỏ qua đúng bước:
proc reconcile --step apply-hosts \
  --evidence hosts-outputs.json --evidence hosts-state.json \
  --confirm-run "$RUN_ID"
proc run --confirm-run "$RUN_ID"
```

`reconcile` là xác nhận có chủ đích của operator sau đối soát, không phải native
recovery tự động. Chỉ trỏ đến proof thật trong `RUN`; controller giữ hash của
proof và giữ exit code/lỗi cũ. Không được dùng file placeholder để bỏ qua một
bước ghi. Không thể dùng `reconcile` biến trial hoặc shutdown lỗi thành PASS.
Các bước đã complete không bị chạy lại khi tiếp tục.

### Vào cleanup khi bài test lỗi

```bash
proc run --cleanup --confirm-run "$RUN_ID"
```

Đường này collect, freeze nếu chưa có manifest, snapshot rồi đến checkpoint
cleanup. Manifest hiện có được giữ nguyên, không freeze lại danh sách khác để
làm audit đạt. Các gate/xác nhận/ownership của kit không bị bỏ qua. Lỗi workload
và bước có outcome chưa rõ vẫn được giữ; cleanup thành công không xóa FAIL.

Process không tự force-delete khi không chứng minh được quiescence/ownership.
Nếu collector/partial apply bị chặn, làm theo operator guide để đối soát, giữ
evidence và xử lý tài nguyên trong deadline. Không bỏ lại AWS rồi mở lượt khác.

## Sửa template cho lượt tiếp theo

Copy [template](../../deploy/acceptance/templates/focused-get-shutdown.json)
trước process `init`, giữ trong file local hoặc commit vào repo để review.
Mỗi step có ID duy nhất, một lệnh kit được hỗ trợ và timeout; mutation phải tham
chiếu checkpoint đứng trước với đủ artifact quyền/chi phí/plan cần thiết.
`cleanup_steps` là đường teardown riêng, bắt buộc có trong recipe.

Có thể bỏ một bài GET không nằm trong scope đã chốt, hoặc thay shutdown pressure
bằng baseline. Không tự thay recipe giữa lượt. Với workload benchmark mới của
PR #88, chỉ nối adapter sau khi contract/CLI của PR đó ổn định và đã review;
template focused hiện tại không chứng minh coverage toàn bộ matrix benchmark.

## Trách nhiệm process và caller

- Process có timeout từng bước, bound với deadline hiện có trong paid window,
  và khóa riêng chống hai controller chạy cùng lượt. Khi timeout/TERM nó dừng
  đúng process group vừa dispatch; outcome remote chưa rõ phải reconcile.
- Không chạy các lệnh kit thủ công trong lúc process đang điều khiển lượt đó.
  Dừng/chờ worker trước recovery; hai lượt vẫn phải có namespace/state riêng.
- Script không thể đánh thức một chat đã đóng hoặc bảo đảm máy điều phối không
  bị tắt. Caller cần giữ session theo dõi process, có thông báo khi exit và
  người nhận xử lý checkpoint/lỗi. Không mặc định tự cleanup sau SIGKILL/mất máy.
- Reserve cleanup, deadline và envelope là giới hạn vận hành, không phải hard
  billing cap. Phê duyệt lượt cũ không dùng cho lượt mới hoặc mở rộng quyền.

Nguồn sự thật: `process/state.json`, `process/summary.json`, `process/logs/`,
receipt/verdict của kit và native evidence. Không commit thư mục private `RUN`.
