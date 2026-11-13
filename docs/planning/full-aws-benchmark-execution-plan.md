# Kế hoạch benchmark AWS theo từng bước do operator quyết định

**Trạng thái: chuẩn bị offline; chưa chạy AWS cho thay đổi này.** Ma trận
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
```

Operator chọn mức chi khoảng **10–20 USD**; config mặc định `max_usd=20`, từ chối
vượt 20. Không cần model dự toán hoặc điền giá chi tiết. `cost-review.json` ghi
mode `operator-envelope`, estimate null; không gọi Pricing API để mở gate.
Đây không phải hard cap billing hoặc cam kết đủ tiền cho cả 60 slots. Theo dõi
chi phí thực tế, dừng workload và cleanup khi gần envelope. Mặc định cửa sổ 4 giờ,
tối đa 12 giờ, luôn giữ 90 phút cleanup; không tự gia hạn cửa sổ đang chạy.
Cold restore có thể kéo dài nhiều giờ: request/status riêng và refresh AWS
session giữa các phase; deadline của installation vẫn giữ nguyên. Nếu restore
chưa ready trước cleanup reserve, giữ evidence incomplete rồi teardown theo plan.

Đọc plan, IAM validations/simulations và envelope. Khi chủ account đã duyệt, chạy
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
chạy 60 slots. Approval benchmark yêu cầu source/matrix khớp,
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

Recovery vẫn chạy một `benchmark-case` riêng theo ID/repetition. Fault chỉ đóng
lease tổng hợp của trial (`lease-revoke`), không kill node hoặc sửa raw rows.
Cut dùng worker R1/R2 có fencing và Chrony thật. Verifier kiểm stale fence bị từ
chối, sealed chain, replay từ cùng genesis hai lần, seed objects và full GET SHA.
Không cài image đè metadata live; không phải nghiệm thu node-crash/PITR.

Cold gồm các bước độc lập, không tự chạy nối tiếp:

1. Trên installation acceptance dành riêng, lưu policy gốc rồi dùng `dsgctl policy
   set --file patch.json --expected-revision N` qua tunnel để tắt `tiering.enabled`
   và `tiering.auto_restore`. Patch là `{"tiering":{"enabled":false,"auto_restore":false}}`.
   Review CAS trước khi gọi; không thay policy installation đang phục vụ người khác.
2. `benchmark-case --case-id cold-cloud-flexible --repetition 1 --confirm-run …`
   tạo tenant/bucket và seed **Warm**, native inspect; kết quả mới là prepared.
3. `cold-status` đọc snapshot/protection. Chờ UTC thật đến khi transition_ready;
   không sleep giữ lệnh hoặc tua clock. Trong lúc chờ, operator tự chọn việc khác.
4. Mỗi stage: `cold-plan` → đọc `cold-plan.json` → approval `cold-step` → `cold-step`.
   Lần lượt `transition`, `put-cold`, `part-cold`, `restore-request`.
   Sau transition, gọi `cold-status` đến khi metadata + S3 class đúng trước PUT.
5. Restore request trả waiting ngay. Gọi `cold-status` sau để xem ready; không
   request restore lại. Khi ready chạy `read`, `put-restored`, `part-restored`,
   `protection`, `finish`. IR bỏ request/status restore và Restored phases;
   sau transition/reuse PUT/part, chạy `read`, `finish`.
6. Mỗi slot có case-verdict riêng, không suy PASS từ prepare hoặc read/restore.
   Sau khi mọi Cold slot kết thúc và không còn job active, restore policy gốc bằng
   CAS qua `dsgctl`; policy cũ đã thay đổi bởi người khác thì dừng để đối soát.

Ví dụ một stage, sau khi đã có slot prepared:

```bash
kit cold-status --case-id cold-cloud-flexible --repetition 1
kit cold-plan --case-id cold-cloud-flexible --repetition 1 --stage transition
# Đọc cold-plan.json: exact packs/generations, policy revision và snapshot.
kit approve --action cold-step --confirm-run "$RUN_ID"
kit cold-step --case-id cold-cloud-flexible --repetition 1 --stage transition --confirm-run "$RUN_ID"
```

Job tiering pin revision bật cho exact packs; kit restore policy disabled bằng
CAS trước enqueue, rồi job dùng revision lịch sử. Chỉ tiếp tục data phase sau native S3/metadata readback.
Protection test cần native deferred checkpoint, cancel exact jobs và Warm state
không đổi. Không tự xóa dữ liệu Cold cũ để làm đẹp retained-byte measurement.
`cold-plan --stage policy-restore` phục hồi patch tạm của kit khi có ACK và policy
vẫn đúng revision kit sở hữu; mất ACK CAS/job phải inspect trước, không overwrite.

Mất SSH ACK: giữ nguyên slot và gọi `cold-reconcile` để lấy receipt của đúng segment;
không chạy lại `benchmark-case`, không reseed. Receipt failed/thiếu hoặc unknown
policy/job không tự thành PASS; cần đối soát thủ công. Cold status chỉ quan sát,
không tự coi một pending write đã thành công. Fixture gần expiry không phải bằng
chứng đã chờ hết restore expiry thật trên AWS.

Cloud foreground trials và Cold data/job segments thu metrics/OS/Chrony trên cả
hai Cloud EC2 và Edge EC2 qua SSM document fixed, pin version/hash và installation tags. Kiểm G2
rằng SSM Agent hỗ trợ ENV_VAR, script đúng release và sample đủ window trước khi
chạy matrix. Clock/OS thiếu một node, source/PID/boot đổi hoặc artifact bị cắt đều
fail; cache heartbeat không thay OS evidence. Glacier waiting không giữ sampler
chạy liên tục nhiều giờ và không tính thành throughput.

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
