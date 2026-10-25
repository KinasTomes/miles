# Coordination prototype — W02-PHONG-03

Owner: Phong. Trạng thái **PROPOSAL / chưa commit, chưa freeze**.
Task Miles đã đọc: C1 fake coordination/read pins/fence/producer admission;
C2 event/descriptor/membership/seal/checkpoint golden cases; C3 failpoints.

## Phạm vi và bằng chứng

`read.go` là mô hình trong RAM, không MetadataStore thứ hai và không cung cấp
authorization. Caller/test driver phải thiết lập authorization và các gate độc
lập trước khi sử dụng. Không nối fake này vào production reader.

- `Acquire`: resolving guard, stable view ID, object/bucket incarnation và local
  monotonic use deadline. Không suy UTC uncertainty từ wall clock hoặc skew.
- `BindRoot`: CAS exact head revision/manifest cùng attach root và detach guard.
  Conflict giữ guard. Root không tự pin generation.
- `Pin`: batch có giới hạn explicit, same ID/input idempotent, all-or-none;
  condition-check canonical location revision và exact PackRef/generation.
- `Move` mô phỏng compaction CAS; old pin còn hiệu lực, stale attach bị reject.
- `Retire` chỉ kiểm interlock với guard/pins; logical reachability, retention,
  protection timestamps và R2 roots vẫn là prerequisites do driver thiết lập.
- `BeginWork`/`FinishWork`: token private, bản copy chia sẻ lifecycle; finish
  idempotent. `ValidateWork` phải chạy lại trước emit; không thay verification.
- `BeginRelease` đóng attachment; `FinishRelease` chỉ bỏ pins/root sau work=0.
  Deadline equality/restart/bucket invalidation dừng authority nhưng giữ roots
  và pins đến cleanup. Không có late renewal hồi sinh view.
- `AcquireWithLease`/`Renew` tách hard deadline và local lease budget; renewal
  CAS revision không kéo dài request deadline. Lost tại equality không resurrect.

Mutex chỉ giữ quanh state transition, không giữ suốt read/I/O. LP là mutation
dưới mutex trong mô hình; tests không chứng minh distributed correctness.

`producer.go` mô phỏng **một epoch**: Open → Closing → Closed. Closing không
nhận producer mới nhưng cohort đã admit còn attach mutation khi authority hợp
lệ. Fence closure và publication serialize cùng mutex. Dispatch trước expiry
có thể commit sau expiry nếu trước closure; committed outcome không bị đổi
thành aborted. Unknown chưa reconcile chặn FinishClose. AbortClose không mở
lại old fence. Stable IDs là receipts, không cấp quyền dispatch mới khi retry.
Mutation IDs unique trong toàn cohort, không chỉ mỗi producer. `Takeover` cần
old closure + mọi outcome reconciled + grace eligibility; Closing/Closed không
cấp new authority. `InvalidateAuthority` đóng old fences, giữ committed outcomes.
`ClosedMembers` trả bản copy sorted của exact committed membership.

## Fault injection

`Faults.Run` đặt Before/After quanh Registration, S3Write, Publication,
EpochClose, SealPublish, PinAttach và ReadRelease. Hook chạy ngoài coordinator
mutex; dùng channels để điều khiển ordering, không dùng sleep.

Before-error không gọi effect; After-error giữ effect và trả ErrUnknown cùng
nguyên nhân. Caller phải reconcile trước dùng handle hoặc giải phóng protection.
Đây là explicit driver; không cài failpoints vào production S3/Badger. Các
tests lost commit/pin/release ACK dùng operations thật của model.
`internal/recovery/fault_pipeline_test.go` nối producer model với immutable fake
S3 journal sink và seal publisher tham chiếu. Tests ngắt Before/After cả năm
loại operation, và từng page/descriptor/membership/seal write; resume cùng ID,
không nhân đôi committed effect, không publish seal trước Closed và exact
membership/durable payload verification. Đây là exporter model, không cloud service.

## Recovery golden candidate

`tests/fixtures/recovery/candidate.json` chứa exact body bytes, byte lengths,
SHA-256 cho một page, descriptor, membership, seal và historical checkpoint.
Hash được chuẩn bị bằng Python hashlib, đối chiếu trong Go crypto/sha256.
Subset dùng ASCII và integer đơn giản; **không phải implementation RFC8785**.
`internal/recovery/candidate_test.go` kiểm chain references, missing/truncated
page, checksum, chain gap, descriptor/checkpoint mismatch và no partial replay.

`journal.json` bổ sung hai epochs, event nhiều page, event-ID formula đúng W1,
membership + predecessor hash chain và historical checkpoint với tombstone.
Reference decoder kiểm required/unknown fields, duplicates, version, length,
hash, bounded ASCII/integer canonical subset; tests rehash ancestors sau semantic
corruption để tránh chỉ bắt lỗi checksum. Replay kiểm atomic bundle, duplicate
idempotency/conflict, revision ordering, missing pages/chain và checkpoint equality.

Ba checklist đã có implementation/reference deliverables và tests để review ở
phạm vi W2. **Format freeze vẫn cần consumer approval**, không tự đánh dấu task
Done. Candidate không bao phủ production full entity inventory, arbitrary Unicode
JCS, hierarchical trees, catalog/markers hoặc recovery security normalization.
Không thay MILESPAG kinds 1–4. Schema/evidence chi tiết tại
`docs/testing/coordination-recovery-evidence.md`.

## Giới hạn còn lại / review gate

- C1 có renew/takeover model và race tests; durable closed membership, multi-node
  shared-time proof, production incarnation transfer vẫn thuộc integration sau.
- Lost ACK là trạng thái quan sát của Faults driver; không có durable pending
  registry. Không truyền receipt chưa reconcile sang consumer như authority.
- Không tích hợp UTCProof/EvaluateRead: đây là local state-machine harness;
  production consumers phải dùng evaluator chung, không lấy fake làm proof.
- Token/receipt maps được giữ cho test run; chưa có bounded retention/cleanup
  service. Không dùng như long-running production coordinator.
- Không tuyên bố C1/C2/C3 Done, recovery thật hoàn tất hoặc API Frozen.
