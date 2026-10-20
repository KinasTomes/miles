# Chunking, dedup, nén và commit — W01-CHIEN-05

Trạng thái: đề xuất để review; Phong review độc lập test vectors và thứ tự durability/commit, người thực hiện (Chiến) không tự review. Task Miles `W01-CHIEN-05` (nguồn `W01-HAI-03`, A08/A14), phụ thuộc `W01-HUNG-04` (Done) và `W01-CHIEN-04` (PR #13 đã merge). Chưa có chunker, codec hay adapter production: fixture trong `tests/fixtures/engine/` được kiểm bằng hàm tham chiếu viết trong test từ tài liệu này. Nguồn: [kiến trúc §1.5, §3, §5, §8–10, §19.1](../dedup-storage-gateway-architecture.md), [API §2.2.3, §2.6, §4.3](../dedup-storage-gateway-api.md), [shared storage](shared-storage.md), [pack/pages](pack-and-pages.md), [clock/policy](clock-and-policy.md).

| Checklist W1 | Mục | Fixture |
|---|---|---|
| FastCDC min/avg/max 256 KiB/1 MiB/4 MiB, SHA-256 raw content, zstd saving threshold | §2–4 | `chunking.json`, `compression.json` |
| Lookup canonical, Chunks insert-if-absent/location CAS, loser orphan, visibility qua head | §5 | `commit.json` (`lookup`, chunk transactions) |
| Transaction head/quota/outbox/Completed receipt, op_id ổn định, refcount ngoài transaction | §6 | `commit.json` (`transactions`) |

## 1. Phạm vi

Write path: body logical → FastCDC → SHA-256 → lookup `(domain, hash)` → nén chunk cần ghi → pack/sidecar → `Chunks` → manifest pages → transaction R2. Tài liệu chốt thuật toán và tham số chunking, identity, quy tắc chọn codec, quyết định reuse/ghi/revive, thứ tự bền vững và danh sách action transaction. Không chốt: byte layout pack/pages (pack-and-pages), cơ chế pin/gate/lease (R1, W01-PHONG-02), encoding event/journal (W01-PHONG-03), merge/validator policy (W02-PHONG-01).

## 2. FastCDC v1

### 2.1 Tham số và validator

Policy `chunking` (KT §19.1, clock-and-policy §4.2) mặc định `min_bytes = 262144`, `avg_bytes = 1048576`, `max_bytes = 4194304`. Validator chỉ nhận số nguyên thỏa:

- `16384 ≤ min_bytes ≤ avg_bytes ≤ max_bytes ≤ 16777216`;
- `avg_bytes` là lũy thừa của 2 (mask lấy `log2(avg_bytes)` bit).

Sàn 16 KiB giữ object 100 GiB dưới 1024 manifest pages (pack-and-pages §3.4). Trần 16 MiB chặn buffer mỗi stream; chunk lớn nhất vẫn vừa `u32 raw_len`. `min = avg = max` hợp lệ và cho chunk cố định. Fixture: `chunking.json` → `policy`.

### 2.2 Thuật toán

FastCDC normalized chunking mức 2 (Xia và cộng sự): bỏ qua `min` byte đầu, mask khó hơn trước `avg`, dễ hơn sau `avg`, cắt cưỡng bức ở `max`. Miles tự định nghĩa bảng gear và mask để mọi implementation tái tạo được mà không chép bảng của thư viện:

- `GEAR[i]` = `u64` big-endian của 8 byte đầu `SHA-256([i])`, `i = 0..255`. SHA-256 của 256 entry nối big-endian: `7ce4baec…3e21f097`.
- `b = log2(avg)`; `MASK_S` = `b + 2` bit cao nhất, `MASK_L` = `b − 2` bit cao nhất của `u64` (mặc định `fffffc0000000000`, `ffffc00000000000`). Bit cao của gear hash phụ thuộc tới 64 byte gần nhất, nên không cần trải mask.

```go
// cut trả độ dài chunk kế tiếp của b (len(b) > 0); mọi phép toán u64 tràn vòng.
n := len(b)
if n <= min { return n }
if n > max { n = max }
normal := min(avg, n)
fp := uint64(0)
i := min
for ; i < normal; i++ { fp = fp<<1 + GEAR[b[i]]; if fp&MASK_S == 0 { return i } }
for ; i < n; i++      { fp = fp<<1 + GEAR[b[i]]; if fp&MASK_L == 0 { return i } }
return n
```

Chunk kết thúc **trước** byte làm hash khớp mask. Chunk không cuối dài `[min, max]`, chunk cuối `[1, max]`, object rỗng không có chunk. Đo trên 256 MiB stream ngẫu nhiên với tham số mặc định: 225 chunk, trung bình 1193046 byte, ngắn nhất 264009, dài nhất 2162221, không chunk nào bị cắt ở `max`.

### 2.3 Áp dụng

- Input là body logical **sau** giải mã aws-chunked, trước nén. PUT chunk cả body; UploadPart chunk từng part từ đầu part (KT §5.2). Copy cùng miền không chunk lại; Copy khác miền giữ ranh giới chunk nguồn và lookup từng hash ở miền đích (KT §5.3).
- Mỗi request dùng policy snapshot pin lúc admission (clock-and-policy §7.2) cho mọi chunk của request/part. Đổi chunking chỉ làm lệch ranh giới của dữ liệu ghi sau (giảm dedup), không ảnh hưởng đọc: manifest lưu ref, không tính lại ranh giới.
- Chèn hoặc xóa 1 byte giữa stream 16 MiB chỉ đổi 1/16 chunk (fixture `insert one byte`, `delete one byte`); block 3 MiB lặp 4 lần cho cùng 3 hash lặp lại (`repeated block`); dữ liệu toàn 0 cắt ở `max` (`zeros`).

## 3. Hash và identity

- Hash chunk là SHA-256 của **raw bytes** của chunk (KT §1.2 nguyên tắc 5), hex lowercase. Không dùng checksum/ETag client, không băm stored bytes. MD5/ETag và `x-amz-checksum-*` tính riêng trên body logical (API §2.7).
- Identity là `storage.ChunkKey{domain, hash}`, partition key `domain#hash` (shared storage). Domain là `dedup_domain` của bucket đích lúc admission (Complete dùng domain đã chép vào upload); commit condition-check `bucket_id` nên domain không đổi giữa admission và commit. Cùng nội dung ở `default` và `tenant-b` là hai identity (fixture `identity`).

## 4. Nén

### 4.1 Quy tắc chọn codec

Chỉ chunk cần ghi (§5: insert hoặc revive) mới được nén, sau lookup; chunk reuse không tốn CPU nén.

```
compression.enabled = false            → raw (không chạy encoder)
encoder lỗi                            → raw, tăng metric lỗi nén
stored_len ≤ raw_len × (1 − min_saving_ratio) → zstd
ngược lại                              → raw
```

So sánh bằng số học thập phân chính xác (`min_saving_ratio` chuẩn hóa từ JSON decimal một lần, clock-and-policy §4.2), không float. Mặc định 0.1: chunk 1 MiB giữ zstd khi frame ≤ 943718 byte. Implementation có thể bỏ qua encoder khi ước lượng entropy cho thấy không đạt ngưỡng; kết quả là raw, vẫn hợp lệ. Fixture: `compression.json` → `decisions`.

### 4.2 Encoder, level và frame

- Encoder: `github.com/klauspost/compress/zstd` (Go thuần, không cgo, build chéo binary Edge); W02-HAI-01 pin version khi thêm dependency. Mỗi chunk là một lần `EncodeAll`, đúng một frame RFC 8878, không dictionary, không byte sau frame (pack-and-pages §2.3).
- Validator `compression`: `codec = zstd` (raw là fallback lưu trữ, không là codec policy; tắt nén dùng `enabled = false`), `level ∈ {1, 3, 7, 11}`, `0 ≤ min_saving_ratio < 1`. Bốn level ứng với bốn encoder khác nhau của `EncoderLevelFromZstd` (fastest, default, better, best); level khác dùng chung encoder nên bị từ chối thay vì nhận im lặng. Tỉ lệ nén không tương đương libzstd cùng số level. Fixture: `compression.json` → `policy`.
- Byte frame không xác định qua version/thư viện, nên không là identity. Retry PUT pack cùng generation gửi lại đúng bytes đã buffer, không nén lại (`PackStore` trả conflict nếu bytes khác).
- Trước khi đưa blob zstd vào pack, writer giải nén lại và so SHA-256 với hash: lỗi encoder bị chặn khi body client còn trong tay, không phát hiện muộn lúc GET. Reader giới hạn output theo `raw_len`, từ chối frame khai báo content/window lớn hơn 16 MiB.
- Đổi policy nén chỉ áp chunk mới/revived; chunk cũ đọc theo `codec` đã lưu (KT §1.6).

## 5. Lookup canonical, insert/CAS và orphan

### 5.1 Lookup

Mỗi request giữ map `(domain, hash) → quyết định`; hash lặp trong request dùng lại quyết định đầu (một blob, đếm `x-dsg-chunks-dup` theo API §5). Baseline: `GetChunks(keys, consistent=true)` theo lô (BatchGetItem ≤ 100 key, retry hết `UnprocessedKeys`). Tối ưu W02-HAI-01 được phép nếu giữ §5.4:

- LRU node-local giữ row `Chunks`: chỉ là ứng viên; reuse vẫn cần pin đúng location hiện hành (§5.4).
- Bloom node-local theo `domain#hash`: âm tính chỉ được bỏ qua lookup khi Bloom phủ đủ store (profile một node Badger: dựng từ scan `Chunks` lúc khởi động và mọi insert trong process). Cluster: Bloom thiếu chunk node khác ghi, bỏ lookup sẽ ghi blob trùng rồi thua insert; vẫn đúng nhưng tốn PUT/bytes, chỉ bật khi đo có lợi.

### 5.2 Quyết định PUT/UploadPart

| Row `Chunks` (consistent) | Pack canonical `(pack_id, generation)` | Hành động |
|---|---|---|
| Không có | — | **insert**: ghi blob, `PutChunkIfAbsent` |
| Có | `Warm` (STANDARD/STANDARD_IA) hoặc `Cold` + `GLACIER_IR`, pin R1 thành công | **reuse** location hiện có |
| Có | `Cold` + `GLACIER`/`DEEP_ARCHIVE`, `Restoring`, `Restored` (R3) | **revive**: ghi blob Warm từ body client, CAS location |
| Có | `Uploading`, `Transitioning`, `Compacting`, `Retiring`, `Retired`, `Deleting`, `Deleted`, hoặc pin bị từ chối | **revive** |

Không restore nguồn, không giữ pointer tạm ở cache. Writer mất fence thì dừng request (503), không revive. Copy khác miền dùng bảng KT §5.3 (reuse Restored đủ deadline + 120 giây), ngoài fixture này. Fixture: `commit.json` → `lookup`.

### 5.3 Ghi pack trước location

1. Đăng ký item `Packs[pack_id, generation]` state `Uploading`, `protected_until ≥ hard deadline request + 15 phút`, gắn writer fence. Port này chưa có trong `MetadataStore` (§8).
2. `PutPack` đủ bytes, rồi `PutSidecar` (STANDARD).
3. CAS `Packs` `Uploading → Warm` dưới fence; `live_bytes` ban đầu là tổng stored bytes của mọi entry (cận trên; GC đối soát giảm).
4. Với từng entry: insert hoặc CAS (§5.4). Không row `Chunks` nào trỏ tới pack chưa `Warm`.

Pack được đóng theo pack-and-pages §2.3 (32–64 MiB hoặc 16384 entry) và ở cuối body; có thể ghi trước khi body kết thúc. Chunk của request sau này lỗi checksum/chữ ký vẫn đúng nội dung theo hash của nó, nên row đã insert vẫn an toàn để dedup.

### 5.4 Insert, CAS và race

Mỗi insert/CAS là một transaction 4 action (§6.3) có Outbox event location riêng, op_id mới mỗi lần thử, cùng op_id khi retry kết quả chưa rõ.

| Bước | Kết quả | Tiếp theo |
|---|---|---|
| `PutChunkIfAbsent` | Thắng (`inserted`) | Location của writer là canonical |
| `PutChunkIfAbsent` | Thua: row winner trả về (DynamoDB `ALL_OLD`) | Phân loại winner theo §5.2: reuse thì pin winner, blob của writer thành orphan; revive thì CAS từ `winner.location_revision` sang blob đã ghi, không ghi lại |
| CAS từ revision `r` | Thắng | Location mới canonical, `location_revision = r + 1`, refcount giữ nguyên |
| CAS | `ErrConflict` | Đọc consistent, phân loại lại; thắng chỉ khi CAS với revision mới |
| CAS | `ErrNotFound` (row đã bị GC) | Insert blob đã ghi |

Vòng lặp kết thúc trước hard deadline; mỗi conflict nghĩa là writer khác đã tiến triển. Location chỉ chuyển sang generation Warm mới (revive, compaction, promote); tiering không chuyển pack đang được pin/protect (R1), nên một location đã pin vẫn đọc được trong cửa sổ protection dù sau đó bị CAS đi.

**Bất biến trước commit (I1):** mỗi `ChunkKey` distinct trong manifest có canonical location đọc được ngay, hoặc do chính writer insert/CAS thắng trên pack ở bước 5.3, hoặc đã được R1 pin đúng `(pack_id, generation)` ở `location_revision` vừa đọc consistent. `PinLocations` (W01-PHONG-02) phải tự kiểm `location_revision` trong cùng thao tác hoặc caller đọc consistent lại sau pin; khác revision thì phân loại lại.

### 5.5 Orphan và visibility

- Blob thua race là byte chết trong pack của writer; tính vào `x-dsg-bytes-stored` (API §5) và được GC/compaction xử lý. Row `Chunks` không có head nào tham chiếu là dữ liệu hợp lệ, dedup được trong cùng miền, GC quyết định bằng root scan R1 (KT §8).
- Client chỉ thấy object qua head: GET/HEAD/LIST đi head → pages → `Chunks`. Pack, sidecar, row `Chunks`, pages bền vững trước commit đều vô hình với S3 API cho tới transaction §6.3.

## 6. Commit

### 6.1 op_id

- `op_id` là 16 byte ngẫu nhiên, 32 hex, sinh lúc admission và ghi vào `QuotaReservations` trước mọi ghi vật lý. Mọi retry nội bộ của request dùng cùng op_id; client gửi lại HTTP request là op mới (last writer wins). DeleteObjects: một op_id mỗi key. Multipart: op_id sinh lúc CreateMultipartUpload cùng reservation ledger của upload và là `completion_op_id` của mọi lần freeze/takeover (API §4.3 gắn nó vào upload khi CAS Active → Completing). Mở lại Active không commit nên event chưa tồn tại; lần freeze sau dùng lại op_id và ledger mà vẫn commit tối đa một lần.
- Mỗi transaction authoritative ghi đúng một Outbox event; `event_id` suy ra xác định từ op_id (API §4.3.1 hiện dùng chính op_id; encoding cuối do W01-PHONG-03 chốt) và Put có điều kiện `attribute_not_exists`. Cùng điều kiện ledger `Active` ở revision đang giữ (§6.3 action 3), một op_id commit tối đa một lần, kể cả khi recompute với head revision mới, attempt cũ đến muộn hoặc token đã hết hạn.
- `ClientRequestToken` = op_id nối `attempt` dạng 4 hex lowercase, luôn đúng 36 ký tự (giới hạn DynamoDB), `attempt` 0..65535. Mỗi bộ tham số transaction mới (recompute, freeze lại, conflict tạm thời) tăng `attempt`; retry kết quả chưa rõ giữ token và tham số (KT §9.0). Hết 65535 attempt thì dừng, 503, giữ reservation; với backoff trong deadline 30 phút không chạm tới. Token chỉ chống áp lặp trong cửa sổ 10 phút của DynamoDB; ngoài cửa sổ, điều kiện Outbox và ledger mới là bảo đảm. Fixture: `commit.json` → `tokens` (attempt 0, 999, 1000, 65535, 65536 và op_id sai).

### 6.2 Thứ tự bền vững — PUT

1. Admission: quyền (API §2.2.3), domain/bucket_id, `RegisterWriter` (R1), reserve quota với op_id (API §2.6), policy snapshot.
2. Stream: giải mã aws-chunked, MD5/checksum tăng dần, FastCDC, SHA-256 từng chunk.
3. Lookup §5.1–5.2; reuse thì pin; ghi thì nén §4 và đưa vào pack.
4. Pack đầy hoặc hết body: §5.3 rồi §5.4.
5. Hết body: kiểm checksum/MD5/độ dài/chữ ký; sai thì lỗi API, release reservation, không có head.
6. Manifest pages chia greedy (pack-and-pages §3.3); `WriteManifestPages`, retry tới khi mọi page bền.
7. Kiểm I1 và đủ pages; tính `size`, `page_count`, `chunk_count`, `page_index`.
8. Transaction §6.3 — **điểm công bố duy nhất**.
9. Trả 200 + ETag.
10. Async theo op_id: export outbox (R2), refcount delta (§6.6), nhả pin sau protection.

| Crash sau bước | Bền vững | Object mới | Phục hồi |
|---|---|---|---|
| 1 | Reservation Active | Không | Hết lease: claim, không có event của op_id → release (shared storage, reservation lifecycle) |
| 4, giữa pack/sidecar | `Packs` Uploading, có thể có object S3 | Không | Pack orphan: hết protection, GC dọn theo inventory S3 + `Packs` |
| 4, sau một phần insert/CAS | Row `Chunks` canonical | Không | Chunk dedup được; GC root scan quyết pack/chunk |
| 6 | Một phần hoặc đủ pages | Không | Manifest orphan, xóa theo KT §8.3 |
| 8, mất ACK | Có thể đã commit | Chưa biết | §6.4 |
| 9 | Head, quota, outbox | Có | Client retry là op mới, ghi đè cùng nội dung |

Mọi nhánh lỗi trước bước 8 để object cũ nguyên vẹn (overwrite) và không đổi quota used (KT §9.0).

### 6.3 Transaction

Một `TransactWriteItems` (Badger: một transaction đọc/ghi cùng các key). Item đã có action ghi thì gộp điều kiện vào action đó, vì DynamoDB từ chối hai action trên một item (API §2.2.3).

| # | Item | Action | Điều kiện | Ghi |
|---|---|---|---|---|
| 1 | `Objects[bucket, key]` | Put | `revision = expected`; expected 0 thì `attribute_not_exists` (tombstone có revision) | Head hoặc tombstone, `revision = expected + 1`, op_id |
| 2 | `Users[owner]` | Update | `used + D + (reserved − r) ≤ limit`, `reserved ≥ r`, `staged ≥ s`; caller là owner thì thêm `status = active` | `used += D`, `reserved −= r`, `staged −= s`; Complete thêm `active_uploads −= 1` |
| 3 | `QuotaReservations[reservation_id]` | Update | `state = Active`, `revision`, op_id, writer fence, deadline chưa qua (bound Clock) | `state = Committed`, `revision + 1` |
| 4 | `Outbox[event_id]` | Put | `attribute_not_exists` | Event `Objects`: history_id/recovery_epoch, object + bucket_id, revision mới, op_id, pointer/SHA-256 payload |
| 5 | `Credentials[access_key_id]` | ConditionCheck | `status = active` | |
| 6 | `Users[caller]`, khi caller ≠ owner | ConditionCheck | `status = active` | |
| 7 | `Buckets[bucket]` | ConditionCheck | `bucket_id` lúc admission, `state = active` | |
| 8 | `BucketGrants[bucket_id, caller]`, khi quyền từ grant | ConditionCheck | `write = true` | |
| 9 | `Leases[writer lease]` | ConditionCheck | Fence hiện hành (lease_id, service_incarnation, epoch) và history/epoch đã đăng ký; chi tiết R1 | |
| 10 | `Uploads[upload_id]`, chỉ Complete | Update | `state = Completing`, `revision`, completion fence/lease | `Completed`, `revision + 1`, receipt (API §4.3.1) |

`D = S − H` với `H` là size của head ở revision expected (0 nếu chưa có hoặc tombstone), `S` là size mới (0 cho tombstone); `r`, `s` là reserved/staged của reservation. Tối đa 10 action, item lớn nhất là head (page_index ≤ 8 KiB) và Uploads (pack-and-pages §3.7), xa giới hạn 100 action/4 MB. DELETE dùng cùng danh sách, Put tombstone. Badger kiểm như ConditionCheck bằng cách đọc các key trong cùng transaction.

Insert/CAS chunk:

| Item | Action | Điều kiện | Ghi |
|---|---|---|---|
| `Chunks[domain#hash]` | Put (insert) / Update (CAS) | `attribute_not_exists` / `location_revision = expected` | Location, codec, raw/stored len, `location_revision` 1 / `+1`; refcount không đổi |
| `Packs[pack_id, generation]` | ConditionCheck | `state = Warm` | |
| `Leases[writer lease]` | ConditionCheck | Fence hiện hành | |
| `Outbox[event_id]` | Put | `attribute_not_exists` | Event `Chunks`: domain/hash, location, `location_revision` |

Fixture: `commit.json` → `transactions` (7 kịch bản, kiểm không trùng item, ≤ 100 action, đúng một event).

### 6.4 Kết quả, reconcile và cleanup

Transaction §6.3 chỉ commit khi `Outbox[event_id]` chưa có và ledger `Active` ở revision writer đang giữ; commit đổi ledger sang `Committed`. Vì vậy ledger còn `Active` ở revision đó và chưa có event, đọc sau một phản hồi xác định, là bằng chứng chưa commit; release đổi revision ledger nên attempt cũ đến muộn cũng không commit được sau đó.

1. Thành công: đã commit.
2. Chưa rõ (timeout, 5xx, mất kết nối, `TransactionInProgressException`): retry cùng token và tham số, backoff, tới khi có phản hồi xác định hoặc hết deadline. Hết deadline: 503, giữ reservation, pin và roots; sweeper claim khi lease hết (shared storage, reservation lifecycle).
3. Mọi phản hồi khác (`TransactionCanceledException` với bất kỳ tập lý do nào, `IdempotentParameterMismatch`): **reconcile theo op_id trước**, không dựa vào lý do hủy hay thứ tự của chúng. Đọc consistent `Outbox[event_id]`, `QuotaReservations[reservation_id]` (Complete thêm receipt trong `Uploads`):

| Kết quả đọc | Kết luận | Hành động |
|---|---|---|
| Không đọc được | Chưa rõ | 503, giữ reservation, pin, roots |
| Có event, ledger `Committed` cho op_id, hoặc receipt có `completion_op_id` | Đã commit | Trả kết quả gốc (revision từ event, ETag đã biết hoặc response trong receipt); không recompute, không cộng quota, không release |
| Ledger không còn `Active` ở revision đang giữ, fence writer không còn hiện hành, hoặc upload không còn `Completing` | Mất quyền sở hữu | Dừng, 503; không publish, không release (owner mới claim và reconcile) |
| Ledger `Active` ở revision đang giữ, không có event | Chưa commit | Phân loại bên dưới |

Phân loại bằng đọc consistent lại, theo thứ tự cố định: credential hoặc user caller không active → `AccessDenied`; bucket sai `bucket_id` hoặc không active → `NoSuchBucket`; grant mất write → `AccessDenied`; head khác revision expected → đọc lại head, tính lại `D`, attempt mới; quota không đủ → `QuotaExceeded`; mọi điều kiện đều đúng (`TransactionConflict` tạm thời) → attempt mới.

Ví dụ: A commit nhưng mất ACK, B ghi đè, A retry sau cửa sổ token. Điều kiện `Objects`, ledger và `Outbox` cùng fail; reconcile thấy event của A nên trả revision của A, quota không đổi lần hai. Phân loại theo action fail đầu tiên sẽ recompute hoặc trả lỗi dù A đã thành công.

Cleanup khi chưa commit:

| Phân loại | PUT, Copy, DELETE | Complete của client | Complete takeover |
|---|---|---|---|
| `QuotaExceeded` | Release (API §2.6) | Mở lại Active, giữ ledger và counters (API §2.6, §4.3.1 dòng 13) | Như client; upload đã hết hạn thì Aborting + release (API §4.3 bước 4) |
| `AccessDenied` | Release (API §2.2.3) | Mở lại Active, giữ ledger và counters (API §4.3.1 dòng 12) | Aborting + release (API §2.2.3, §4.3 bước 4) |
| `NoSuchBucket` | Release | Aborting + release; phòng thủ, vì DeleteBucket từ chối khi còn upload hoạt động (API §4.2) | Aborting + release |

- Release: một transaction CAS ledger `Active@R → Released@R+1`, `reserved −= r`, `staged −= s` (shared storage, release); Aborting thêm `active_uploads −= 1` và CAS `Uploads` sang `Aborting`.
- Mở lại Active: CAS `Uploads` `Completing → Active` dưới completion fence; ledger giữ `Active@R`, counters giữ nguyên. Client retry Complete freeze lại và commit với cùng op_id, cùng ledger.
- Complete: retry cùng input kiểm receipt trước (API §4.3.1); takeover dùng `completion_op_id`.

Fixture: `commit.json` → `scenarios` (13 kịch bản: mất ACK rồi retry cùng token; mất ACK, ghi đè, retry sau cửa sổ token; head conflict rồi recompute; PUT quá quota và grant bị thu hồi; Complete quá quota giữ reservation rồi retry thành công; Complete takeover quá quota; Complete client/takeover sau thu hồi grant; Complete khi bucket mất; mất fence; metadata không đọc được; tombstone). Mỗi bước kiểm kết quả, revision, số transaction đã gửi; cuối kịch bản kiểm head, counters owner, ledger, upload và event.

### 6.5 Complete, Copy, Delete

- Complete: chunk đã bền qua UploadPart (§6.2 bước 1–6 cho part pages + publish part head). Complete freeze, ghi CompletionPages, prepared manifest (pages mới, manifest_id mới), pin location hiện hành, rồi transaction §6.3 có action 10 với op_id của upload (§6.1, API §4.3).
- Copy: cùng miền chỉ ghi manifest mới từ refs nguồn đã pin; khác miền theo §5 ở miền đích; commit như PUT.
- DELETE: không có bước chunk/pages; transaction §6.3 với tombstone.

### 6.6 Refcount

Refcount không nằm trong transaction §6.3 (KT §9.0: không đưa hàng nghìn update vào commit). Mỗi event `Objects` được áp đúng một lần theo lô:

- Delta mỗi key = số lần xuất hiện trong manifest mới − trong manifest bị thay (overwrite/delete; event mang cả hai manifest_id, §8). Bỏ delta 0, sắp key theo partition key `domain#hash`, cắt lô 99 key. Manifest bất biến nên lô `b` luôn cùng nội dung khi replay; hash lặp chỉ thành một Update (giảm hot key, KT §17).
- Lô `b` là một `TransactWriteItems` tối đa 100 action: Update `Outbox[event_id]` điều kiện `refcount_next = b` (thiếu = 0), đặt `b + 1`; Update từng `Chunks[domain#hash]` điều kiện `attribute_exists`, `ADD refcount delta`. Điều kiện tiến độ nằm cùng transaction với delta nên lô áp đúng một lần dù crash sau commit trước ACK, replay nhiều lần, hay worker cũ gửi lại lô đã áp; không cần `ClientRequestToken`. Phản hồi khác thành công: đọc consistent `refcount_next` rồi tiếp tục từ đó; `TransactionConflict` thì retry.
- Refcount là số nguyên có dấu, không có điều kiện chống âm: event DELETE có thể được xử lý trước event PUT tạo tham chiếu nên giá trị âm tạm thời; guard chống âm sẽ làm lô kẹt mãi. Giá trị đúng khi mọi event đã áp; GC không bao giờ xóa theo refcount (KT §8.1).
- Thiếu row `Chunks` là vi phạm bất biến (manifest bị thay vẫn là root tới khi outbox xử lý xong, KT §8.1): dừng worker, báo động; không để `ADD` tạo row mới.
- `refcount_next` là thuộc tính tiến độ trên item Outbox, như export state; payload event vẫn bất biến. Không dọn item Outbox trước khi `refcount_next` bằng số lô.

Fixture: `commit.json` → `refcount` (xóa về 0 rồi crash và replay, 150 key hai lô crash sau từng lô và gửi lại, overwrite gộp hash lặp, DELETE xử lý trước PUT).

## 7. Fixtures và test

| File | Nội dung |
|---|---|
| `chunking.json` | Digest bảng gear, mask theo avg, 11 ca chunk (rỗng, 1 byte, đúng `min`, `min + 1`, stream 16 MiB, chèn/xóa 1 byte, toàn 0, block lặp, tham số nhỏ, cố định) với độ dài và SHA-256 từng chunk; identity hai miền; 10 ca validator chunking |
| `compression.json` | 11 quyết định codec (biên 10%, 1 MiB, ratio 0 và 0.999, chunk nhỏ, tắt nén, encoder lỗi); 14 ca validator compression |
| `commit.json` | 18 ca lookup; action của 7 transaction; 13 kịch bản commit (§6.4); 7 token; 4 kịch bản refcount |

`internal/engine/contract_test.go` chạy chunking/nén/lookup/action qua hàm tham chiếu (chunker, validator, chọn codec, lookup, dựng transaction), so từng chunk, kiểm biên `[min, max]`, số chunk đổi khi chèn/xóa, partition key qua `storage.ChunkKey`, và bất biến DynamoDB của từng transaction. `internal/engine/commit_test.go` mô hình điều kiện và hiệu ứng transaction §6.3, writer §6.4 (token, reconcile, phân loại, cleanup), token §6.1 và lô refcount §6.6. Input stream: `SHA-256(seed ‖ u64be(k))` nối nhau, cắt đúng độ dài. Một implementation JavaScript viết riêng (gear hash bằng hai nửa 32 bit) cho cùng mọi chunk của 11 ca.

```bash
go test ./internal/engine
make check
```

## 8. Bàn giao

| Task | Việc |
|---|---|
| W02-HAI-01 | Chunker/hasher §2–3, zstd §4 (thêm dependency klauspost, pin version), Bloom/LRU §5.1; chạy `chunking.json`/`compression.json` rồi xóa reference tương ứng trong `contract_test.go` |
| W02-PHONG-01 | Validator `chunking`/`compression` theo §2.1/§4.2 và các ca `policy` |
| W02-HAI-02 | Pack writer §5.3; thêm port đăng ký `Packs` Uploading → Warm (hiện `MetadataStore` chỉ có `GetPacks`) |
| W02-HAI-03, W04-HAI-01 | Badger/DynamoDB: transaction §6.3, insert/CAS kèm Outbox, `ALL_OLD` cho winner, token §6.1, reconcile và cleanup §6.4; chạy `transactions`, `scenarios`, `tokens` |
| W03-HAI-01 | Write path §5–6.2, I1, bảng crash |
| W01-PHONG-02 | `PinLocations` kiểm `location_revision`; item `Leases` trong điều kiện commit/insert/CAS |
| W01-PHONG-03, W04-HAI-02 | Encoding event identity; entity `Chunks` (`OutboxEvent.Validate` hiện chỉ nhận `Objects`/`SystemConfig`); payload event `Objects` mang manifest mới và manifest bị thay; thuộc tính `refcount_next`, không dọn Outbox trước khi refcount xong (§6.6); op_id upload sinh lúc Create, mở lại Active giữ ledger (§6.1, §6.4) |
| W05-HAI-01 | Refcount worker §6.6, chạy `refcount`; đối soát `live_bytes`/refcount, dọn orphan §5.5 |

Đổi thuật toán, gear, mask hoặc giới hạn tham số sau freeze chỉ làm giảm dedup với dữ liệu cũ, không làm hỏng dữ liệu; vẫn cần version mới (`fastcdc-v2`) và review vì fixture v1 phải giữ nguyên.
