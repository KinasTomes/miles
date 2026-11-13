# Kế hoạch benchmark AWS theo từng bước do operator quyết định

**Trạng thái: chuẩn bị offline; chưa phê duyệt ngân sách hoặc chạy AWS.** Ma trận
vẫn là **20 cases × 3 repetitions = 60 slots**, có tối thiểu **24 paired direct-S3
legs**. Đây là inventory công việc; không có lệnh mặc định tự chạy toàn bộ G1–G5.

Nguồn: [acceptance kit](../../deploy/acceptance/README.md),
[matrix](../../benchmarks/workload-matrix.json),
[thresholds](../../benchmarks/thresholds.md),
[hợp đồng native evidence](../testing/benchmark-native-evidence.md).

## Mô hình thực thi

Operator/agent chọn từng lệnh từ `run.py` sau khi đọc evidence của bước trước.
Mỗi bước ghi input, intent trước mutation, output/receipt và lỗi vào run directory
0700/file 0600 ngoài Git. Không cần `process.py init/run` hoặc recipe toàn ma trận.
`process.py` chỉ là adapter tùy chọn; checkpoint của nó phải bao gồm mọi file mà
lệnh độc lập yêu cầu review. Không tự approve, tự kéo dài cửa sổ hoặc tự tiếp tục
cleanup khi operator đang chọn các bước thủ công.

Sau `init`, dùng source đã freeze:

```bash
kit() { python3 "$RUN/source/deploy/acceptance/run.py" --run-dir "$RUN" "$@"; }
kit status
```

`RUN` là thư mục private tuyệt đối của lượt đã init. Lấy `RUN_ID` từ `run.json`.
Source sạch và dependency cần có trước khi init; không trộn binary khác commit,
không adopt installation/state đang được người khác sử dụng.

## G1 — Preflight, plan và duyệt riêng

Đăng nhập hai profile ở máy operator theo [hướng dẫn](../testing/focused-aws-operator-guide.md).
Chạy riêng `configure`, `init`, `assets`, `preflight`, `plan foundation`, `iam-review`.
Các lệnh read/plan không cho phép apply hoặc chi tiền.

```bash
kit direct-plan
kit benchmark-budget --inputs /absolute/private/benchmark-budget.json
```

Copy [budget inputs](../../deploy/acceptance/benchmark-budget.example.json) ra
ngoài Git, điền quantities và đơn giá Singapore có nguồn/ngày kiểm tra. Các giá
S3/DynamoDB có sẵn trong example là catalog lịch sử, `verified=false`; chưa phải
báo giá hiện hành. Giá chưa biết không được coi là 0. `benchmark-budget.json`
liệt kê đủ 60 slots/24 đối chứng, phí restore, bản phục hồi Standard, mọi retained
version, tối thiểu 90 ngày IR/Flexible, phụ phí metadata Flexible và các hạng mục
compute/network/metadata/monitoring/KMS/secrets/evidence. Không trừ credit/Free Tier.

Không tiếp tục full benchmark khi `status=UNKNOWN`, vượt envelope hoặc
`window_fit=false`. Example cố ý chưa đủ điều kiện phê duyệt. Dự toán thời gian là
assumption cần review, không phải duration đã đo. Mặc định kit 4 giờ; giới hạn
code hiện hành tối đa 12 giờ/50 USD; luôn giữ 90 phút cleanup. Không tăng các giới
hạn này trong thay đổi này. Standard Flexible restore thường kéo dài hơn một
trial 20 phút; timeout giữ trạng thái failed/incomplete, không sửa thành PASS.
Nếu cần chia nhiều lượt độc lập hoặc thay đổi cách chờ restore, duyệt kế hoạch và
chi phí đó trước; chưa có cơ chế tự gia hạn một installation qua nhiều paid windows.

Đọc plan, IAM validations/simulations và chi phí. Khi chủ account đã duyệt, chạy
lệnh approval cụ thể rồi lệnh thực hiện; approval không tự thực hiện mutation:

```bash
kit approve --action iam-activate --confirm-run "$RUN_ID"
kit iam-activate --confirm-run "$RUN_ID"
kit approve --action apply:foundation --confirm-run "$RUN_ID"
kit apply foundation --confirm-run "$RUN_ID"
```

Receipt bind run/source/config và SHA-256 của từng artifact; thay nội dung hoặc
quá 45 phút thì phải review lại. Quyền deploy/root vẫn theo kit; không dùng root
để chạy workload. Các IAM template thay đổi chỉ được render/review, không tự apply.

## G2 — Provision, bootstrap và smoke riêng

Lần lượt `plan hosts` → duyệt `apply:hosts` → `apply hosts` → duyệt `install` →
`install`; duyệt `material`/`bootstrap` trước từng lệnh; `plan active` → duyệt
`apply:active` → `apply active`. Không apply lại khi đã có intent chưa reconcile.
Kiểm HTTPS/ALB/clock/readiness và source trước smoke. Lệnh `trial --concurrency 1`
là focused smoke riêng, cần approval `trial`; nó không thay ma trận benchmark.

Bucket đối chứng có lifecycle riêng:

```bash
kit approve --action direct-provision --confirm-run "$RUN_ID"
kit direct-provision --confirm-run "$RUN_ID"
```

