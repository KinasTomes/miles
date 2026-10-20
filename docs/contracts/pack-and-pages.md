# Định dạng pack, sidecar và pages v1 — W01-CHIEN-04

Trạng thái: đề xuất để review, chuẩn bị freeze cuối tuần 2 ở W02-HAI-02. Task Miles `W01-CHIEN-04` (nguồn `W01-HAI-02`, A02/A09/A11), phụ thuộc `W01-HUNG-04` (Done). Chưa có codec production: fixture trong `tests/fixtures/format/` được kiểm bằng encoder/reader tham chiếu viết trong test từ tài liệu này. Nguồn: [kiến trúc §4, §5, §9.0, §10](../dedup-storage-gateway-architecture.md), [API §4.3.1](../dedup-storage-gateway-api.md), [shared storage](shared-storage.md).

| Checklist W1 | Mục | Fixture |
|---|---|---|
| Footer/index có domain, codec, offset/length/hash/generation, checksum/version | §2 | `pack.json` |
| Manifest/part/completion pages phân trang theo serialized bytes dưới 400 KiB, transaction dưới giới hạn | §3 | `pages.json` |
| Golden empty/small/large/mixed-domain và dữ liệu hỏng | §4 | cả hai |

## 1. Quy ước chung

- Số nguyên little-endian: `u8`, `u32`, `u64`. Không padding, không alignment.
- `id`: `u8` độ dài + byte ASCII khớp `^[a-z0-9_-]{1,64}$` (cùng quy tắc domain/bucket_id của `internal/storage`). Dùng cho domain, pack_id, generation, manifest_id, part_manifest_id, completion_id. Writer production sinh mọi ID trừ domain bằng 16 byte ngẫu nhiên, ghi 32 ký tự hex lowercase.
- `hash`: 32 byte SHA-256 của **raw content** (trước nén); metadata ghi hex lowercase. Cùng hash khác domain là identity khác (KT §1.5).
- Mỗi cấu trúc có `version` `u32` = 1. Reader gặp version khác trả lỗi, không fallback. Sau freeze, đổi layout phải tăng version kèm migration và review.
- Reader strict: mọi length được kiểm giới hạn trước khi cấp phát, mọi byte phải được giải thích (không byte thừa), lỗi đầu tiên dừng việc đọc. Writer phải tự kiểm như reader trước khi PUT/ghi page.

Lỗi được phân theo **stage**; fixture ghim stage, không ghim thông điệp. Mọi stage là dữ liệu hỏng hoặc writer sai: GET trả 500 trước header hoặc ngắt stream sau header, tăng `integrity_error_total` (KT §6); scrub báo corrupt.

| Stage | Nghĩa |
|---|---|
| `trailer` | Thiếu magic, version lạ, `index_offset + index_len + 24 ≠ size`, index quá 2 MiB |
| `index_checksum` | SHA-256 của footer/sidecar sai, kể cả bị cắt |
| `index` | Index đúng checksum nhưng sai cấu trúc: magic/version, ID, số entry, domain/hash trùng, offset không liền, codec, độ dài |
| `identity` | pack_id/generation trong index khác PackRef đang đọc |
| `header` | Header pack sai magic/version/ID |
| `blob_checksum` | CRC-32C của stored bytes sai |
| `content` | Giải nén lỗi, sai `raw_len` hoặc SHA-256 |
| `page_checksum` | SHA-256 của page sai |
| `page` | Envelope/payload sai cấu trúc, ref `raw_len` = 0, domain sai quy tắc |
| `page_key` | kind/owner/page_no trong page khác item key đang đọc |
| `manifest` | Head và pages không khớp: thiếu page, offset, domain, size, chunk_count, page_index, quá 1024 pages |
| `stream` | Completion stream thiếu page, sai độ dài/digest, snapshot sai cấu trúc |

## 2. Pack, footer index và sidecar

### 2.1 Key S3

```
packs/{xx}/{pack_id}/{generation}
packs-index/{xx}/{pack_id}/{generation}.idx
```

`xx` là 2 ký tự đầu của pack_id ngẫu nhiên (256 prefix, KT §4.1); adapter từ chối pack_id ngắn hơn 2 ký tự. Generation nằm trong key vì `PackRef` là cặp (pack_id, generation) và contract `PackStore` cho hai generation cùng pack_id tồn tại, xóa độc lập (promotion). Mọi lần ghi nội dung mới (pack mới, promotion, compaction) dùng generation mới; đổi storage class giữ nguyên key, generation và bytes. Sidecar luôn STANDARD.

