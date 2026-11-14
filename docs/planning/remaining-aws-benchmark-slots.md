# Bàn giao 59 slots benchmark AWS còn lại

Chạy **Plan A**, trả evidence để Lead review, rồi mới giao **Plan B**. Hai plan
là hai đợt vận hành; không phải hai script tự điều phối toàn bộ lifecycle.

| Plan | Phạm vi | Slots còn lại | File giao cho agent |
|---|---|---:|---|
| A | Warm, overwrite, domain, multipart, Edge clone, Recovery | 47 | [Plan A](aws-benchmark-plan-a.md) |
| B | Cold Cloud/Edge × Glacier IR/Flexible | 12 | [Plan B](aws-benchmark-plan-b.md) |

Mỗi plan dựng một deployment, chạy nhiều slot tuần tự rồi teardown. Nếu window
hoặc ngân sách hết, bàn giao các slot pending/incomplete, không tự kéo dài hoặc
chạy tiếp plan khác. Một slot = case × repetition; GET lặp không phải repetition.

## Quyết định đã chốt sau audit

- Frozen inventory lượt cũ được đọc lại đủ 93 mục: 89 ABSENT, 3 approved
  PENDING_DELETION, 1 shared role RETAINED. Không có STILL_PRESENT trong scope đó.
- Raw audit toàn account vẫn ghi `FAIL / ACCOUNT_BASELINE_CHANGED`; giữ nguyên
  raw result và baseline, không sửa thành “0 baseline removed”.
- Lead chấp nhận **documented drift**: policy `MilesDevDeployServices` mất
  version v4 không-default. Default v3 và audited role/trust/default-policy
  snapshots không đổi. Không tái tạo version hoặc sửa IAM để làm đẹp audit.
- Các exact concurrent resources của `miles-chien` đã nhận diện được giữ ngoài
  scope teardown của mình. Không delete/adopt/đổi tag/quyền/state của chúng.
  Không dùng substring hoặc tiền tố để miễn mọi tài nguyên mới.
- Biên bản quyết định và native receipts nằm trong private run directory cũ,
  được chỉ tới trong tracker local; không đưa account/resource IDs lên Git.
- Hai sai lệch lịch sử đã được disposition, **không còn là blocker của G1 mới**.
  Mỗi deployment phải chụp baseline mới. Sai lệch mới, đặc biệt baseline removal
  mới, phải được giữ và review riêng; không kế thừa ngoại lệ của lượt cũ.

## Source, scope và ngân sách chung

Code baseline đã review: `9476abc13dda7c1ddf5327d393d4bc6934ad42ff`.
Trước `init`, pin HEAD sạch chứa auditor đã sửa và hai handoff này. Agent ghi
full commit, tree/build hashes, SDK/config/dataset hashes vào evidence. Nếu HEAD
có thay đổi ngoài docs so với baseline này, review diff trước khi dùng.

Đã có `warm-cloud-c1-rep1` chạy binary `c93eec0`; giữ nó là historical pilot,
không chạy lại trong 59 slots và không tự gộp thành một repetition cùng source
mới. Go/SDK/IaC không đổi ở mốc lập kế hoạch nhưng runner/telemetry/IAM có đổi.
Bộ 60 slots cùng source là scope bổ sung, không tự chạy thêm để làm đẹp bảng.

Envelope khoảng **10–20 USD cho tổng A+B**, không phải 20 USD cho mỗi plan.
Không yêu cầu dự toán chi tiết. Plan B chỉ dùng phần ngân sách còn lại đã review
sau A; thiếu số liệu thì ghi unknown, không suy chi phí 0 hoặc bịa bill thực tế.
Chọn `hours` trước init, tối đa 12 giờ/window và giữ 90 phút cleanup theo kit.
Không có hard billing cap và không cam kết 59 slots chắc chắn vừa 20 USD.

Cloud chạy hai gateway sau ALB; Edge trong bộ kit này chạy EC2. Đó là Edge
profile trên AWS, không phải phép đo on-prem WAN. Không đổi topology giữa các
paired legs hoặc dùng kết quả AWS Edge để tuyên bố hiệu năng mạng local thật.

## Ranh giới thực thi và review

Khi được giao chạy một plan, agent làm các bước trong plan đó; không tự chuyển
plan tiếp theo, merge PR, mở rộng IAM/Region hoặc tự tăng budget/window. Giữ
approval receipts hiện có của kit. Quyền và phê duyệt Lead đã cấp trong phiên
vẫn có hiệu lực; không hỏi lại cho từng thao tác routine trong scope đã duyệt.
Nếu gói plan/IAM cụ thể chưa được duyệt, chuẩn bị đủ artifacts trước khi xin
approval; không tự ký thay người có thẩm quyền.

Nguồn kỹ thuật dùng chung: [runbook](full-aws-benchmark-execution-plan.md),
[operator guide](../testing/focused-aws-operator-guide.md),
[native evidence](../testing/benchmark-native-evidence.md),
[matrix](../../benchmarks/workload-matrix.json),
[thresholds](../../benchmarks/thresholds.md).

Tracker local ở `$HOME/.local/state/miles/benchmark-continuation/remaining-slots.json`
và `remaining-slots.csv`, đủ 59 slot IDs. Trên máy khác, tạo tracker mới từ bảng
case/repetition trong hai plan, không copy secrets hoặc state của run cũ.
Mọi slot đang pending trước bàn giao; agent ghi run directory, source, evidence
và verdict thật. Không lấy việc task
đã giao, CI xanh hoặc result file tồn tại làm bằng chứng AWS acceptance PASS.
