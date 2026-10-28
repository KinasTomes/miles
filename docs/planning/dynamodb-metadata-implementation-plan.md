# Kế hoạch triển khai DynamoDB MetadataStore và atomic commit/quota

Task [W04-HAI-01](https://miles.extase.dev/tasks/W04-HAI-01), Tô Ngọc Hải.
Kế hoạch ban đầu ngày 28/10/2026, Asia/Saigon. Nội dung baseline dưới đây ghi lại
thời điểm trước triển khai; trạng thái implementation hiện tại và các giới hạn
được cập nhật trong [validation](../testing/dynamodb-metadata-validation.md).

Đã triển khai bốn nhóm checklist: native adapter, atomic publication, durable
unknown-outcome reconciliation và Local contract/consumer handoff/CI. Ba commit
đầu lần lượt là `567f6bb`, `30dc30e`, `edc9877`; commit chứa cập nhật này hoàn tất
nhóm acceptance. Không push hoặc thay status board trong lượt triển khai.

Điều chỉnh thiết kế sau khi kiểm tra consumer: Warm reader nhận MetadataStore
thay concrete Badger; outbox discovery giữ Limit+1 candidates nhưng quét base
table do inventory không có GSI. Object/bucket/pack listings vẫn gom candidate
view trong bộ nhớ. Reconciliation dùng op row nguyên tử + saved exact request và
CAS retirement; không gọi nhiều GetItem rồi coi chúng là coherent snapshot.
Pending request phải fit full op item. Các giới hạn này được ghi rõ trong
validation, không đổi thành claim scale/AWS/R1 production readiness.

## 1. Readiness và nhánh làm việc

Board hiện ghi **Todo / Ready / checklist 0/4**. Hai hard prerequisites đều Done:
W01-PHONG-03 (R2 epoch/retention design), W03-HAI-03 (storage/schema handoff).
Không có bình luận task hoặc linked PR mới tại thời điểm đọc.

Đã fast-forward main và tạo nhánh:

```text
codex/w04-hai-01-dynamodb-metadata
base: aa219407850dd6178e3e8906e10599daf2a4e1c3
```

Main đã chứa PR #35, #32 bucket API và #34 exporter/recovery prototype.
Untracked notes hiện có được giữ nguyên. Chưa commit/push, đổi status board,
thay source hoặc provision AWS trong bước lập kế hoạch này.

Baseline vừa chạy (không dùng test cache):

```text
go test ./internal/metadata/... ./internal/storage/contracttest -count=1
badger PASS; dynamodb preparation PASS; shared contracttest PASS
```

Docker CLI và Java có trên máy. Docker Linux Engine hiện không kết nối được
(`dockerDesktopLinuxEngine` pipe chưa tồn tại). Trước DDB Local integration cần
khởi động Docker Desktop Linux engine và kiểm API readiness, hoặc dùng official
DDB Local JAR tương thích JRE. Chưa chạy container/JAR hoặc DDB Local suite.

## 2. Kết quả phải đạt và ranh giới task

Bốn checklist gốc:

1. MetadataStore trên DDB: batch/consistent reads, CAS revision,
   quota/reservation/staging conditions.
2. Atomic head/tombstone/outbox/receipt; stable op_id và exact-parameter token retry.
3. Timeout/unknown/throttle reconciliation trước retry/release; không lặp action
   cho cùng item.
4. Shared Badger/DDB Local contract suite và item/transaction caps.

DoD: head visibility và counters đúng khi conflict/retry; các consumer multipart/
control plane có backend thật. Cần implement toàn bộ interface hiện hành và
các extension thực sự được consumer dùng, không tạo Store đầy các ErrUnsupported
rồi coi checklist đã hoàn tất.

HAI-01 bao gồm final CAS/receipt projection tối thiểu của Completion hiện hành.
HAI-02 mở rộng Uploads/Parts/freeze/takeover APIs, completion streams kinds 3/4
và producer coverage mọi control entity. Không viết HTTP multipart orchestration,
stats/layout, Cloud deployment hoặc toàn bộ R1/R2 exporter/sealing trong HAI-01.
Tuy nhiên, mutation HAI-01 phải có đúng outbox/outcome/authority effects; không
defer tính atomic của chính nó sang HAI-02.

## 3. Code hiện có và phần cần bổ sung

| File/area | Vai trò hiện có | Cần bổ sung |
|---|---|---|
| internal/storage/store.go | Shared interfaces và consistency/error contract | Review minimal extension cho prepare/authority, không duplicate port |
| internal/metadata/dynamodb/api.go | 6 SDK operations, official client assertion | Store/Options và runtime/local wiring |
| schema.go | 20 tables, object/chunk/pack/page/op/owner keys | Typed record bindings/validation, explicit logical->physical inventory |
| items.go | Head, page kinds1/2, dev envelope, outcome result, receipt attributes | Decoders + full op/ledger/authority/owner records; complete final postimages |
| limits.go | Native item/action/aggregate validation | Final builder integration, expression/wire accounting |
| badger/commit.go, reservation.go | Working atomic dev behavior và retry contracts | Reimplement bằng DDB conditions; không copy Badger snapshot algorithm |
| contracttest/metadata.go | Reusable suite với seed/fault hooks | DDB Local factory, before/after-dispatch fault wrapper |
| scripts/local_test.py | DDB Local/Moto API smoke và isolated dummy credentials | Adapter contract job trên đủ inventory; smoke CLI không thay adapter proof |

Đặc biệt: OutcomeItem chỉ chuẩn bị result portion; ReceiptAttributes chỉ là phần
embedded trong Uploads. Passing helper size tests không chứng minh full op/upload
item fit hoặc transaction đã bind đầy đủ authority.

## 4. Các bất biến giữ nguyên

- 20 base tables, không GSI/LSI/TTL; không sửa Terraform resources để tiện test.
- Chunks = domain#hash; Packs = pack_id/generation. Objects giữ bucket/key,
  bucket_id/domain conditions chống ABA; không normalize object key.
- UTC persisted facts tách local monotonic Clock. WriterFence không thay R1 admission.
- Pages native keys + Binary body; descriptor/index frozen, <=1024 entries.
- Item <=400 KiB, <=100 unique transaction targets, <=4 MiB item aggregate;
  separate reviewed wire budget cho SDK encoding/expressions. Không split publish.
- Mọi authoritative fact trong publication phải cùng transaction với matching
  outbox/outcome. Immutable payload/pages hoàn tất trước final pointer publication.
- Missing outcome/current head không chứng minh no-commit. Không release unknown.

## 5. Kiến trúc adapter đề xuất

Các tên file dưới đây là cách tổ chức dự kiến, không yêu cầu tạo stub trước:

| Nhóm | Files dự kiến | Trách nhiệm |
|---|---|---|
| Construction | store.go, codec.go | Options/API/inventory, typed decoding, owned copies |
| Reads | read.go, batch.go, query.go | Exact reads, BatchGet retry, Query/Scan pagination |
| Preparation | pages.go, canonical.go, pack_lifecycle.go | Owner lifecycle, immutable pages, Warm canonical CAS |
| Transactions | transaction.go, records.go | Target map, merged predicates, full images, token/attempt |
| Logical mutations | quota.go, commit.go, policy.go | Reservation/counters/head/tombstone/receipt |
| Reconciliation | outcome.go, errors.go | Coherent lookup, cancellation classification, bounded retry |
| Acceptance | contract_test.go, local_test.go, fault_test.go | Actual DDB Local + SDK fault injection |

Store nhận dependencies explicit; constructor kiểm đủ table mapping, không suy
account/region/table từ bucket S3. Local config có endpoint riêng/dummy creds,
không kế thừa profile hoặc ambient credentials thật. Production thiếu authority/
clock wiring phải fail closed/unready, không tự thay bằng fixture provider.

## 6. Checklist 1 — methods, reads, CAS và quota

### 6.1 Inventory các methods

| Nhóm | Methods/capabilities | Quy tắc |
|---|---|---|
| Identity/auth | GetBucket, GetCredential, Authorize, CreateBucket | Consistent facts; auth/incarnation predicates trong mutation |
| Listings/policy | ListAccessibleBuckets, ListObjects, GetPolicy, PutPolicy | Bound cursors/options; no-op policy không tạo event mới |
| Physical metadata | GetChunks, GetPacks, PutChunkIfAbsent, CompareAndSwapChunkLocation | Full domain/generation; winner và location_revision |
| Pack lifecycle | RegisterUploadingPack, MarkPackWarm | Register before physical write; Warm sau verify; CAS revision |
| Pages | WriteManifestPages, GetManifest, GetManifestPage | Immutable per-page, partial-batch retry, bounded indexed read |
| Logical quota | GetQuota/Reservation, Reserve/Adjust/Claim/Release | Owner ledger/counters, checked math, exact revisions |
| Publication | CommitObject, DeleteObject | Atomic bundle, auth/current bucket/head/base conditions |
| Outcomes | GetCommitOutcome, GetOutbox, GetCompletionReceipt | Stable original result, consistent lookup |
| Runtime support | UpdateAccessStats, ListPacksByState, OutboxExportStore where consumed | Bounded discovery, progress không đổi immutable event |

PartPageStore implementation có thể dùng chung page primitive khi cần bàn giao,
nhưng complete UploadPart/freeze APIs thuộc HAI-02.

### 6.2 Read implementation

- GetItem consistent cho auth/head/ledger/op/policy. Bounded coherent reconciliation
  dùng TransactGetItems; nhiều GetItem mạnh riêng lẻ không phải snapshot.
- BatchGet: deduplicate/split <=100 keys, key-inclusive projection, map unordered
  responses, retry chỉ UnprocessedKeys bằng jitter/backoff theo caller context.
- Chỉ processed-but-absent mới là missing. Unprocessed/throttle/deadline không
  được trả như chunk không tồn tại để engine ghi duplicate sai.
- LIST objects dùng Query; skip tombstone/wrong incarnation, giữ delimiter state,
  cursor/principal/options. Empty filtered page còn LastEvaluatedKey thì tiếp tục.
- Accessible buckets/packs/outbox candidates dùng bounded base Scan; không lấy
  Scan làm auth snapshot hoặc epoch completeness proof.

### 6.3 Pages và canonical mutation

Owner record kind/domain/writer/descriptor đặt trong SystemConfig namespace đã
duyệt: Building -> Closing -> Verified -> Published. Mỗi new page write condition
owner Building/revision/authority; identical retry đối chiếu bytes/digest kể cả
sau closure. Closing serialize với page write để không append sau verification.

Verify dense pages/offsets/domain/greedy split/chunk raw size/page_index, gồm empty
manifest và unexpected trailing pages. Retain roots/canonical dependencies tới
publication; không scan trước rồi publish dưới authority không ràng buộc.

Canonical insert condition absent + destination pack Warm; duplicate loser đọc
winner. Location CAS bind expected location revision, Warm next generation và
stable child event/op. Không tạo authoritative winner từ sidecar inventory.

### 6.4 Quota và reservation

Reserve gắn op/object/bucket_id/owner/base head snapshot/writer và staged bytes.
Giữ credit overwrite logical độc lập staging. Tái dùng AdjustReservationQuota
cho checked calculations nơi contract phù hợp, persist ledger+counters atomic.

Mỗi attempt đọc counters và tính postimages trong Go; compare observed counters/
revision trong transaction. Reserve/adjust retries không cộng delta hai lần.
Claim/release chỉ sau reconciliation và old-authority closure. Khác owner hoặc
bucket incarnation không được hưởng base size/credit của object cũ.

## 7. Checklist 2 — transaction planner và publication

### 7.1 Planner

Map target theo physical table + full key, gom predicates/aliases và chỉ phát
một action/target. Nếu cùng owner Users item vừa cần active check vừa update
counters, điều kiện active phải nằm trong Update đó.

Planner giữ complete Put/Update postimages và observed images cho check/delete,
bind observations bằng revision/content conditions. Chạy native guard và SDK wire
budget trước dispatch; hoàn toàn không coi keys-only image là size evidence.
CreateBucket/PutPolicy và các mutation không có object reservation vẫn cần stable
operation identity; nếu shared request model thiếu binding, mở rộng shared boundary
có review, không tự cấp random new op_id trên mỗi retry.

### 7.2 Commit bundle

```text
Objects: new revision/head hoặc tombstone
Users: Used/Reserved/Staged và ActiveUploads khi Complete
QuotaReservations: Active -> Committed, exact revision/writer/op
Outbox: immutable matching complete event
SystemConfig storage/op: original result + durable binding/attempt/outcome
SystemConfig storage/manifest: exact Verified -> Published (non-delete)
Uploads: Completing -> Completed + embedded receipt (khi Complete)
+ credential/user/grant/bucket/authority predicates merged by target
```

Receipt không tạo bảng mới; kiểm full Uploads item. Delete tombstone giữ revision,
không physical-delete data. expected=0 không wildcard: active bucket condition đi
cùng physical absence hoặc old-incarnation row hợp lệ; malformed missing bucket_id
phải fail, không xem là old bucket. Outcome cũ giữ original committed response
ngay cả sau overwrite/delete/bucket recreation.

### 7.3 Identity và attempt

Logical op/mutation stable; final object mutation và child chunk mutations có
binding riêng. Event identity dùng shared EventID, không literal op_id. Token
format theo baseline mutation32hex+attempt4hex; chỉ đổi attempt sau known conflict
và sau khi reconcile đủ. Exact retry phải giữ token, expressions, values và flags,
không chỉ giữ cùng logical JSON rồi để builder reorder theo map không ổn định.

Durable op records lưu parameters binding/attempt state và full result; phải
validate toàn bộ final item sau merge fields. Không dựa vào token window để thay
durable idempotence hoặc lookup current head khi op cũ đã bị overwrite.

## 8. Checklist 3 — unknown/throttle/error handling

| Tình huống | Cách xử lý |
|---|---|
| Response mất sau actual commit | Reconcile durable op/ledger/event/receipt, trả original result; không publish lại |
| Timeout chưa biết đã dispatch/commit | Giữ same attempt/token/params và protection; trả retryable unknown khi hết budget |
| Known conditional conflict | Phân loại condition, đọc coherent facts; chỉ recompute sau xác định attempt không commit |
| Throttle/UnprocessedKeys | Bounded retry; không biến thành missing hoặc quota failure |
| Same op khác input | Conflict, không mutate hoặc tạo event mới |
| Missing op nhưng authority chưa closed | Không release reservation, không claim no-commit |
| Stale writer/fence/current auth bị revoke | Fail closed với shared error phù hợp, không generic success |
| Missing internal record/malformed data | Internal/unavailable; không map thành NoSuchBucket tùy tiện |

SDK retry và application retry phải cùng budget, tránh nested unbounded loops.
Cancellation reasons không phải bằng chứng duy nhất cho business classification;
cần reconcile facts. Không catch mọi TransactionCanceledException thành ErrQuotaExceeded.

## 9. Checklist 4 — acceptance matrix

| Boundary | Cases tối thiểu | Backend/evidence |
|---|---|---|
| Codec/schema | Required fields/version, owned bytes, wrong type, max UTF-8 IDs, 20 tables/no-index/TTL | Unit + frozen fixtures |
| Batch/query | 100/101 keys, duplicate input, unordered, partial response, retries exhausted, empty filtered page | SDK fault double + Local |
| Pages | Empty/large, missing/trailing/domain mismatch, append-vs-Closing, owner reuse | Local actual transactions |
| Canonical | Same hash same/different domain, duplicate race, stale location CAS, non-Warm target | Shared suite + Local concurrency |
| Quota | Full quota same-size overwrite, delta growth/shrink, staging cap, concurrent owners/head/reservations | Badger + Local same contract |
| Atomicity | Auth/grant/bucket/fence conflict leaves zero partial head/counter/outbox/receipt effects | Local conditions/transactions |
| Unknown | Drop response after actual transaction, pre-dispatch failure, lookup unavailable, retry after another overwrite | Local SDK wrapper + contract hooks |
| Receipt | Exact completion/fence/state, retry identical, original expiry, ActiveUploads decrement once | Minimal Completion contract on Local |
| Bounds | Full final item 400 KiB edges, action100/101, duplicate target, aggregate4MiB and wire budget | Unit planner + Local valid requests |
| Integration | Writer prepare->commit->GET/Range->overwrite/delete, consumers use Store not fake | Existing real codec/read path + Local metadata |

Local fixture tạo đúng logical inventory với prefix test riêng; teardown chỉ các
bảng do factory tạo, không dùng credentials/profile thật. Existing contract hooks
Set*/FailCommit/FailReconcile được implement trong test harness, không public
admin backdoor. Integration CI khi được bật phải thực sự chạy và fail nếu endpoint
thiếu; không dùng SKIP làm evidence contract acceptance.

Levels: unit SDK shape != Local expression/atomic proof != AWS IAM/throttle/runtime
acceptance. Task W05-HAI-02 dành riêng real-DDB integrity/quota. HAI-01 không cần
full AWS stack; mọi real dev test bổ sung phải review account/role/budget/cleanup.

## 10. Lát cắt, commits và consumer handoff

| Mốc | Commit dự kiến | Gate trước commit/handoff |
|---|---|---|
| M0 | Contract/model adjustments nếu cần | Method/record/authority matrix rõ với Chiến/Phong/Hưng; Local engine sẵn sàng |
| M1 | Store/codecs/reads/local harness | Typed roundtrip + consistent/batch/pagination behavior |
| M2 | Canonical/pages/quota CAS | Shared first-group tests trên actual Local; owner closing races |
| M3 | Atomic Commit/Delete/policy/receipt | Complete transaction effects, full postimages/caps, no repeated target |
| M4 | Unknown outcome/reconciliation | Post-commit lost response và same-op replay không double count |
| M5 | Contract/integration CI + docs/evidence | Shared Badger/Local suite, make check/race, no-SKIP evidence |

Các mốc này phục vụ bốn checklist gốc; không tự tick Done từng mục chỉ vì có
commit. Có thể gộp các commit nhỏ theo checklist khi scope đã ổn; không tạo commit
"adapter complete" trước khi local contract pass.

- Chiến: bàn giao method/error/quota/Completion binding; Admin mutation request
  extensions review sớm, multipart orchestration tiếp tục ở HAI-02/CHIEN tasks.
- Phong: chốt durable R1 authority/registry/closure records/predicates ngay M0.
  HAI-01 build/test transactional seam song song, runtime không dùng fake proof.
- Hưng: explicit inventory/local fixture CI và app readiness wiring. Không bật
  runtime ready chỉ vì DDB ping thành công; cần adapter/config/Clock/protection.

Board Ready cho phép bắt đầu, không loại bỏ các integration obligations trên.
Nếu thiếu R1 proof hoặc unimplemented consumer methods, báo đúng scope còn thiếu,
không lấy local mutex hoặc contract fake thay distributed evidence.

## 11. Validation và hoàn tất

Trước PR: targeted package/shared suite; actual DDB Local contract; full make
check (Windows chạy equivalents nếu thiếu make); Linux race; diff/doc-name checks.
Evidence ghi source SHA, SDK/Local version, invocation, backend, test counts/no-SKIP,
unknown/conflict cases và scoped cleanup. Hash canonical Git blobs rồi verify từ
fresh Linux checkout, không lặp lỗi CRLF evidence của W03.

Điều kiện kết luận task hoàn tất: bốn checklist có bằng chứng; full shared
contract và consumers backend thật; atomic head/counters/outbox/outcome/receipt
đúng; CI trên final head xanh; review/acceptance theo quy trình nhóm. Merge không
tự thay backlog Done và DDB Local pass không khẳng định multi-node readiness.

## 12. Nguồn tham khảo

- [Approved adapter baseline](../contracts/dynamodb-adapter.md).
- [Storage handoff](../contracts/storage-handoff.md).
- [R1](../contracts/r1-and-cache.md), [R2](../contracts/r2-recovery.md).
- [AWS TransactWriteItems](https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_TransactWriteItems.html).
- [AWS BatchGetItem](https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_BatchGetItem.html).
- [Official DDB Local](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.html).

Kế hoạch kỹ thuật đề xuất không thay thế review shared protocol. Không có source
implementation, AWS mutation, commit hoặc push trong lượt lập kế hoạch này.