### 2.2 Bố cục

```
[header][blob 0][blob 1]...[blob n-1][index][trailer 24 byte]
```

**Header** — độ dài `H = 14 + len(pack_id) + len(generation)` (78 với ID 32 hex):

| Field | Kiểu | Giá trị |
|---|---|---|
| magic | 8 byte | `MILESPAK` |
| version | u32 | 1 |
| pack_id | id | |
| generation | id | |

**Blobs** — stored bytes nối liền từ offset `H`, không framing. Vị trí và độ dài nằm trong index.

**Index** — footer index; sidecar là **đúng các byte này**:

| Field | Kiểu | Giá trị |
|---|---|---|
| magic | 8 byte | `MILESIDX` |
| version | u32 | 1 |
| pack_id, generation | id, id | Bằng header và key |
| entry_count | u32 | 1..16384 |
| entries | entry × entry_count | Theo offset tăng dần |
| index_sha256 | 32 byte | SHA-256 mọi byte index trước nó |

| Entry field | Kiểu | Giá trị |
|---|---|---|
| domain | id | Domain của chunk |
| hash | 32 byte | SHA-256 raw |
| codec | u8 | 0 = raw, 1 = zstd |
| offset | u64 | Từ đầu pack |
| stored_len | u32 | > 0 |
| raw_len | u32 | > 0 |
| crc32c | u32 | CRC-32C (Castagnoli) của stored bytes |

**Trailer** — 24 byte cuối object:

| Field | Kiểu | Giá trị |
|---|---|---|
| index_offset | u64 | Offset byte đầu index |
| index_len | u32 | Gồm index_sha256; ≤ 2 MiB |
| version | u32 | 1 |
| magic | 8 byte | `MILESEND` |

12 byte cuối (version + magic) giữ vị trí ở mọi version sau để reader chọn parser. Generation ghi một lần trong header/index và áp cho mọi entry; location đầy đủ của entry là (pack_id, generation, offset, stored_len).

### 2.3 Bất biến

- `size = index_offset + index_len + 24`. Entry 0 bắt đầu ở `H`; entry i+1 bắt đầu ngay sau entry i; blob cuối kết thúc ở `index_offset`. Không gap, không overlap, không byte thừa sau entry cuối.
- (domain, hash) duy nhất trong pack. Cùng hash ở hai domain là hai entry và hai blob (fixture `mixed-domain`).
- raw: `stored_len = raw_len`, stored là raw. zstd: stored là đúng một frame RFC 8878, không dictionary, không byte sau frame, giải nén ra đúng `raw_len` byte; content checksum và single-segment tùy thư viện. Chọn raw/zstd theo policy (KT §19.1, W01-CHIEN-05); codec khác bị từ chối.
- Không ghi pack rỗng. Writer đóng pack ở target bytes (32–64 MiB, W02-HAI-02) hoặc ở 16384 entries — chặn sidecar khổng lồ khi chunk nén rất tốt; index tối đa 1933490 byte (khoảng 1,85 MiB) với domain và ID 64 ký tự.

### 2.4 Đọc và kiểm tra

- **GET một chunk** không đọc footer: `Chunks` cho (pack, generation, offset, stored_len, codec, raw_len); `RangeGet` → so CRC-32C → giải nén → kiểm `raw_len` và SHA-256 = hash trước khi phát byte (KT §6). CRC bắt lỗi trước khi tốn CPU giải nén; SHA-256 là kiểm tra identity cuối.
- **Đọc footer**: Range `bytes=-24` (Content-Range cho `size`) → kiểm magic, version, phương trình size → Range `[index_offset, index_offset+index_len)` → SHA-256 → parse và kiểm identity. Có thể đọc một suffix lớn hơn (ví dụ 64 KiB) để lấy cả index trong một request khi index nhỏ.
- **Scrub đầy đủ**: footer + header + từng blob như GET.
- **Sidecar** được kiểm độc lập (index_sha256 và identity theo key) vì pack Cold không đọc được. Khi pack đọc được, sidecar phải byte-identical với footer index; khác nhau là hỏng — footer đã kiểm là nguồn đúng, sidecar được ghi lại.

### 2.5 Ghi, ánh xạ model và rebuild

