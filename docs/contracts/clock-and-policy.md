# Đề xuất contract Clock và policy toàn hệ thống

**Task:** [W01-PHONG-01 — Contract Clock và policy toàn hệ thống](https://miles.extase.dev/tasks/W01-PHONG-01).  
**Owner:** Đinh Tuấn Phong. **Ngày đối chiếu:** 20/10/2026, UTC+7.  
**Trạng thái:** Draft để Hải/Hưng/Chiến review; chưa phê duyệt hoặc freeze interface/schema.  
**Baseline:** `main`, `0c98d50b6ef4846cb8b90b6ae4a07c7d5efb0366`.

## 1. Phạm vi, nguồn và cách đọc

Đây là deliverable tài liệu W1: thiết kế schema, semantics và boundary cases. Không có implementation W2, kết quả test runtime/simulator hay xác nhận task Done. Không thay Architecture/API nguồn; nếu review phát hiện mâu thuẫn thì ghi quyết định tại Open Questions trước khi đồng bộ nguồn bằng thay đổi riêng.

- **FACT / Nguồn:** yêu cầu quan sát trên Miles hoặc trong file nguồn.
- **ĐỀ XUẤT:** chi tiết thiết kế cần review, không tự trở thành hợp đồng đã duyệt.
- **UNKNOWN / Open Question (OQ):** nguồn chưa đủ chi tiết hoặc cần thống nhất cách diễn giải. Các phần phụ thuộc OQ chưa đủ điều kiện freeze.

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

**FACT:** chưa tìm thấy file schema/shared-interface độc lập hay Go types cho Clock/policy tại baseline. Contract hiện nằm trong KT §10.1/18 và API §6/10; `internal/clock`, `config`, `coordination`, `lifecycle`, `simulator` mới là placeholder. Các tên kiểu bên dưới là đề xuất, không phải API Go hiện hữu.

## 2. Đối chiếu nguyên văn task trên Miles

Đã đọc trực tiếp trang task ngày 20/10/2026. Task hiển thị Todo, Ready, checklist 0/3, không prerequisite và chưa có evidence ở thời điểm đối chiếu. Description: `internal/cache`, `internal/clock`, `internal/coordination`, `internal/lifecycle`, `internal/recovery`, `tests` theo nội dung task. Không cập nhật task.

| ID | Checklist nguyên văn | Phần đáp ứng trong tài liệu |
|---|---|---|
| C1 | Định nghĩa Clock thật/ảo và policy compression/chunking/tiering theo defaults kiến trúc 19.1. | §3–5, §7–8 |
| C2 | Bucket chỉ giữ cache admission/read-ahead; domain bất biến; policy update có revision/CAS. | §4–6, cases P01..P07 |
| C3 | Liệt kê deadline, lease/heartbeat/grace, restore margin, protection và clock skew phải đồng bộ. | §3, §5, §8, OQ-03..05 |

**Acceptance criteria nguyên văn:** “Có schema và boundary cases cho real clock/simulator; chưa nhân bản policy engine theo profile.”

§4–6 cung cấp schema/validation/CAS; §8 mô tả expected cases cho real/virtual Clock; §7 xác định một evaluator dùng chung. Thiết kế đủ để review không có nghĩa các case đã chạy hoặc chữ ký/types đã được chấp thuận.

## 3. Clock abstraction

### 3.1 Hai miền thời gian

**Nguồn:** KT §19.1 yêu cầu deadline local dùng monotonic clock; API §2.2.3 yêu cầu tuổi auth snapshot đo trên node đọc, không nhận snapshot node khác. Timestamp metadata/restore cần so sánh qua node và tồn tại qua restart.

| Giá trị | Miền thời gian / lifetime | Quy tắc đề xuất |
|---|---|---|
| `created_at`, `committed_at`, `cut_at`, `restore_expiry`, `protected_until`, retention timestamps | Wall-clock UTC, persist được | Dùng cho metadata/backend/audit; phải có nguồn và độ chính xác, không chứa monotonic origin |
| Request elapsed deadline, local lease-use budget, auth snapshot age, close wait budget, retry delay | Monotonic, local Clock domain | Không serialize/chuyển node; không reset khi wall clock nhảy |
| Persisted lease expiry | Shared wall timestamp + metadata lease/fence/incarnation | Không tự tạo quyền publish chỉ vì local timer chưa hết; cần condition-check authoritative lease |
| `recovery_epoch`, R1 fence, policy revision | Logical identifiers/counters, không phải thời gian | Không thay thế nhau; không so thứ tự commit bằng wall time |

**ĐỀ XUẤT:** diễn đạt Clock bằng một sample gồm hai thành phần được lấy cùng thời điểm logic:

```text
Clock.Now() -> Reading { wall_utc: Timestamp, instant: Instant }
Clock.Sleep(cancellation, duration) -> completed | cancelled
Instant.ElapsedSince(earlier) -> Duration | incompatible_clock_domain
Instant.Add(duration) -> Instant | overflow
```

Đây là chữ ký mô tả, chưa là mã Go triển khai. Giữ ý nghĩa `Now`/`Sleep` của KT §18; thêm kiểu opaque để tránh trộn UTC với elapsed time. Go types/package/signature cụ thể chờ OQ-01.

`Instant` chỉ so sánh được trong cùng process/Clock domain. Restart hoặc một virtual run mới tạo domain mới; không phục hồi local deadline bằng cách deserialize Instant. `Timestamp` đề xuất serialize UTC RFC3339 có fractional seconds; precision/rounding phải được Hải/Chiến chốt (OQ-02). So sánh dùng giá trị thời gian, không dùng chuỗi hiển thị.

### 3.2 Real Clock

**ĐỀ XUẤT:** adapter thật lấy wall/monotonic reading; chờ có cancellation; đánh thức trễ không được hiểu là timer còn hợp lệ. Mỗi wakeup phải đọc lại thời gian và kiểm tra state/fence trước hành động.

- Hard deadline cố định tại admission bằng `start.instant + request_budget`; idle activity, retry, heartbeat và wall clock jump không gia hạn deadline đó.
- Cancellation, drain hoặc mất lease có thể kết thúc sớm. Hết deadline không rollback transaction đã commit; outcome chưa rõ phải reconcile bằng op_id/receipt/outbox theo KT §9.0.
- Không gọi `time.Now`, `time.Sleep`, timer/ticker thật trong evaluator; adapter/runtime chịu trách nhiệm đưa reading vào.
- Trong hiện thực Go W2, phải bảo toàn monotonic semantics; timestamp parse/serialize không được coi như còn local monotonic origin. Không lấy wall subtraction làm elapsed deadline.

### 3.3 Virtual Clock và deterministic simulator

**ĐỀ XUẤT:** fake Clock do test điều khiển; simulator dùng cùng contract với event scheduler. `Sleep` chỉ đăng ký wakeup, không tự nhảy global time theo từng caller. Scheduler là bên duy nhất `AdvanceBy`/`AdvanceTo` và drain các sự kiện đến hạn.

| Tình huống | Semantics đề xuất |
|---|---|
| Advance tiến | Monotonic instant tăng; wall tăng cùng delta trong chế độ bình thường |
| Giả lập NTP/clock jump | `SetWallForTest` đổi wall riêng; instant không lùi/nhảy theo |
| Advance ngược hoặc overflow | Reject; không âm thầm wrap/clamp |
| Sleep âm/0 | Âm reject; 0 là yield/check cancellation, không tự advance |
| Events cùng instant | Stable order theo `(due_instant, insertion_sequence)`; seed/trace/order lưu trong evidence |
| Advance lớn | Chạy các scheduled events theo due time; không chỉ nhảy tới cuối rồi bỏ heartbeat/expiry/close events |
| Restart mô phỏng | Giữ persisted wall metadata, tạo Clock domain mới; không resurrect lease/read-view cũ |

Simulator không được dùng goroutine scheduling của host làm thứ tự quyết định. Dùng virtual deadlines/cancellation cùng scheduler; không dùng `context.WithTimeout` dựa timer thật làm nguồn hết hạn của virtual run. Runtime có thể bridge sang context thật ở adapter. Scheduler phải xử lý vòng lặp sự kiện không tiến thời gian bằng lỗi chẩn đoán, không treo vô hạn; giới hạn guard là OQ-01.

Tính deterministic có nghĩa: cùng input state/revisions, trace, start wall, seed, scheduler order và evaluator version thì cùng decision log. Không đồng nghĩa latency AWS hay thời gian restore thật được tái hiện chính xác.

### 3.4 Quy đổi thời gian local sang shared timestamps

**ĐỀ XUẤT:** tại admission giữ cặp `(wall0, instant0)` và hard elapsed deadline. `deadline_wall_estimate = wall0 + request_budget` chỉ là ước lượng cho metadata. Không serialize Instant và không để wall jump kéo dài local request.

Khi quyết định trên `restore_expiry`/`protected_until` hoặc shared lease, cần tính cả clock uncertainty, backend precision và elapsed time giữa sample/commit. OQ-03 chặn freeze công thức distributed lease; OQ-04 chặn freeze phép quy đổi protection/restore. Trong lúc chưa có proof không được dùng ước lượng để mở admission hoặc xóa dữ liệu sát biên. Fail readiness khi skew vượt bound là yêu cầu nguồn, không thay thế proof cho skew còn trong bound.

## 4. Schema và scope policy

### 4.1 SystemConfig và snapshot toàn hệ thống

**Nguồn:** KT §10.1: `SystemConfig` khóa `config_id`, giữ policy/default limits, revision, recovery history/epoch và service incarnation. API §6.2–6.3 chỉ mở policy tiering/compression/chunking; không cấp quyền sửa recovery identity bằng `/policy`.

**ĐỀ XUẤT logical model, chưa freeze storage encoding:**

```text
SystemConfig record(s)
  config_id: stable identity                    [value/partitioning: OQ-02]
  revision: integer revision                    [revision scope: OQ-02]
  policy: GlobalPolicy
  runtime_limits: TimingAndLimits               [internal, not new /policy fields]
  recovery_control: history_id, recovery_epoch, service_incarnation

GlobalPolicy
  tiering: enabled, warm_storage_class, cold_storage_class,
           cold_after_days, min_pack_live_ratio,
           auto_restore, auto_restore_days, auto_restore_tier
  compression: enabled, codec, level, min_saving_ratio
  chunking: min_bytes, avg_bytes, max_bytes
```

Đây không phải yêu cầu nhét tất cả vào cùng DynamoDB item. Recovery epoch cập nhật thường xuyên không được vô tình làm policy CAS conflict hoặc thay job policy revision (OQ-02). Phân biệt `schema_version` (định dạng, nếu được review bổ sung) với `revision` (mỗi cấu hình đã commit); không tự thêm field API chưa có trong nguồn.

### 4.2 Global policy fields/defaults

Nguồn defaults: KT §1.6/19.1 và API §6.3/13.3. Đơn vị byte là KiB/MiB; ngày/thời lượng cần quy ước ở OQ-05.

| Field | Type logical | Default nguồn | Validation nguồn / đề xuất cần review |
|---|---|---|---|
| `tiering.enabled` | boolean | `true` | Boolean đúng type |
| `tiering.warm_storage_class` | enum string | `STANDARD` | API cho `STANDARD`, `STANDARD_IA` |
| `tiering.cold_storage_class` | enum string | `GLACIER_IR` | P1: `GLACIER_IR`, `GLACIER`; `DEEP_ARCHIVE` là P2, không tự bật |
| `tiering.cold_after_days` | integer | `30` | Đề xuất không âm; 0/upper bound cần OQ-05 |
| `tiering.min_pack_live_ratio` | number | `0.8` | Đề xuất finite, trong [0,1]; equality đủ điều kiện nếu các gates khác pass |
| `tiering.auto_restore` | boolean | `true` | Không bỏ qua preflight hoặc expiry khi tắt |
| `tiering.auto_restore_days` | integer | `1` | Days phải dương theo API §4.4; cap chưa chốt |
| `tiering.auto_restore_tier` | enum string | `Standard` | Default nguồn; allowlist theo class chờ OQ-05 |
| `compression.enabled` | boolean | `true` theo API §6.3, KT mô tả nén mặc định | False không sửa codec chunk cũ |
| `compression.codec` | string | `zstd` | Raw là kết quả fallback lưu chunk, không tự mở enum codec API mới |
| `compression.level` | integer | `3` | Supported range theo thư viện sẽ chọn; OQ-05 |
| `compression.min_saving_ratio` | number | `0.1` | Nguồn: threshold hợp lệ; đề xuất finite [0,1), bounds chờ OQ-05 |
| `chunking.min_bytes` | integer bytes | `262144` | Đề xuất >0; nguồn min≤avg≤max |
| `chunking.avg_bytes` | integer bytes | `1048576` | FastCDC constraints bổ sung cần Hải review |
| `chunking.max_bytes` | integer bytes | `4194304` | Không overflow; cap và thuật toán validation chờ OQ-05 |

Nén nếu `stored_len <= raw_len * (1 - min_saving_ratio)` theo KT §3; không đạt thì lưu `codec=raw`. Đề xuất biểu diễn ratio nội bộ bằng fixed-point/rational đã chuẩn hóa để runtime/simulator không lệch do rounding (OQ-05). API vẫn giữ JSON number theo nguồn; không tự đổi wire type.

### 4.3 Bucket cache policy

**Nguồn:** KT §1.5–1.6, API §6.1–6.3. Bucket giữ owner, `bucket_id`, immutable `dedup_domain` và cache policy; domain không phải field mutable của cache policy.

```json
{
  "expected_revision": 1,
  "expected_bucket_id": "bucket-demo-001",
  "cache": { "admit_on_write": true, "readahead_chunks": 4 }
}
```

| Field | Type | Default | Validation / semantics |
|---|---|---|---|
| `cache.admit_on_write` | boolean | `true` | Cache admission không là data permission/durability |
| `cache.readahead_chunks` | integer | `4` | Đề xuất >=0, 0 tắt prefetch; cap byte/chunk cần OQ-05 |
| `expected_revision` | integer CAS token | Không có default | Phải cung cấp hoặc dùng If-Match theo API §6.1 |
| `expected_bucket_id` | bucket incarnation identity | Không có default | Bắt buộc để stale request không sửa bucket cùng tên đã tái tạo |

Reject bucket fields `tiering`, `compression`, `chunking`, `dedup_domain`, global runtime limits và mọi unknown/privilege field; không silently ignore. Caller không chọn lại domain qua policy. A ghi vào bucket B dùng domain của B. Cache vật lý có thể dùng chung; cache policy không tạo security domain. Owner/admin mới sửa bucket cache; system admin mới sửa global policy.

### 4.4 Validation pipeline

**ĐỀ XUẤT:** strict decode/type/duplicate-field handling → unknown-field/scope validation → field/cross-field validation → load authoritative revision/identity → CAS cùng quyền/gate/outbox cần thiết → publish immutable snapshot. Duplicate JSON members, null/omitted và replace/merge cần OQ-06; không tự suy từ zero-value Go.

Reject NaN/infinity (khi đi qua internal model), integer overflow, duration overflow và unsupported enums. Toàn bộ candidate phải hợp lệ trước mutation; lỗi không được ghi một phần fields hoặc tăng revision. Default chỉ materialize theo quy tắc đã review, không reset field cũ do request thiếu trường.

## 5. TimingAndLimits và các default liên quan

**FACT:** bảng này truy vết KT §19.1/API §13.3. Tên nhóm `TimingAndLimits` là đề xuất internal; không đồng nghĩa `/admin/v1/policy` nhận các field này. Route/quyền cập nhật limits chưa được nguồn mô tả đầy đủ (OQ-07).

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

**ĐỀ XUẤT validation liên trường:** durations an toàn phải dương; heartbeat < lease, nhưng inequality này một mình chưa bảo đảm renewal dưới network pause; grace không cộng vào local worker lease; tăng request budget phải đồng bộ restore margin/protection/drain/client/proxy timeout. Snapshot request/job không tự kéo dài khi config đổi. Không giảm retention làm mất supported cuts hoặc hạ protection đang còn hiệu lực; migration/config rollout cần OQ-07.

## 6. Revision, CAS và hiệu lực thay đổi

**Nguồn:** API §6.1: expected_revision hoặc If-Match; thiếu trả 400; conflict revision/state trả 409. Bucket thêm expected_bucket_id. KT §9.0: authoritative SystemConfig/Buckets changes có outbox cùng transaction/CAS. API §6.3: job giữ revision lúc tạo hoặc fail revision unavailable.

**ĐỀ XUẤT chuỗi cập nhật:**

1. Authorize đúng scope; parse và validate cả candidate, không mutation trong validation.
2. Đọc policy revision hiện hành và bucket incarnation khi có. Reject field vượt scope.
3. CAS expected revision/identity cùng điều kiện quyền/gate và authoritative outbox event; một người thắng trong hai updates cùng revision.
4. Snapshot sau commit bất biến; đề xuất revision tăng một lần cho update thành công. Scope revision, no-op và unknown outcome ở OQ-02/06.
5. Runtime chọn snapshot cho request/job, lưu revision trong context/decision/evidence. Job dùng pinned revision; không trộn fields từ nhiều revision.
6. New/revived chunks áp compression/chunking mới; chunk cũ đọc theo stored codec/identity. Không tự rewrite toàn kho hoặc đổi bucket domain.

| Trường hợp | Hành vi nguồn / phần cần review |
|---|---|
| Không expected revision/If-Match | 400 theo API |
| Sai revision | 409, không partial update; reread trước khi quyết định retry |
| Bucket tên cũ nhưng bucket_id khác | Không được áp update; exact identity error mapping OQ-06 |
| Thiếu/invalid bearer, thiếu role | 401 / 403 theo API; không suy quyền từ cache |
| Unknown/global field trên bucket | 400 schema sai; không silently ignore |
| If-Match và body cùng có revision | Không tự chọn một bên; mismatch precedence OQ-06 |
| Commit response mất | Không tự retry với revision mới như cùng một operation; outcome/retry protocol OQ-06 |

Policy snapshot không thay authorization snapshot: read auth tại admission; writes condition-check quyền hiện hành tại publish theo API §2.2.3. Re-evaluate an toàn pack/location/pins trước action dù decision dùng pinned policy. Global policy revision không là R1 fence hay recovery epoch.

## 7. Một policy evaluator cho runtime và simulator

**Nguồn:** KT §13.3 yêu cầu package policy tách biệt, dùng chung cho simulator và Tiering Engine. Cloud/Edge cùng core contract.

**ĐỀ XUẤT data contract:**

```text
Evaluate(policy_snapshot, validated_time_inputs, operation,
         pack_state, access_stats, safety_facts) -> Decision

Decision: action, reason, policy_revision, required_protection,
          restore_requirement, facts_to_revalidate
```

Evaluator là hàm quyết định không I/O: không S3/DDB, không random, không sleep hay lấy host clock, không tự cấp lease/quyền. Inputs immutable và explicit về units/time domain; incomplete/unknown safety facts không trả quyết định destructive allow. Decisions đề xuất gồm readable/reject/request_restore/revive_from_body/eligible/defer với reason; tên enum cuối cùng chưa freeze.

```text
Runtime Cloud/Edge: RealClock + authoritative metadata/backend observations
                       -> chung evaluator -> R1/CAS + AWS executor
Simulator: VirtualClock + trace + model backend state
                       -> chung evaluator -> simulated executor/cost model
```

Rules dùng chung: IR readable; archive GET cần restored đủ margin; PUT/part archive hoặc mọi Restored dùng client-body revival; tiering chỉ khi đủ age/live ratio và không protection/pins/roots; codec fallback theo saving threshold. Auth, fence và final metadata conditions vẫn thuộc runtime transaction, không được bypass vì evaluator đã trả eligible.

Baseline A/B/C của simulator có mô hình riêng theo KT §13.3; policy D (DSG) phải là evaluator runtime dùng. Model restore completion/latency, network/cost và fault timeline không bị trộn vào evaluator. Cloud/Edge chỉ đổi observation/executor/topology, không fork luật. Cùng state/time/revision thì decision phải giống nhau giữa runtime adapter và simulator.

## 8. Boundary cases để triển khai/kiểm chứng W2

**Tất cả case dưới đây là thiết kế test, chưa chạy.** `ε` là một tick chính xác đã chốt sau OQ-02; không dùng epsilon floating-point tùy ý. Chạy cùng decision vectors trên fake/virtual Clock và real adapter khi phù hợp. Real tests dùng controllable clock source/synchronization, không sleep đúng mốc rồi đòi scheduler host đạt precision tuyệt đối; AWS timing là evidence khác.

Ký hiệu: **N** = kết quả bắt buộc theo nguồn; **ĐX** = lựa chọn proposed; **OQ** = expected chưa đủ để freeze. Fixture phải giữ nhãn, không biến expected đề xuất thành source fact.

| ID | Input/biên | Expected và nguồn |
|---|---|---|
| T01 | Request tại D−ε, D, D+ε | ĐX: còn budget trước D; từ D không bắt đầu publish/emit tiếp; equality chốt OQ-03. Transaction đã gửi/unknown outcome cần reconcile, không giả rollback |
| T02 | Wall nhảy ±1h, elapsed chỉ tăng 1s | N: local deadline/auth age không tăng/giảm theo wall jump; shared expiry phải qua uncertainty checks |
| T03 | Restart hoặc so Instant hai Clock domains | ĐX: reject comparison; không phục hồi request/lease rights từ local ticks cũ |
| T04 | Sleep cancel trước wake, zero, negative, overflow | ĐX: cancelled không hành động tiếp; zero không advance; âm/overflow reject |
| T05 | Virtual nhiều wakeups cùng instant; replay cùng trace/seed | ĐX: stable ordering, decision log giống nhau; backward advance reject |
| T06 | Advance 5 phút qua nhiều heartbeat/export events | ĐX: scheduler chạy từng due event, không bỏ intermediate events; không tự giả renewal thành công |
| L01 | Lease tại E−ε, E, E+ε; renew tới sau E | N: phải renew trước expiry; worker đã mất lease không publish/stream. ĐX equality expired; distributed uncertainty OQ-03 |
| L02 | Heartbeat 15s nhưng worker pause quá lease60s | N: lịch heartbeat không là proof lease; tỉnh lại phải kiểm fence/state, không tiếp tục vì timer mới chạy |
| L03 | E+5s−ε, E+5s, E+5s+ε | N: grace5s chỉ reclamation; không giây nào sau mất lease cấp thêm worker rights. Reclaim vẫn kiểm roots/fence/unknown action; equality OQ-03 |
| L04 | Old fence/incarnation, timer còn hoặc clock lùi | N: reject stale action; R1 fence không bằng recovery_epoch; S3 late completion reconcile durable state |
| S01 | Node skew ±(2s−ε), ±2s, ±(2s+ε) | N: vượt 2s fail readiness; đúng2s không fail chỉ vì bound. Health/skew unknown behavior OQ-03; không coi ready là bypass admission |
| S02 | Client SigV4 skew ±15m và vượt15m | N: API §2.2.2 nhận ≤15m, vượt trả RequestTimeTooSkewed; không dùng node bound2s thay client bound |
| R01 | Restore expiry X = Dwall+120s ±ε và bằng | N: đủ khi X≥Dwall+120s; thiếu reject/preflight và auto-restore nếu bật. Conversion/precision uncertainty OQ-04 |
| R02 | GLACIER_IR ở state Cold, hoặc GLACIER chưa restore nhưng cache hit | N: IR readable; archive không bypass preflight nhờ cache; thiếu restore trả InvalidObjectState trước header |
| R03 | PUT/part trùng Restored còn hạn rất lâu | N: revive từ client body, không reuse temporary restore; GET/Copy có rule khác |
| R04 | Restore hết hạn/backend lỗi sau header | N: abort stream/metric; không phát chunk chưa verify; không ghi XML lỗi thứ hai |
| W01 | Admission A, budget30m: D=A+30m, protected_until P | N: P≥D+15m; chỉ A+15m không đủ. Nếu commit gần D vẫn còn ≥15m; wall mapping OQ-04 |
| W02 | Tier candidate khi now=P−ε, P, P+ε | N: trước P không tier; pins/roots/retention vẫn chặn ở mọi mốc. ĐX equality hết time protection; chốt OQ-03/04 |
| W03 | Deadline config tăng giữa request; renewal/retry | ĐX: request giữ budget cũ; request mới nhận snapshot mới; không rút ngắn durable protection đã cấp. Rollout OQ-07 |
| A01 | Auth snapshot read_at+5s−ε, +5s, +5s+ε | N: chỉ dùng khi now<valid_until; bằng5s đọc lại. Hit không refresh tuổi; token expiry có thể làm hết sớm hơn |
| A02 | Revoke sau admission nhưng trước write publish | N: write transaction kiểm quyền hiện hành và fail; read đã admit có thể hết trong lease/deadline, không cắt chỉ vì revoke |
| C01 | Close wait120s−ε, 120s, +ε còn producer khỏe | N: không quá budget; bỏ close/nhả recovery gate, không kill PUT khỏe. Tie quiesce-vs-timeout OQ-03 |
| C02 | Export mỗi60s nhưng thiếu một membership event | N: không seal/không công bố complete cut; RPO dựa last sealed cut, không 60s |
| C03 | Recovery tại24h edge, buffer10m hết, anchor/pins còn cần | N: age không đủ cho delete; giữ mọi supported-cut/anchor roots. Target/window precision OQ-02/05 và R2 contract |
| P01 | Hai updates cùng expected revision R | N: CAS một winner, loser409, không partial fields; exact revision/no-op OQ-02/06 |
| P02 | Bucket tên giống nhưng incarnation mới | N: expected_bucket_id cũ không sửa policy mới; exact error OQ-06 |
| P03 | Bucket gửi tiering/compression/chunking/domain/unknown | N: reject; owner không tự nâng privilege hoặc đổi domain |
| P04 | min=avg=max; min>avg; zero/overflow chunk sizes | N: source nhận thứ tự min≤avg≤max; constraints FastCDC/zero/cap OQ-05, không giả source chỉ cần inequality |
| P05 | Stored size đúng90% raw tại saving0.1; vượt1 byte | N: dấu ≤ cho compress acceptance; numeric/rounding OQ-05; vượt threshold lưu raw |
| P06 | Live ratio0.8−ε, 0.8, 0.8+ε; age30d edge | N: live ratio≥0.8, safety gates luôn ưu tiên; day arithmetic/age equality OQ-05 |
| P07 | Omitted/null/zero field; readahead0; codec/level sai | OQ-05/06: schema phải có explicit verdict; không áp default từ zero-value gây reset update |
| P08 | Job tạo ở R, global policy đổi R+1 | N: job dùng pinned R hoặc fail revision unavailable; không lấy nửa R/nửa R+1 |
| P09 | Runtime/virtual/simulator cùng vector, đổi profile | N: chung evaluator DSG, decision giống; adapter/cost có thể khác; không claim actual AWS latency |

## 9. Open Questions và người review

Chưa xác nhận một mâu thuẫn giá trị default trực tiếp giữa KT §19.1 và API §13.3 trong phạm vi task. Có các khoảng trống/khác cách diễn đạt sau; không tự quyết định thay nguồn.

| ID | Điểm cần quyết định | Đề xuất để review / giới hạn hiện tại | Reviewer |
|---|---|---|---|
| OQ-01 | KT §18 chỉ ghi Now/Sleep; Go Clock types, cancellation/timer/scheduler APIs chưa có | Reading/opaque Instant, cancel-aware Sleep, scheduler-owned advance; deterministic tie order và guard cần thống nhất | Phong, Hưng; Hải review consumers |
| OQ-02 | Timestamp precision/rounding, config_id/record split và revision domain | UTC wire timestamp; policy revision tách ảnh hưởng recovery-control updates; không freeze schema/initial revision khi Hải chưa review | Hải, Chiến, Hưng |
| OQ-03 | Shared lease authority/skew uncertainty, equality expiry/grace, unknown skew, close-timeout tie | Local elapsed conservative; distributed fence/renew atomic; chưa có proof công thức lease giữa nodes. Không coi 2s readiness đủ chứng minh reclaim an toàn | Hải, Hưng; Chiến error mapping |
| OQ-04 | KT ghi protection sau commit≥15m và admission≥hard deadline+15m; chuyển wall/monotonic khi clock jump | Hai câu phù hợp khi commit≤D và mapping an toàn; cần proof uncertainty/rounding và rule recheck trước publish. Không giản lược thành admission+15m | Hải, Hưng, Chiến |
| OQ-05 | Range/cap của zstd level, days, ratios, readahead; FastCDC constraints; day arithmetic; restore tier theo class | Bounds bảng §4 là đề xuất khi nguồn chưa chốt; byte/day/ratio encoding cần frozen vectors. AWS minimum duration/pricing không tự thành literal policy age | Hải, Hưng; Chiến wire validation |
| OQ-06 | PUT replace/merge, omitted/null/duplicate fields, body+If-Match precedence, initial/no-op revision, retry unknown outcome, bucket_id error | API example bỏ vài fields nhưng không định nghĩa update semantics; cần quyết định để không reset defaults hoặc double increment. Không tự nhận partial PUT là PATCH | Chiến, Hải; Hưng audit |
| OQ-07 | Timing limits mutable ở đâu, revision snapshot cho request, rollout qua nodes/config retention | Giữ internal, không tự mở /policy fields; cần đồng bộ timeout/protection/restore/drain. Không giảm bảo vệ resource/cut còn hiệu lực | Hưng, Hải, Chiến |
| OQ-08 | Package placement và full policy snapshot retention | Đề xuất package policy riêng đúng KT §13.3, Clock riêng; không tạo evaluator trong từng profile. Ai lưu revision cũ để job replay/fail rõ? | Hải, Hưng, Chiến |

Review W1 nên ghi accepted/rejected/changes requested theo từng OQ và nguồn liên quan; unresolved chỉ chặn phần schema/behavior tương ứng, không tự đánh task Done hay tự sửa Architecture/API. R1 full state/sequence và R2 format/roots sẽ bàn giao trong W01-PHONG-02/03, không tuyên bố đã hoàn tất ở tài liệu này.

## 10. Tự review và handoff

| Mục | Kết quả tự review tài liệu | Evidence / việc còn lại |
|---|---|---|
| C1 | Đã mô tả real/virtual Clock, time domains và global fields/defaults | §3–5/7; interface signatures/validation details chờ reviewers, chưa có W2 code |
| C2 | Đã phân biệt bucket cache/global/domain, revision/CAS | §4/6; PUT semantics và revision scope là OQ-02/06 |
| C3 | Đã liệt kê timing và quan hệ phải đồng bộ | §3/5/8; distributed uncertainty OQ-03/04 cần chốt trước freeze |
| Acceptance: schema | Có logical schema, API fields/units/defaults/validation; đề xuất được gắn nhãn | §4–6; chưa là schema approved/implemented |
| Acceptance: boundary cases | Có expected/source/proposed/OQ cho real/virtual/simulator | §8; mọi test case chưa chạy |
| Acceptance: shared evaluator | Một evaluator, explicit inputs, runtime/simulator adapters riêng | §7/P09; package placement OQ-08 |
| D1-01/02 | Task/source/scope/deliverable được truy vết trong bản draft | §1–2; cần review độc lập, không đồng nghĩa đủ đóng task |
| D1-03 | Self-review nội dung: defaults, exact checklist, scope, links và OQ | Kiểm cơ học: 4 local links tồn tại, 10 code fences cân bằng, 33 boundary cases; hashes ba nguồn khớp §1. Không nhận Go/CI pass |
| D1-04/05 | PR/CI/approval và handoff chưa xác minh | Chờ người dùng kiểm tra; không commit/push/PR, không sửa Miles |

**Evidence cần bổ sung sau review:** revision/hash deliverable, reviewer và kết quả từng OQ, source consistency quyết định cuối, consumer handoff Hải/Hưng/Chiến. W2 sau đó mới hiện thực Clock/evaluator và chạy vectors §8; mọi kết quả thật cần command/config/version/expected/actual và giới hạn theo DoD §6.

**Validation tài liệu đã thực hiện trong phiên 20/10/2026:** đọc task bằng browser; đọc nguồn bằng PowerShell `Get-Content`/`rg`; kiểm local-link targets bằng `Test-Path`, đếm fences/case IDs bằng regex; đối chiếu `Get-FileHash -Algorithm SHA256`; kiểm `git status --short` và `git diff --stat`. Working tree ban đầu sạch; sau tạo chỉ có tài liệu mới trong `docs/contracts/`. Đây là self-review tại workspace, chưa có reviewer độc lập, CI, runtime tests, AWS tests, commit hoặc PR; không cập nhật Miles.