Tên bucket có account + hash run; IAM render đúng ARN đó. Không dùng bucket
`pilot-direct` cũ. Tạo bucket, versioning, SSE-KMS cùng runtime key, public block,
TLS-only policy và tags của run; không tự adopt tài nguyên đã tồn tại. Lost ACK
hoặc partial configuration giữ intent/ack để operator reconcile, không tự create
hoặc delete lại dựa vào một lần HEAD 403/404.

## G3 — Operator chọn từng Warm case/repetition

```bash
kit approve --action benchmark-case --confirm-run "$RUN_ID"
kit benchmark-case --case-id warm-cloud-c1 --repetition 1 --confirm-run "$RUN_ID"
kit collect --trial warm-cloud-c1-rep1
kit status
```

Operator xem artifact rồi mới chọn case/repetition tiếp theo. Không có loop tự
chạy 60 slots. Approval benchmark yêu cầu budget đầy đủ, source/matrix khớp,
cửa sổ đủ và envelope hợp lệ. Mỗi slot có intent riêng; slot đã dispatch không
được lặp mù, kể cả chưa có `result.json`.

Trước trial: domain khác giữa các slot, native Chunks scan chứng minh domain
chưa có dữ liệu; hai tenant chỉ chia sẻ domain trong đúng shared case. Cache
purge cluster có kết quả từng node và sample mới xác nhận memory/disk trống.
Membership phải khớp hai target Cloud healthy và Edge; không dùng ALB scrape
làm metric cả fleet. Bloom vẫn retained và phải công bố; không gọi đây là reset
Bloom hoặc backend hoàn toàn mới. Hash dataset/client/route/checksum/encryption
của cặp direct phải khớp. Không trộn Edge AWS VM với kết quả on-prem thật.

## G4 — Operator gọi Cold/Recovery riêng

Cùng `benchmark-case` với case ID Cold hoặc Recovery và repetition cụ thể; operator
quyết định khi nào dispatch. Không tự chạy G4 sau G3.

- Cold: inventory native toàn bucket gồm current/noncurrent versions, snapshot
  hai thời điểm, resolve manifest → chunk → generation để kiểm class physical.
  Logical storage-class header không thay bằng chứng pack đã transition.
- Recovery: fault chỉ đóng lease tổng hợp của trial (`lease-revoke`), không kill
  node hoặc sửa raw rows. Cut dùng worker R1/R2 có fencing và Chrony thật.
  Verifier kiểm native dispatch của fence cũ bị từ chối, sealed artifact chain,
  replay từ cùng genesis hai lần và sự hiện diện các object seed trong image.
  Không cài image đè metadata live. Đây không phải nghiệm thu node-crash/PITR.
- Các phép đo còn thiếu như PUT revival/UploadPart/protection-window và expiry
  thật không được suy ra từ Restore+GET. `coldbench` chỉ chứng nhận phạm vi đọc,
  restore và inventory; receipt revival giữ false khi chưa exercise.

Nếu recipe yêu cầu toàn bộ Cold lifecycle mà thiếu proof revival, verdict của
case phải INCOMPLETE. Không nới điều kiện vì thiếu thời gian restore, mất session
hoặc provider lỗi. Evidence local/fake không thay native AWS evidence.

## G5 — Collect, review và teardown theo lệnh riêng

Dừng workload, thu `collect`, `shutdown` có approval riêng và stop proof.
Xem evidence trước khi chọn cleanup; không có timer tự xóa.

```bash
kit direct-cleanup-plan
kit approve --action direct-cleanup --confirm-run "$RUN_ID"
kit direct-cleanup --confirm-run "$RUN_ID"
```

Direct cleanup kiểm ownership, client quiescence, exact versions/delete markers
trong plan; versions mới hoặc partial errors thì dừng. Native account listing
xác nhận bucket biến mất; không coi AccessDenied là absence. Không restore IAM
khi direct resource còn unresolved.

Sau đó gọi riêng `freeze`, `snapshot`, `cleanup-review`; duyệt `cleanup-grant` và
`unprotect`; `cleanup-plan`; duyệt `cleanup` theo exact destroy plan rồi chạy.
Duyệt/chạy `finish`, sau đó `audit`, `logout`. Các bước cleanup hiện có vẫn kiểm
ownership/quiescence và giữ failed workload evidence. `final-absence-summary.json`
phải accepted, ghi rõ bootstrap/state giữ lại và pending deletion được duyệt.

## Bằng chứng và stop conditions

Giữ commit/build/SDK/config/dataset hashes, slot status, raw request samples,
full SHA-256, route/Region/encryption, per-node samples + cache reset/job ACK,
physical snapshots, native cut/probe/checkpoint receipts, chi phí dự tính/quan
sát và cleanup audit. Threshold vẫn provisional; không tạo số đo hoặc approvals.

AccessDenied, SHA mismatch, unknown mutation, sai scope/ownership, drift, thiếu
node/sample, reset không hoàn tất, clock lỗi, thiếu ngân sách hoặc gần reserve
cleanup: dừng bước hiện tại, giữ FAIL/INCOMPLETE và reconcile bằng exact IDs.
Không force unlock, tự retry mutation, tự đổi clock hay tự sửa tags để nhận tài
nguyên của lượt khác. Merge/CI xanh không tự đồng nghĩa G1–G5 đã nghiệm thu.