- Thứ tự: PUT pack, PUT sidecar, rồi mới insert/CAS `Chunks` (KT §5.1). Retry cùng generation phải gửi cùng bytes (`PackStore` trả conflict nếu khác). `PackInfo.Size` = size object.
- Entry ánh xạ thẳng sang `storage.Chunk`: `Key{domain, hex(hash)}`, `RawBytes = raw_len`, `Location{Pack: PackRef{pack_id, generation}, Range{offset, stored_len}, Codec}`. Test kiểm mọi entry golden qua `storage.Chunk.Validate`.
- Sidecar đủ để dựng inventory (domain, hash) → các bản sao vật lý mà không restore pack Cold. Sidecar **không** có: canonical winner, `location_revision`, refcount, namespace/manifest (journal/checkpoint R2, W01-PHONG-03), state/class/restore (HEAD S3). Rebuild không chọn bản sao chỉ vì sidecar có hash (KT §9.1).
- Quét orphan: list `packs/` cho PackRef từ key; Range `[0, H)` kiểm header khi cần.

## 3. Pages

### 3.1 Envelope

Mọi item `ManifestPages`, `UploadPartPages`, `CompletionPages` lưu một body:

| Field | Kiểu | Giá trị |
|---|---|---|
| magic | 8 byte | `MILESPAG` |
| version | u32 | 1 |
| kind | u8 | 1 object manifest, 2 upload part, 3 completion input, 4 completion snapshot |
| owner | id | manifest_id / part_manifest_id / completion_id của item |
| page_no | u32 | Từ 0, bằng sort key |
| payload_len | u32 | > 0 |
| payload | payload_len byte | Theo kind |
| page_sha256 | 32 byte | SHA-256 mọi byte trước nó |

Body tối đa **393216 byte (384 KiB)**; overhead envelope là `54 + len(owner)` byte. kind/owner/page_no phải khớp key item đã đọc, nên page đặt nhầm chỗ hoặc bị hoán đổi bị phát hiện.

### 3.2 Item và kích thước

Item DynamoDB chỉ gồm partition key (`manifest_id`, `part_manifest_id` hoặc `completion_id`, kiểu S), `page_no` (N) và `body` (B); không thêm attribute khác. Kích thước theo quy tắc DynamoDB (tên + giá trị, số 1 byte cho hai chữ số cộng 1) tối đa `16 + 64 + 7 + 3 + 4 + 393216 = 393310` byte, dưới 400 KB dù tính 400000 hay 409600 byte; test kiểm mọi page golden < 400000. Badger dùng key (bảng, owner, page_no) và value là body.

Page bất biến. Owner ID ngẫu nhiên chỉ thuộc một writer và chia greedy (§3.3) nên retry/takeover ghi lại đúng cùng bytes; BatchWriteItem (≤25 page/16 MB, retry hết UnprocessedItems, KT §10.2) hoặc Put có điều kiện đều giữ bất biến này, adapter chọn ở W02-HAI-03/W04-HAI-01. Không có transaction trên pages; mọi page phải bền và đủ trước commit.

### 3.3 Ref payload — kind 1 và 2

| Field | Kiểu | Giá trị |
|---|---|---|
| domain | id | Domain của mọi ref trong page |
| first_offset | u64 | Logical offset của ref đầu trong object/part |
| ref_count | u32 | ≥ 1 |
| refs | (32 byte hash + u32 raw_len) × ref_count | raw_len > 0; hash có thể lặp |

**Chia greedy, bắt buộc**: duyệt refs theo thứ tự, mỗi page nhận nhiều ref nhất còn vừa 393216 byte, rồi sang page mới; retry/takeover vì vậy sinh đúng cùng bytes. Số ref mỗi page là `floor((393149 − len(owner) − len(domain)) / 36)`: 10919 với owner 32 hex và domain `default` (fixture `one full page` / `one ref past a full page`).

### 3.4 Object manifest (`ManifestPages`)

Head `Objects` thêm các field (model hiện chưa có, W02-HAI-03 bổ sung):

| Field | Giá trị |
|---|---|
| `manifest_id` | Owner của pages, mới cho mỗi lần ghi/copy/complete |
| `size` | Logical bytes |
| `page_count` | 0..1024 |
| `chunk_count` | Tổng ref |
| `page_index` | Binary, `u64` first_offset của từng page, `8 × page_count` byte (≤ 8 KiB) |

