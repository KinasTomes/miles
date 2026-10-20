# Đề xuất contract Clock và policy toàn hệ thống

**Task:** [W01-PHONG-01 — Contract Clock và policy toàn hệ thống](https://miles.extase.dev/tasks/W01-PHONG-01).
**Owner:** Đinh Tuấn Phong. **Ngày đối chiếu:** 20/10/2026, UTC+7.
**Trạng thái:** Quyết định thiết kế W1 đã chốt theo yêu cầu người dùng; phần deferred chưa đủ điều kiện freeze implementation/consumer. Không xác nhận task Done.
**Baseline sửa đổi:** `0046f6d7ea859b099dbd07b7a49575efe01eb478`. Nguồn task được đọc ngày 20/10/2026; không coi snapshot Miles đó là trạng thái live mới.

## 1. Phạm vi, nguồn và cách đọc

Đây là deliverable tài liệu W1: decision, schema, semantics và test design. Không có implementation W2, kết quả test runtime/AWS/benchmark hoặc proof R1 hoàn chỉnh. Giữ defaults Architecture §19.1, API v2 và R1/R2; chỉ sửa tài liệu này. §9 ghi dependency đồng bộ Architecture/API trước freeze consumer khi clarification bổ sung hoặc thu hẹp nguồn.

- **FACT / Nguồn:** yêu cầu quan sát trên Miles hoặc trong file nguồn.
- **W1 decision / accepted in W1:** lựa chọn DSG được người dùng chốt trong lần sửa này; không phải kết quả đã đo/chạy.
- **Deferred:** protocol/proof/implementation còn lại có task, owner và freeze gate tại §9. Giữ ID OQ-01..08 để truy vết lịch sử, không để quyết định W1 tiếp tục mơ hồ.

Mục tiêu scale là một Clock/evaluator dùng chung Cloud/Edge/simulator, snapshot bất biến được cache trong node, update policy không tranh chấp heartbeat/recovery epoch hoặc thêm metadata round trip mỗi chunk. Đây là thiết kế DSG, chưa phải cam kết hiệu năng.

| Ref | Nguồn tại baseline | Phạm vi sử dụng |
|---|---|---|
| KT | [Architecture v4](../dedup-storage-gateway-architecture.md) §1.4–1.6, §5.1–5.2, §6–9, §10.1–10.2, §13.3, §18–19.2 | Global/bucket scope, Clock, R1/R2, shared models, defaults |
| API | [API v2](../dedup-storage-gateway-api.md) §2.2–2.2.3, §2.6, §4.2–4.4, §6.1–6.3, §7–8, §10, §13.3 | Auth time, wire fields, CAS/error semantics, jobs và readiness |
| DoD | [Definition of Done](../acceptance/definition-of-done.md) §1–2, D2-09..12/14/16, §6 | D1 document review; evidence và giới hạn kết luận |
| Scope | [Scope/priorities](../acceptance/scope-and-priorities.md) §4–5 | Phân công và handoff W1/W2 |

SHA-256 nguồn đã đọc (không phải hash của deliverable này):

| File | SHA-256 |
|---|---|
| `docs/dedup-storage-gateway-architecture.md` | `7CC1D1CABC47FD346991285609B12934AA5EDFBDC22B409B0B842FF7E33D9FBC` |
| `docs/dedup-storage-gateway-api.md` | `F15611FA06548A6682F05CD2BA55B3502BE695F535AF948E254F63EA9695F205` |
| `docs/acceptance/definition-of-done.md` | `A424B3FD1E9F0354D5E07F22C50F6CCA76A883E8CFE9906108DABA029644F5B5` |

**FACT:** chưa có Go Clock/policy implementation tại baseline. Contract nguồn nằm trong KT §10.1/18 và API §6/10; `internal/clock`, `config`, `coordination`, `lifecycle`, `simulator` là placeholder. Chữ ký mô tả dưới đây không phải implementation Go hiện hữu.

## 2. Đối chiếu nguyên văn task trên Miles

Đã đọc trực tiếp trang task ngày 20/10/2026. Task hiển thị Todo, Ready, checklist 0/3, không prerequisite và chưa có evidence ở thời điểm đối chiếu. Description: `internal/cache`, `internal/clock`, `internal/coordination`, `internal/lifecycle`, `internal/recovery`, `tests` theo nội dung task. Không cập nhật task.

| ID | Checklist nguyên văn | Phần đáp ứng trong tài liệu |
|---|---|---|
| C1 | Định nghĩa Clock thật/ảo và policy compression/chunking/tiering theo defaults kiến trúc 19.1. | §3–5, §7–8 |
| C2 | Bucket chỉ giữ cache admission/read-ahead; domain bất biến; policy update có revision/CAS. | §4–6, cases P01..P07 |
| C3 | Liệt kê deadline, lease/heartbeat/grace, restore margin, protection và clock skew phải đồng bộ. | §3, §5, §8, OQ-03..05 |

**Acceptance criteria nguyên văn:** “Có schema và boundary cases cho real clock/simulator; chưa nhân bản policy engine theo profile.”

§4–6 cung cấp schema/validation/CAS; §8 mô tả expected cases cho real/virtual Clock; §7 xác định evaluator dùng chung. W1 decisions đã chốt; concrete types, proofs và implementation deferred tại §9. Không case nào được coi là đã chạy.

## 3. Clock abstraction — accepted in W1 (OQ-01/03/04)

### 3.1 Miền thời gian và contract

Nguồn KT §18/19.1, API §2.2.3. Package `internal/clock` sở hữu contract duy nhất sau (chữ ký mô tả, không code W2):

```text
Clock.Now() -> Reading { wall_utc: Timestamp, instant: Instant }
Clock.Sleep(ctx, duration: time.Duration) -> error
Instant.Add(time.Duration) -> Instant | overflow
Instant.Sub(Instant) -> time.Duration | incompatible_domain | overflow
Instant.Compare(Instant) -> order | incompatible_domain
```

`Instant` opaque; Add/Sub/compare chỉ trong cùng Clock domain. Không serialize, chuyển node, dùng lại sau restart hoặc lấy wall subtraction làm elapsed deadline. `time.Duration` phải kiểm overflow cả parse, convert, cộng/trừ/nhân và horizon arithmetic. Không mở thêm production timer/ticker/scheduler interface trong W1. Concrete Go types, scheduler implementation và guard value thuộc W02-PHONG-01/03.

| Giá trị | Miền/lifetime | Contract |
|---|---|---|
| created/committed/cut timestamps, restore expiry, protected_until, retention | Wall UTC, persist được | Wire RFC3339Nano, normalize UTC, precision logic tối đa ns; so parsed values, không so chuỗi |
| Request deadline, auth snapshot age, local lease-use budget, close wait, retry delay | Monotonic trong Clock domain | Không đổi theo wall jump; không serialize local ticks |
| Lease expiry E | Shared UTC với proof và metadata owner/fence/incarnation | Không suy quyền từ một timestamp client hoặc timer đơn lẻ |
| Policy revision, R1 fence, recovery_epoch | Logical identifiers | Không hoán đổi; commit order không dựa node wall time |

Real adapter giữ monotonic reading riêng trước khi format/normalize UTC. `UTC`/serialization không được làm mất nguồn elapsed time đang dùng; [Go time: Monotonic Clocks](https://pkg.go.dev/time#hdr-Monotonic_Clocks) mô tả các thao tác strip monotonic component. Không thay toolchain trong task này.

### 3.2 Real/virtual cancellation và expiry

- `now.instant >= hard_deadline_instant` là expired. Hard deadline chốt lúc admission; idle traffic, retry, renewal hoặc config mới không gia hạn nó.
- Sleep âm reject; zero kiểm cancellation rồi yield, không advance. Dương chờ deadline/cancellation. Khi cancel và timer cùng ready, kiểm lại cancellation/deadline trước side effect. Wakeup không cấp quyền thực hiện action.
- Mất lease dừng local stream và dispatch side effect mới; read đã admit không cắt chỉ vì revoke, nhưng vẫn chịu deadline/lease. Transaction dispatch hợp lệ trước lease expiry có thể commit/settle sau expiry theo shared fence closure ordering, không bị timer rollback; unknown outcome phải reconcile bằng op_id/outbox/receipt trước takeover cấp new publication authority. Xem [R1 §8](r1-and-cache.md#8-lease-fencing-takeover-và-reconciliation).
- Sau process suspend/resume, invalidate lease-use assumptions, revalidate/reacquire theo R1 trước tiếp tục. Không mặc định host monotonic timer phản ánh suspend như mong muốn.

### 3.3 Virtual scheduler và deterministic simulator

Scheduler là bên duy nhất advance; Sleep chỉ đăng ký wakeup. Chạy events theo `(due_instant, insertion_sequence)`; cùng inputs/seed/order phải cùng decision log. Advance lớn chạy mọi due event theo thứ tự, không bỏ heartbeat/expiry/export ở giữa. Backward/overflow advance reject. Test có thể đổi wall riêng nhưng Instant không lùi theo wall.

Zero-time event loop có guard hữu hạn và lỗi chẩn đoán; không tự chế giá trị guard W1. Virtual deadline/cancellation dùng scheduler, không host goroutine order hoặc timer thật/context timeout thật. Restart simulator tạo Clock domain mới, không phục hồi local lease rights. Khi quiesce và close-budget đồng thời đến hạn nhưng chưa có xác nhận quiesce, timeout thắng: bỏ close, nhả gate theo protocol, không kill writer khỏe; không xóa roots/gates của operation còn unknown outcome.

### 3.4 Shared UTC uncertainty và lease safety

Mọi shared-time decision dùng khoảng UTC có kiểm chứng `[L,U]` bao được thời gian thực, gồm clock error, sample age, precision và drift trong horizon cần bảo vệ. Đây là invariant DSG, không phải proof chrony/runtime đã cung cấp được bound. Không coi node skew 2s là toàn bộ uncertainty hoặc cộng 2s một lần đủ cho hai nodes.

- Lease valid trước E, expired tại E. Chỉ dùng lease-dependent rights nếu chứng minh `U < E` cùng owner/revision/fence/service incarnation hợp lệ. Không renew để resurrect quyền đã mất.
- Chỉ bắt đầu xét reclaim khi `L >= E + grace`; equality chỉ đủ time eligibility. Vẫn CAS state/fence và kiểm roots/pins/in-flight outcome. Grace5s không kéo dài quyền worker.
- Unsynced source, bound unknown/stale/hết horizon hoặc vi phạm source bound: fail readiness và từ chối admission/action cần proof. Không xóa/nhả roots/gates đang bảo vệ unknown outcome.
- Publish/renew/reclaim gắn đúng owner/revision/fence/incarnation. Timestamp client nhét vào condition không phải server-time check lúc commit. TTL là cleanup bất đồng bộ, không timer lease ([DynamoDB TTL](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/TTL.html)).
- **Đồng bộ quyết định Hưng W01-PHONG-02 Q6 — chờ xác minh tích hợp:** `U<E` cấp quyền dispatch side effect mới với lease/fence/incarnation hợp lệ, không yêu cầu transaction commit trước E. Mọi authoritative metadata publication kiểm shared fence closure và incarnation trong cùng transaction với mutation: LP trước closure giữ committed outcome kể cả sau E; LP sau closure fail fence condition. Takeover atomically close old authority, stabilize membership và reconcile mọi in-flight/unknown outcomes trước cấp new publication authority. Không dùng timeout/expiry làm no-commit; `L>=E+grace` vẫn chỉ reclaim eligibility, không extension.
- W01-PHONG-02 (Phong) sở hữu acquire/renew/takeover, cách hình thành E và local lease-use budget; proof target là không có hai publication authorities có thể publish chồng nhau, tách dispatch permission, closure ordering, reclaim eligibility và operation outcome. W02-PHONG-03 và W04-PHONG-03 hiện thực/fault-test. W1 Clock chỉ chốt invariants/equality, không nhận đã có proof protocol.
- S3 action unknown giữ durable Deleting/Transitioning theo KT §8.2; timestamp/fence metadata không hủy được request S3 đã gửi.

### 3.5 Protection và Restored admission

Tại sample hiện hành có bound hợp lệ trong horizon cần bảo vệ:

```text
remaining = max(0, hard_deadline_instant - now.instant)
D_hi = U + remaining
P_required = round_up(D_hi + 15 minutes)
P_new = max(P_old, P_required)
Restored GET/Copy admissible iff restore_expiry_lower_bound >= D_hi + 120 seconds
```

Arithmetic kiểm overflow. Expired/cancelled request không được admit chỉ vì remaining=0. P phải bền vững trước publish; không giảm do policy đổi/wall jump. Tier/delete chỉ qua time gate khi lower bound hiện hành `L >= P`; vẫn kiểm roots/pins/retention/fence/gate. Protection không đủ nếu chỉ tính admission+15m thay hard deadline+15m.

Restore equality hợp lệ; missing/stale/insufficient bound thì defer/revalidate, không suy allow từ `wall0+budget`. Re-evaluate trước action; mất proof thì dừng side effect mới và giữ protection/outcome cần reconcile. Expiry là backend observation được kiểm chứng, không `requested_at + Days`; [S3 restore status](https://docs.aws.amazon.com/AmazonS3/latest/userguide/restoring-objects.html#restore-status) cung cấp expiry bản restore. PUT/UploadPart vẫn revive mọi Restored từ client body.

Wire UTC RFC3339Nano với ε=1ns trong fake vectors không hứa real timer chính xác 1ns. Nếu backend/encoding tick thô hơn: protection round lên, expiry dùng làm quyền đọc round xuống; uncertainty/drift/sample age phải nằm trong bound. W02-PHONG-01 sở hữu time-source adapter/bound validation, W02-PHONG-03 harness; chưa có proof thì không bật destructive path phụ thuộc bound.

## 4. Schema và scope policy — accepted in W1 (OQ-02/05/06)

### 4.1 Logical records và revision domains

KT §10.1/18 là nguồn shared model. Tách các logical records/revision domains, có thể cùng bảng SystemConfig với key khác nhau; không tạo dịch vụ/config store mới:

```text
GlobalPolicyCurrent: policy_revision + immutable snapshot/current pointer
GlobalPolicyHistory[policy_revision]: immutable complete snapshot
DeploymentTimingLimits: limits_revision + validated startup limits
RecoveryControl: history_id, recovery_epoch, service_incarnation + own concurrency control
BucketCachePolicy: bucket_id + independent cache revision + cache fields
```

Heartbeat/counters/recovery epoch không tăng policy revision. Key encoding bàn giao W01-HAI-01 với consumer/model reviewer Hải; history encoding phải hỗ trợ roots/R2. Revision là integer `1..9007199254740991` (2^53−1), bootstrap1; không wrap. Global policy và bucket cache revision độc lập. Schema version, nếu cần trong format handoff, không thay revision.

### 4.2 Global fields, defaults và validation

Giữ defaults KT §19.1/API §13.3; API §6.3 cung cấp wire field names. Không tự thêm dynamic timing fields vào /policy.

| Field | Type | Default | Validation W1 / phần deferred |
|---|---|---|---|
| tiering.enabled | boolean | true | Strict boolean |
| tiering.warm_storage_class | enum | STANDARD | STANDARD hoặc STANDARD_IA |
| tiering.cold_storage_class | enum | GLACIER_IR | P1: GLACIER_IR hoặc GLACIER; DEEP_ARCHIVE P2 |
| tiering.cold_after_days | integer | 30 | >=0; 0 chỉ bỏ age gate; duration overflow reject; cap W02-PHONG-01 |
| tiering.min_pack_live_ratio | number | 0.8 | Finite [0,1]; exact decimal arithmetic |
| tiering.auto_restore | boolean | true | Không bypass expiry/preflight khi false |
| tiering.auto_restore_days | integer | 1 | >0; cap W02-PHONG-01; không tự tính backend expiry |
| tiering.auto_restore_tier | enum | Standard | Giữ P1/class-compatible allowlist nguồn; adapter/validator phải pin supported values trước freeze, không arbitrary string |
| compression.enabled | boolean | true | Không đổi codec chunk cũ |
| compression.codec | enum | zstd | Không tự mở codec mới; raw là stored fallback |
| compression.level | integer | 3 | Library-supported range theo W01-HAI-03; validator không nhận tùy ý khi chưa pin |
| compression.min_saving_ratio | number | 0.1 | Finite [0,1); exact decimal arithmetic |
| chunking.min_bytes | integer bytes | 262144 | >0, min<=avg<=max; thêm FastCDC/library constraints W01-HAI-03 |
| chunking.avg_bytes | integer bytes | 1048576 | Như trên; không bịa constraint từ benchmark |
| chunking.max_bytes | integer bytes | 4194304 | Như trên; bounded memory budget trước validator freeze |

Policy age: 1 ngày =24h UTC, không calendar/DST; đủ age khi age>=configured duration (cần conservative time proof nếu shared timestamps). Tách Days của restore request với expiry thực tế backend. Saving: `stored <= raw*(1-saving)`; live ratio: `live/total >= threshold`, total=0/unknown không chia và không tự eligible.

Normalize JSON decimal một lần lúc load snapshot thành số học chính xác; so nhân chéo có overflow checking, không epsilon float. JSON vẫn number như nguồn. Không gọi ns epsilon cho ratio; ratio vectors dùng rational ticks riêng. Integer/duration overflow, unknown/null/duplicate fields, wrong types/scope, unsupported enums reject trước mutation. Supported zstd/FastCDC thuộc W01-HAI-03 và implementation tương ứng (consumer hiện hành Chiến; Hải giữ model review). Memory/prefetch/day caps cụ thể thuộc W02-PHONG-01: pin supported range/budget và tests trước freeze; constraint chưa chốt không có nghĩa chấp nhận mọi giá trị. Caps DSG không được gọi là AWS limits.

### 4.3 Bucket cache policy

Nguồn KT §1.5–1.6 và API §6.1–6.3. Owner/admin sửa bucket cache; system admin sửa global policy. Bucket `dedup_domain` bất biến, không là mutable field; A ghi bucket B dùng domain B. Cache policy không tạo security domain hoặc data grant.

```json
{
  "expected_revision": 1,
  "expected_bucket_id": "bucket-demo-001",
  "cache": { "admit_on_write": true, "readahead_chunks": 4 }
}
```

| Field | Default | Contract |
|---|---|---|
| cache.admit_on_write | true | Boolean; false không chủ động admit từ write path, không tắt read cache; true best-effort khi final exact identity/physical bytes đầy đủ; không thay durability/quyền; R1 §11.1 |
| cache.readahead_chunks | 4 | Integer>=0; 4 chunk phía trước không tính demand (review Hưng); 0 tắt prefetch; cap memory/chunks W02-PHONG-01 |
| expected_revision | Không default | Hoặc If-Match; token rules §6 |
| expected_bucket_id | Không default | Bắt buộc và check incarnation cùng update |

Reject bucket tiering/compression/chunking/dedup_domain/global limits/privilege/unknown fields. Không silently ignore.

### 4.4 Candidate validation

Auth trước resource/revision disclosure. Strict parse duplicate/unknown/null/type/scope → đọc current snapshot → merge fields được gửi, kể cả nested → validate toàn candidate → atomic CAS. Omitted giữ nguyên; zero là giá trị explicit (nếu field cho phép), không trigger default. Defaults chỉ materialize bootstrap/create. Không thêm PATCH endpoint. Error không partial update/revision/history/event.

## 5. TimingAndLimits và các default liên quan

**FACT:** bảng này truy vết KT §19.1/API §13.3. `TimingAndLimits` là internal deployment snapshot riêng revision theo W1 decision; không hot-update qua `/admin/v1/policy`, không thêm endpoint. Validate startup và rollout compatibility/drain ở §7.3/§9.

| Tham số | Default nguồn | Miền / ràng buộc |
|---|---|---|
| Request hard deadline | 30 phút | Monotonic từ admission tới stream/commit |
| Lease / heartbeat / grace | 60s / 15s / 5s | Grace chỉ reclamation, không cấp thêm quyền worker |
| Node clock skew | ≤2s | >2s fail readiness; local deadline monotonic |
| Restore margin | Hard deadline còn lại +120s | GET/Copy Restored; expiry ≥ hard deadline +120s |
| Write protection | ≥15 phút sau commit | Admission bảo vệ đến ít nhất request hard deadline +15 phút |
| ALB idle / app idle / drain | 120s / 150s / 1800s | Idle không là hard deadline; Cloud hooks bao phủ drain |
| Authorization snapshot | Tối đa 5s | Local monotonic age, token expiry có thể cắt ngắn; write reauthorize tại publish |
| Active upload / Completed receipt | 24h từ Create / 7 ngày từ commit | Completing theo lease/recovery, không TTL mù quáng |
| Export / recovery cut+checkpoint cadence | 60s / mỗi giờ | Mục tiêu scheduling, không SLA RPO |
| Close-epoch wait budget | 120s | Chưa quiesce thì bỏ lần đóng, nhả gate; không hủy PUT khỏe |
| Supported recovery data | 24h +10 phút GC buffer | Cộng checkpoint anchor và mọi roots còn cần; PITR 1 ngày |
| Tombstone/outbox/journal retention | Recovery watermark | Không cleanup chỉ vì wall age/TTL |
| Object cap / single PUT hoặc part | 100 GiB / 5 GiB | 107374182400 / 5368709120 bytes |
| Streaming reservation growth | 64 MiB | Logical; known length reserve trước body |
| Active uploads / staging cap mỗi owner | 16 / logical_limit +100 GiB | Không lộ physical bytes |
| New-user quota | 1 TiB logical | Không là bucket compression/tiering policy |
| Bearer lifetime | 24h, tối đa 7 ngày | API §6.4 còn quy định request expires_in_seconds 300..604800 |
| Admin page/cursor | 100, cap1000 / 1h | Cursor bound principal/resource/filter/bucket_id |
| Presigned lifetime | Tối đa 7 ngày | API §2.2.2: 1..604800s; vẫn chịu revoke/grants |

**Các khoảng thời gian khác nguồn nêu:** client SigV4 skew ±15 phút (khác node skew 2s); readiness backend probe cache tối đa 5s (khác authorization snapshot). KT §7.1: IR/Flexible minimum retention 90 ngày, Standard-IA 30 ngày; DEEP_ARCHIVE 180 ngày chỉ tham chiếu P2. Không coi retention class là policy age, không bật Deep Archive P1, không hard-code giá AWS trong evaluator.

**W1 decision — validation liên trường:** durations an toàn phải dương; heartbeat < lease, nhưng inequality này một mình chưa bảo đảm renewal dưới network pause; grace không cộng vào local worker lease; tăng request budget phải đồng bộ restore margin/protection/drain/client/proxy timeout. Snapshot request/job không tự kéo dài khi config đổi. Không giảm retention làm mất supported cuts hoặc hạ protection đang còn hiệu lực; migration/config rollout theo §7.3, deferred tasks/gates OQ-07 tại §9.

## 6. Revision/CAS, update semantics và retry — accepted in W1

Nguồn API §6.1/6.3, KT §9.0. Các clarification dưới đây là W1 decision của DSG; API source chưa được sửa trong lượt này. Dependency SYNC-API ở §9 phải xong trước consumer freeze.

1. Giữ PUT hiện có; update các field được gửi, kể cả nested; omitted giữ nguyên. Merge current rồi validate toàn candidate. Không dùng Go zero-value reset defaults. Không thêm PATCH.
2. Auth trước khi tiết lộ resource/revision: bearer invalid401, role thiếu403. Unknown/duplicate/null/wrong-type/scope400. Bucket missing404, tên tồn tại nhưng expected_bucket_id sai409 incarnation conflict; identity condition vẫn bắt buộc cho update/no-op.
3. Thiếu cả expected_revision/If-Match400. Có cả hai phải cùng revision, khác400. If-Match là một strong ETag decimal revision, ví dụ `"3"`; không wildcard/weak/list. Tokens phải integer trong1..2^53−1. Current không khớp409 trước khi gọi candidate là no-op.
4. CAS hợp lệ + nội dung canonical không đổi:200, revision giữ nguyên, không thêm authoritative change event/history revision. No-op vẫn phải kiểm auth/identity/revision nguyên tử; không trả200 từ snapshot stale.
5. CAS hợp lệ + nội dung đổi: revision+1 đúng một lần, current snapshot/pointer, immutable history và authoritative event cập nhật transactionally; không partial update. Hai candidate khác nhau cùng R chỉ một winner đổi nội dung; không khẳng định mọi pair CAS kể cả no-op đều phải có loser409.
6. Hết revision domain mà cần change: lỗi rõ, không wrap; no-op hợp lệ ở max vẫn giữ revision. Wire code/message cho exhaustion phải pin cùng SYNC-API/validator trước freeze, không tự gán HTTP code ngoài nguồn trong tài liệu này.
7. Khi transaction Update một item, gộp conditions vào Update, không thêm ConditionCheck cùng item. Các fields/current/history/outbox cần atomicity phải nằm transaction phù hợp, không tách nhiều write rời ([DynamoDB transactions](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/transaction-apis.html)).
8. Mất response: retry nguyên body/revision token; nếu stale409 rồi đọc/reconcile. Không lấy revision mới và áp lại như cùng operation. Current trùng candidate không chứng minh chính caller đã commit; backend tra op_id/outbox khi cần. ClientRequestToken chỉ reuse với đúng transaction params, không receipt vĩnh viễn; KT §9.0 đã ghi giới hạn idempotency10 phút.

Ví dụ nested merge: current R3 có tiering enabled=true, cold class=GLACIER_IR, warm class=STANDARD, days30, auto_restore_days1; request `{"expected_revision":3,"tiering":{"cold_storage_class":"GLACIER"}}` tạo candidate R4 chỉ đổi cold class. Omitted enabled/warm/days/auto_restore_days và toàn compression/chunking giữ nguyên. Gửi lại nội dung tương đương ở expected4 là no-op200/R4; retry request expected3 sau commit trả409, không tự nhận lại response thành công cũ.

Policy snapshot không là auth snapshot: source API §2.2.3 vẫn kiểm quyền hiện hành tại publish. Compression/chunking mới chỉ áp chunk mới/revived; codec/domain cũ không tự rewrite. Policy revision không là R1 fence/recovery epoch.

## 7. Snapshot caching, shared evaluator và timing rollout — accepted in W1

### 7.1 Package/consumer boundary

| Package | Trách nhiệm |
|---|---|
| internal/clock | Reading, Instant, cancellation-aware Sleep |
| internal/config | Load/validate, immutable policy/limits snapshots, adapter/history integration |
| internal/policy | Evaluator thuần dùng chung, không I/O/clock thật/S3/DDB/random/sleep |
| internal/coordination | Lease/gate/fence/incarnation/proof protocol |
| internal/lifecycle | Revalidation và execution qua R1/AWS |

Không thêm interface/framework khi chưa có consumer thứ hai. Đây là package decision, chưa tạo package/code ở PR tài liệu này.

```text
Evaluate(policy_snapshot, validated_time_inputs, operation,
         pack_state, access_stats, safety_facts) -> Decision
Decision: action, reason, policy_revision, required_protection,
          restore_requirement, facts_to_revalidate
Runtime Cloud/Edge: RealClock + authoritative observations -> evaluator -> R1/CAS/AWS executor
Simulator DSG: VirtualClock + trace/model observations -> cùng evaluator -> modeled executor/cost
```

Evaluator không cấp quyền/lease; unknown safety facts không trả destructive allow. IR readable; archive GET cần restored proof; PUT/part revive mọi Restored từ body; tier eligibility cần age/live ratio và mọi protection/roots gates. State/pins/fence phải revalidate trước side effect. Simulator baselines A/B/C có mô hình riêng theo KT §13.3; baseline D dùng đúng evaluator DSG. Cùng state/time/revision thì Cloud/Edge/simulator cùng decision; không đồng nhất latency/cost model với số đo AWS.

### 7.2 Cache, history và policy propagation

Global revision là immutable snapshot cache theo revision trong node. Request pin snapshot đang active tại node lúc admission; dùng nguyên khối, không đọc SystemConfig mỗi chunk hoặc trộn revisions. Policy commit control plane không hứa mọi node áp tức thì; refresh/cache implementation W2 phải có evidence applied revision. Không làm policy CAS tranh chấp heartbeat/recovery epoch/counters.

Metadata/config adapter giữ immutable history theo key riêng; current pointer/snapshot, history và event update nguyên tử. Job pin revision cụ thể lúc tạo/resume; unavailable thì lỗi rõ trước execution, không fallback current. History còn được queued/running/cancelling/resumable jobs hoặc supported cuts tham chiếu phải giữ. Cleanup theo roots/watermark R2 hiện có; không list append vô hạn trong một item, không TTL mù quáng, không thêm background service riêng.

### 7.3 Deployment timing/limits

Giữ bảng defaults §5. Không mở hot update timing qua /policy hoặc endpoint mới. Limits snapshot validate startup; request/job/lease đang dùng giữ nguyên budget/limits revision. Policy update không đổi hard deadline/lease budget đang sống. Pin policy/limits revision trong context/evidence.

Thay timing cần rollout được review compatibility/drain; không rút ngắn protection/retention/roots đã cấp. Chưa chứng minh tương thích leases/cuts còn sống thì dừng rollout, không cho hai semantics âm thầm đồng tồn tại. Protocol/code rollout thuộc W2 và tasks ops, không tạo dynamic-config feature W1.

## 8. Expected vectors — test design, chưa chạy

**W1 chốt expected, không ghi pass implementation.** ε=1ns của fake logical time, không cam kết real timer1ns. Ratio dùng rational exact ticks; real adapter tests không sleep tại equality rồi đòi host scheduler tuyệt đối. Cases chạy với fake/virtual và adapter tương ứng W2; real AWS evidence thuộc phase sau. Vectors shared-time chỉ allow khi bound bao UTC trong cả horizon và vẫn hợp lệ tại revalidation.

| ID | Input/biên | Expected W1 |
|---|---|---|
| T01 | now=D−ε, D, D+ε | Local budget còn trước D; tại/sau D expired, không side effect mới; transaction gửi rồi không rollback vì timer |
| T02 | Wall jump ±1h; instant tăng1s | Local deadline/auth elapsed không đổi; revalidate shared proof, không derive allow từ wall0+budget |
| T03 | Instant khác domain/restart | Reject compare/Sub; không deserialize rights cũ |
| T04 | Sleep âm, zero, overflow; cancel+timer ready cùng lúc | Âm/overflow reject; zero check cancellation/yield không advance; recheck cancel/deadline trước action |
| T05 | Cùng due time/input/seed/order | Insertion sequence ổn định, decision log giống; backward advance reject |
| T06 | Advance5m; zero-time loop | Chạy intermediate events, không fake renewal thành công; loop đạt configured finite guard thì diagnostic error |
| L01 | U=E−ε, E, E+ε; owner/fence/incarnation đúng | Chỉ trước E đủ time proof; bằng/sau E không dùng rights; missing protocol proof vẫn không allow |
| L02 | Heartbeat15s nhưng pause/suspend | Không suy lease còn sống từ schedule; resume invalidate assumptions, revalidate/reacquire |
| L03 | L=E+5s−ε, E+5s, E+5s+ε | Trước grace chưa xét reclaim; bằng/sau chỉ time-eligible, vẫn CAS/roots/in-flight checks; không gia hạn worker rights |
| L04 | Old fence/incarnation; S3 action unknown | Reject stale new action, giữ durable Deleting/Transitioning để reconcile; timestamp không hủy S3 request |
| L05 | Valid dispatch U<E; metadata LP sau E trước closure / LP sau closure | Trước closure giữ committed outcome nếu conditions khác đạt; sau closure fail fence; reconcile mọi in-flight/unknown outcomes trước new publication authority, không strict commit-before-E |
| S01 | Node skew ±(2s−ε), ±2s, ±(2s+ε) | Vượt2s fail readiness; equality không fail chỉ vì bound; <=2s vẫn cần proof đầy đủ |
| S02 | Client SigV4 skew ±15m và vượt15m | API nhận <=15m; vượt RequestTimeTooSkewed; không dùng node bound2s thay client bound |
| S03 | Unsynced clock; stale/unknown [L,U]; horizon hết | Fail readiness và refuse time-proof admission/action; giữ unknown-outcome roots/gates |
| S04 | Fast/slow nodes có wall khác nhau, cùng validated interval bao UTC | Cùng safety inequalities; không dùng local wall đơn lẻ hoặc giả uncertainty chỉ2s |
| R01 | restore_expiry_lower_bound=D_hi+120s−ε, bằng, +ε | Dưới reject/defer/preflight; bằng/trên đủ time predicate, vẫn auth/pins/state checks |
| R02 | IR Cold; GLACIER chưa restore nhưng cache hit | IR readable; archive không bypass preflight, InvalidObjectState trước header khi thiếu restore |
| R03 | PUT/part trùng Restored còn hạn lâu | Revive client body, không reuse temporary copy |
| R04 | Backend expiry/failure sau header | Abort stream/metric; không phát unverified chunk hoặc ghi XML thứ hai |
| R05 | Requested Days=1 nhưng HEAD chưa có verified expiry | Không tự suy requested_at+24h; defer/restore progress theo backend |
| W01 | P=D_hi+15m−ε, bằng, +ε | Dưới không đủ publish protection; bằng/trên đủ predicate nếu durable; chỉ A+15m không đủ khi D sau A |
| W02 | L=P−ε, P, P+ε | Trước không qua time gate; bằng/trên chỉ time-eligible; roots/pins/retention/fence vẫn bắt buộc |
| W03 | Policy/wall đổi; P_old>P_required | P_new=max(old,required), không giảm; không đổi local deadline đang sống |
| W04 | Sample U tăng do age/drift trước action | Tính lại D_hi/requirements; proof stale hoặc protection thiếu thì revalidate/extend durable hoặc defer |
| W05 | Backend tick1s, P_required có fractional ns, expiry có fractional ns | P round lên giây kế; expiry lower bound round xuống; so lại inequalities, không round-to-nearest |
| W06 | Ví dụ sample U=12:00:02Z, remaining=28m | D_hi=12:28:02Z; P>=12:43:02Z; restored expiry lower bound>=12:30:02Z; sample/horizon invalid thì các số không cấp quyền |
| A01 | Auth read_at+5s−ε, +5s, +5s+ε | Chỉ trước valid_until dùng snapshot; bằng5s reread; hit không refresh; token expiry cắt sớm |
| A02 | Revoke trước write publish | Transaction kiểm quyền hiện hành; read đã admit có thể hết trong deadline/lease, không cắt chỉ vì revoke |
| C01 | Quiesce chưa xác nhận đúng close budget120s | Timeout thắng, bỏ close/nhả gate theo protocol; không kill healthy writer; không nhả bảo vệ unknown outcome |
| C02 | Export60s nhưng thiếu member/descriptor | Không seal, không complete cut; RPO theo last complete seal |
| C03 | Recovery24h boundary/buffer10m hết nhưng anchor/pins còn | Không delete chỉ theo tuổi; giữ supported cuts/roots; target validation theo R2 |
| P01 | Hai content-changing candidates khác nhau cùng R | Chỉ một change winner R+1; loser409, không partial current/history/outbox |
| P02 | Bucket missing hoặc tên giống/id khác | Sau auth: missing404; incarnation khác409; không sửa bucket mới |
| P03 | Bucket gửi global/unknown/privilege fields | 400; không domain mutation hoặc privilege elevation |
| P04 | Chunk zero/min>avg/overflow; min=avg=max | Invalid arithmetic/order reject; equality order chỉ qua gate W1, còn supported-library constraints phải pass |
| P05 | saving0.1, raw100, stored90/91 | Exact90 accepted compression,91 raw fallback; ratio out-of-range/nonfinite reject |
| P06 | live80,total100,threshold0.8; total0/unknown | Equality passes ratio gate; total0/unknown không chia/không tự eligible; safety gates vẫn ưu tiên |
| P07 | Omitted nested tiering field; null/duplicate | Omitted giữ current; null/duplicate400 trước mutation; không reset default |
| P08 | Job pinnedR nhưng active nodeR+1 | Job dùng R/resumeR; unavailable lỗi trước execution, không fallback |
| P09 | Cùng state/time/policy khác Cloud/Edge/simulator | Decision DSG giống; models/cost/latency không thành measured facts |
| P10 | Missing tokens; body3+If-Match4; weak/list/wildcard | 400; strong ETag duy nhất `"3"` hợp lệ nếu cùng body/current3 |
| P11 | No-op currentR/tokenR; stale tokenR−1 dù candidate giống current | CAS-valid no-op200/R, không change event; stale409, không bypass CAS vì equal content |
| P12 | No-op và content change cùng R, các interleavings | No-op linearize trước change có thể cả hai200; change trước no-op thì stale409. Không ép mọi CAS pair có một loser |
| P13 | Commit đổi R3->R4 nhưng response mất | Retry body/token3:409 nếu đã commit; reconcile op_id/outbox; current equal không chứng minh caller winner |
| P14 | Max revision2^53−1, no-op/change; token vượt max | No-op hợp lệ giữ max; change exhaustion lỗi rõ không wrap; invalid token400; pin exhaustion error mapping trước freeze |
| P15 | cold_after_days0, readahead0, day qua DST | Age gate bỏ nhưng protection/roots giữ; prefetch off; policy day=24h UTC không DST |
| P16 | Update cold class duy nhất như ví dụ §6 | Omitted nested/defaults và groups khác giữ nguyên; canonical candidate đổi đúng một field |
| P17 | History pinned bởi job/cut; config heartbeat/epoch đổi | History không cleanup; policy revision/cache snapshot không đổi vì heartbeat/epoch; không read per chunk |
| P18 | Request node activeR3 sau control plane commitR4 | Request pin nguyên R3; applied revision có evidence; không quảng cáo instant propagation; refresh atomic R4 cho admission sau khi apply |
| P19 | Ratio1/live hợp lệ; saving1/negative; large integer cross-product | Live1 trong range; saving1/negative reject; overflow reject hoặc exact arithmetic an toàn, không float epsilon |
| P20 | Source precision drift/rounding làm thiếu margin1tick | Không allow; rounding/uncertainty phải nằm bound và inequalities; không tự cộng2s để hợp thức proof |

## 9. Decision register, deferred work và freeze gates

**Accepted in W1** nghĩa decision/test design đã chốt theo chỉ đạo người dùng, không nghĩa code/proof/test đã pass. Không còn bỏ ngỏ W1 semantics OQ-01..08; phần deferred được truy vết dưới đây. Tên reviewer ở baseline không phải phân công consumer mới nhất.

| ID | Accepted in W1 | Deferred: task / owner | Điều kiện freeze phần deferred |
|---|---|---|---|
| OQ-01 | Reading/opaque Instant, Duration, ctx Sleep, equality, scheduler order/cancel/guard hữu hạn; internal/clock | W02-PHONG-01/03 — Phong; Hưng simulator consumer | Concrete Go types/cancellation/scheduler/guard pinned; deterministic/overflow/cancel vectors và adapter behavior verified |
| OQ-02 | Logical record/revision separation; positive JSON-safe revision/bootstrap1/no-op; UTC RFC3339Nano/ns/conservative rounding | W01-HAI-01 — shared model Hải với consumer hiện hành; W02-PHONG-01/03 — Phong adapter; W01-PHONG-03 — R2 history/roots | Key/encoding/atomic transaction model reviewed; no heartbeat/epoch policy conflict; model handoff và SYNC-KT hoàn tất trước storage consumer freeze |
| OQ-03 | Expired at E; U<E cho dispatch mới; L>=E+grace chỉ reclaim; publication theo atomic closure/incarnation, không strict commit-before-E; unknown proof fail closed | W01-PHONG-02 — Phong protocol; W02-PHONG-03/W04-PHONG-03 — Phong code/faults, metadata consumer review Hải/Chiến | Dispatch/local-budget và closure→stable membership→reconcile proof không overlap publication authorities; uncertainty horizon validated; late settlement/closure/suspend tests trước destructive enablement |
| OQ-04 | D_hi/current interval, durable max protection, restored lower bound, revalidation/rounding | W02-PHONG-01/03 — Phong time-source/bound/tests; W01-PHONG-02 — R1 proof; W01-PHONG-03 — roots | Adapter bound thực sự bao UTC/drift horizon; fast/slow/stale/rounding vectors verified; không bật destructive path khi chưa đủ proof |
| OQ-05 | Ratio ranges/exact arithmetic; positive ordered chunks; day24h UTC; zero age/read-ahead; existing defaults/allowlist | W01-HAI-03 — supported library contract (Chiến chunking/compression consumer hiện hành, Hải model review); W02-PHONG-01 — Phong caps/validator | Library-supported levels/FastCDC constraints, memory/prefetch/days caps và restore tier/class validation pinned; không nhận arbitrary values trước validator freeze |
| OQ-06 | Nested supplied-field PUT, null/duplicate400, token/identity errors, no-op/CAS/retry rules | W02-CHIEN-02 — Chiến Admin wire/error clarification; W02-PHONG-01 — Phong config merge; W01-HAI-01/model và W04-HAI-01/metadata consumer review | SYNC-API xong; exact exhaustion error, canonical equality, retry/current/history/outbox transaction contract và vectors thống nhất trước API consumer freeze |
| OQ-07 | Startup limits snapshot, no hot timing endpoint, pinned active budgets, compatibility/drain rollout | W02-PHONG-01 — Phong startup validation; W02-HUNG-03/W04-HUNG-01 — Hưng config/ops rollout consumers | Version/compatibility/drain protocol reviewed; evidence policy/limits revisions và existing leases/cuts an toàn; chưa chứng minh thì dừng rollout |
| OQ-08 | Package boundaries, one evaluator, node snapshot cache, history/jobs/cut retention | W02-PHONG-01/03 — Phong config/history contract; W04-HUNG-02 — Hưng jobs; W01-PHONG-03/W05-PHONG-03 — Phong roots/cleanup | History encoding/atomicity, job-resume exact revision, refresh applied-revision evidence, bounded-item storage và roots cleanup verified; SYNC-KT trước consumer freeze |

Task IDs là đường bàn giao cho phần việc tương ứng; không cập nhật assignee/status trên Miles và không khẳng định backlog đã được sửa. Handoff theo chỉ đạo hiện tại: **Phong** Clock/policy/R1/R2, **Chiến** Admin wire và chunking/compression hiện được phân công, **Hưng** jobs/simulator/ops; **Hải** giữ shared-model/metadata consumers cần review. Nếu task title baseline mang tên Hải, xác nhận consumer/owner hiện hành khi handoff, không coi tên cũ là quyết định phân công mới.

### 9.1 Dependency đồng bộ nguồn trước freeze

Chọn đường dependency thay vì sửa Architecture/API trong lượt này. Defaults/R1/R2 không đổi. Consumer không được freeze các clarification chỉ có trong file này khi nguồn/API expected chưa được đồng bộ:

| Gate | Nguồn cần đồng bộ | Nội dung bắt buộc / owner |
|---|---|---|
| SYNC-API | API §6.1/6.3, §10 và policy fixtures liên quan | Chiến phối hợp Phong: nested omitted merge, no-op, strong If-Match/token precedence, duplicate/null/type/scope,404/409 incarnation, JSON-safe revision/exhaustion và lost-response reconciliation. Nếu nguồn đồng bộ chọn expected khác phải sửa vectors/decision tương ứng trước freeze; không giữ hai expected |
| SYNC-KT | KT §10.1/18/19.2 và §13.3 khi cần | Phong/Hưng cùng model consumer Hải/Chiến: records/revision domains, immutable history/current/outbox, Clock/precision/equality/uncertainty contract, package/snapshot/cache/rollout semantics; giữ §19.1 defaults và protocol R1/R2 |
| SYNC-R1/R2 | W01-PHONG-02/03, KT §8–9/shared model handoff | Phong với metadata/jobs consumers: formal lease/non-overlap proof và history/cut roots, time-source assumptions; không coi Clock decision là proof runtime |

Không có runtime/AWS benchmark, policy-store service mới hoặc hoàn chỉnh protocol R1 được yêu cầu để viết W1 decision này. Proof/implementation/tests là gate của đúng downstream scope, không tự đánh W1 Done hay claim pass.

## 10. Tự review và handoff

| Mục | Kết quả self-review tài liệu | Giới hạn |
|---|---|---|
| C1 | Clock real/virtual/simulator, global defaults/schema, package/evaluator decisions rõ ở §3–5/7 | Concrete types/library constraints/bounds deferred có owner/gate §9 |
| C2 | Bucket cache-only, immutable domain, nested merge/CAS/no-op/retry/revision ở §4/6 | SYNC-API/SYNC-KT bắt buộc trước consumer freeze |
| C3 | Deadline/lease/heartbeat/grace/skew/margin/protection, uncertainty/equality/rounding ở §3/5/8 | Chưa có lease non-overlap proof hay time-source adapter verified |
| Acceptance: schema và boundary | §4–6 và vectors §8; ε1ns logic, exact ratio vectors | Test design, chưa chạy real/fake/runtime/AWS tests |
| Acceptance: shared engine | Một internal/policy evaluator, immutable node cache, pinned request/job/history | Chưa đo scale/metadata I/O/latency; refresh/history code deferred |
| OQ-01..08 | W1 decisions accepted; deferred task/owner/freeze cụ thể ở §9 | Không suy pass implementation từ accepted decision |
| D1-01/02 | Task/source/defaults và deliverable được truy vết | Review/handoff độc lập và task completion chưa xác minh |
| D1-03 | Kiểm tài liệu: 50 unique vectors, 10 code fences cân bằng, 4 local links tồn tại, 8 decision-register rows; git diff --check sạch, ba source hashes khớp §1 | Không nhận CI/Go/runtime pass |
| D1-04/05 | Chưa commit/push/PR hoặc cập nhật Miles | Người dùng kiểm tra trước commit; consumer handoff/SYNC pending |

Lần tạo tài liệu trước ghi nhận 33 vectors và4 local links; đây là lịch sử baseline, không số đếm phiên sửa. Phiên sửa chỉ thay file này, đọc lại baseline0046f6d và nguồn không đổi; kiểm final diff/links/fences/unique case IDs/source hashes bằng thao tác tài liệu. Evidence tương lai cần task/build/config/source revision, command/expected/actual, reviewer và giới hạn theo DoD §6. Không đưa secret vào evidence.