- Object rỗng: `size = 0`, `page_count = 0`, `chunk_count = 0`, `page_index` rỗng, không ghi page; manifest_id vẫn duy nhất.
- Đọc toàn object: page i đọc từ key (manifest_id, i); domain = domain của head (bucket); `first_offset = page_index[i]` = tổng raw các page trước; cuối cùng tổng raw = size và tổng ref = chunk_count. Kết quả ánh xạ `storage.ManifestPage{ManifestID, Domain, Number, Chunks[{Key, RawBytes, LogicalOffset}]}`; test kiểm qua `ManifestPage.Validate`.
- Range `[a, b]`: tìm nhị phân page cuối có `page_index ≤ a`, đọc tiếp đến khi phủ b; không đọc cả manifest. Một page đầy đọc consistent tốn tối đa 96 RCU; object dưới khoảng 10 GiB với chunk 1 MiB chỉ có một page.
- **Giới hạn 1024 pages**: với chunking mặc định (min 256 KiB, object ≤ 100 GiB, ≤ 10000 part mỗi part thêm tối đa một chunk ngắn) có tối đa 419600 ref = 39 pages (fixture `most refs at default chunking`). Validator chunking (W01-CHIEN-05, W02-PHONG-01) phải giữ `chunking.min_bytes ≥ 16 KiB` để không vượt 1024 pages; writer vẫn từ chối manifest vượt giới hạn trước commit.

### 3.5 Upload part (`UploadPartPages`) và prepared manifest

- Kind 2, owner = `part_manifest_id`, offset tính từ đầu part. Head `UploadParts` giữ `part_manifest_id`, `size`, `page_count` (không page_index/chunk_count); reader kiểm offset liền và tổng raw = size. Part ≤ 5 GiB nên tối đa 2 pages ở chunking mặc định.
- Complete dựng **prepared manifest** là object pages mới (kind 1, manifest_id mới): refs của các part theo thứ tự trong input, first_offset cộng dồn qua part. Object không tham chiếu part pages; GET chỉ có một đường đọc. Prepared manifest ghi đủ trước transaction Completed.

### 3.6 `CompletionPages`

Mỗi lần freeze có hai stream bất biến, mỗi stream một `completion_id` (32 hex ngẫu nhiên) làm owner:

| Stream | Kind | Nội dung | Digest | Ghi lúc |
|---|---|---|---|---|
| Completion input | 3 | Byte canonical input v1 của API 4.3.1 | `input_digest` | Trước CAS Active → Completing |
| Frozen part snapshot | 4 | Snapshot v1 bên dưới | `snapshot_digest` | Bước 2 mục 4.3, sau khi publisher quiesce |

- Chia theo byte: page i chứa đoạn `[i·c, (i+1)·c)` của stream với `c = 393162 − len(owner)` (393130 với owner 32 hex); không căn theo record. Đọc: nối payload page 0..n−1, kiểm độ dài và SHA-256 với pointer.
- `Uploads` giữ cho mỗi stream: `completion_id`, `length`, `page_count`, digest hex; cộng prepared manifest (`manifest_id`, `size`, `page_count`, `chunk_count`, `page_index` như head). Digest là SHA-256 của cả stream trước khi chia: `input_digest` của golden `cli-cp-multipart` trong `multipart.json` bằng digest các page input (test kiểm chéo).
- Mở lại Active sau lỗi validation thì freeze sau dùng completion_id mới; stream cũ chỉ còn là rác chờ GC sau khi hết là root.

**Snapshot v1**:

| Field | Kiểu | Giá trị |
|---|---|---|
| part_count | u32 | 1..10000 |
| records | record × part_count | PartNumber tăng nghiêm ngặt (thứ tự input) |

| Record field | Kiểu | Giá trị |
|---|---|---|
| part_number | u32 | 1..10000 |
| revision | u64 | ≥ 1, revision part head đã freeze |
| size | u64 | ≤ 5 GiB |
| page_count | u32 | 0 khi và chỉ khi size = 0 |
| etag | 16 byte | MD5 nhị phân của part |
| part_manifest_id | id | |
| checksum | u8 độ dài + byte | Checksum đã verify lúc UploadPart: 4 (CRC32/CRC32C), 8 (CRC64NVME), 20 (SHA1), 32 (SHA256); cùng độ dài trong một snapshot |

10000 part: input khoảng 1,2 MB = 4 pages, snapshot khoảng 1,06 MB = 3 pages (fixture).

### 3.7 Transaction

Pages, sidecar và pack không nằm trong transaction (KT §9.0). Định dạng này chỉ thêm vào item trong transaction: head (`manifest_id`, `size`, `page_count`, `chunk_count`, `page_index` ≤ 8 KiB) và `Uploads` (hai pointer, digest, prepared manifest ≤ 8 KiB). Mỗi item dưới 400 KB nên transaction với tối đa 9 action luôn dưới 4 MB; contract hiện tại có tối đa 8 action (head, Users, QuotaReservations, Outbox, condition Credentials/Buckets/BucketGrants, thêm Uploads khi Complete), xa giới hạn 100 action. Danh sách action chính xác chốt ở W01-CHIEN-05.

## 4. Golden fixtures

| File | Nhóm | Nội dung |
|---|---|---|
| `pack.json` | packs | `small` (raw + zstd), `mixed-domain` (cùng hash ở `default` và `tenant-b`), `large` (2048 entries, index > 64 KiB), `max entries` (16384); bytes chính xác (`pack_hex`) cho hai pack nhỏ, SHA-256 pack/sidecar cho mọi pack |
| `pack.json` | invalid, corrupt | Pack rỗng, 16385 entries, trùng domain/hash, ID/domain sai, chunk rỗng, codec lạ, raw sai độ dài, frame zstd ngắn, hash sai; trailer cắt/sai version/sai offset, byte index/blob/header lật, đọc nhầm generation, sidecar cắt/nhầm pack/0 entry/thiếu entry_count/codec lạ/gap/raw sai độ dài, footer để byte ngoài entry trước index |
| `pages.json` | manifests | `empty object` (0 page), `small object` (hash lặp), `upload part` (`tenant-b`), biên 10919/10920 ref, `most refs at default chunking` (419600 ref, 39 pages) |
| `pages.json` | streams | Input `cli-cp-multipart`, input 10000 part (4 pages), snapshot upload `full` của `multipart.json`, snapshot 10000 part (3 pages) |
| `pages.json` | invalid, corrupt | raw_len 0, domain sai, snapshot sai thứ tự/lẫn algorithm/part rỗng có page; byte lật, byte thừa, đọc nhầm owner/kind, hoán đổi/thiếu page, head sai size/chunk_count/domain/page_index/1025 pages, stream thiếu page/sai digest/sai độ dài |

`internal/pack/format_test.go` và `internal/manifest/pages_test.go` encode lại mọi golden và so bytes, đọc lại qua reader tham chiếu, ánh xạ sang model `internal/storage`, rồi kiểm từng case invalid/corrupt dừng đúng stage. Frame zstd trong fixture là frame RLE một block; reader tham chiếu chỉ giải các frame raw/RLE single-segment đó, codec production dùng decoder zstd đầy đủ. Kiểm ngoài Go: các frame được libzstd (Bun 1.4.3 `zstdDecompressSync`) giải ra đúng raw; một parser JavaScript viết riêng từ bảng ở §2–3 đọc `pack_hex` và `page_hex` cho cùng CRC-32C, SHA-256, offset và digest.

```bash
go test ./internal/pack ./internal/manifest
make check
```

## 5. Bàn giao và freeze

| Task | Việc |
|---|---|
| W02-HAI-02 | Codec production cho pack/footer/sidecar, key S3 §2.1, đóng pack 32–64 MiB/16384 entries; chạy `pack.json` rồi xóa reference trong `format_test.go`; review và freeze format cuối tuần 2 |
| W02-HAI-01 | Zstd theo quy tắc frame §2.3; decoder đầy đủ phải giải các frame fixture |
| W02-HAI-03, W04-HAI-01 | Thêm field head/Uploads §3.4–3.6 vào model, item layout và cách ghi page §3.2 cho Badger/DynamoDB; chạy `pages.json` rồi xóa reference trong `pages_test.go` |
| W02-CHIEN-02, W04 multipart | Ghi input/snapshot stream và prepared manifest theo §3.5–3.6 |
| W02-PHONG-02 | Read path: CRC-32C → giải nén → SHA-256, Range qua `page_index` |
| W01-PHONG-03 | Sidecar chỉ cho inventory vật lý (§2.5); journal có thể nhúng nguyên body page |
| W01-CHIEN-05 | `chunking.min_bytes ≥ 16 KiB`, codec raw/zstd, danh sách action transaction |

Sau freeze, format chỉ đổi bằng version mới, migration và review; fixture v1 giữ nguyên để reader còn đọc dữ liệu cũ.
