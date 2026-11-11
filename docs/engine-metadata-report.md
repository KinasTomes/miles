# Engine, metadata và cách diễn giải bằng chứng lưu trữ

## Phạm vi và nguồn kiểm chứng

Tài liệu mô tả implementation trên base `224ae15`, gồm Warm write/reuse,
revival, cross-domain copy, transactional metadata và GC/recovery. Đây là phần
giải thích kỹ thuật để dùng trong báo cáo; các con số benchmark phải dẫn tới
một run/artifact riêng. Tham số mặc định dưới đây là cấu hình, không là kết quả đo.

Các [contracts](contracts/shared-storage.md) chốt giao diện/định dạng. Một số
đoạn mô tả lịch sử trong contracts hoặc code comments còn ghi thời điểm chưa có
implementation; khi đánh giá chức năng hiện tại phải đối chiếu call path và
regression test, không lấy trạng thái lịch sử làm kết luận cho release mới.

## 1. Chunk và dedup domain

Body được đọc streaming, chia bằng FastCDC và hash SHA-256 trên **raw bytes**.
Mặc định min/avg/max là 256 KiB / 1 MiB / 4 MiB. Validator cho phép các cấu hình
hợp lệ trong giới hạn implementation; vì vậy báo cáo phải lưu policy revision
và document thực tế thay vì giả định mọi run dùng default.

Identity là `(domain, hash)`, không phải hash riêng lẻ. Hai owner có thể dùng
chung domain nếu policy cho phép. Cùng bytes ở hai domain khác nhau tạo hai
identity và không được dùng source-domain key để công bố destination copy.
Đây là ranh giới tái sử dụng dữ liệu, không phải tuyên bố domain là một cơ chế
mã hóa hay đã loại mọi timing side-channel.

- Code: [chunker](../internal/engine/chunker.go),
  [pipeline](../internal/engine/write/pipeline.go),
  [cross-domain copy](../internal/object/copy.go).
- Bằng chứng: [chunking vectors](../tests/fixtures/engine/chunking.json),
  `TestPipelineBatchesReusesAndKeepsDomainsSeparate` trong
  [pipeline tests](../internal/engine/write/pipeline_test.go),
  [copy tests](../internal/object/copy_test.go).

Chunk boundary ổn định khi vùng dữ liệu không đổi giúp tái sử dụng giữa các bản
backup. Tuy nhiên, file smoke khoảng 64 KiB nhỏ hơn min chunk mặc định: sửa 1%
file có thể đổi hash của cả chunk. Smoke kiểm tra correctness và quy trình xuất
artifact; hiệu quả CDC với thay đổi nhỏ cần workload lớn riêng.

## 2. Nén và lựa chọn representation

Hash không phụ thuộc codec. Chunk mới/revived có thể lưu raw hoặc zstd. Mặc định
zstd level 3 và yêu cầu tiết kiệm ít nhất 10%; quyết định dùng số hữu tỷ chính
xác để tránh sai số ở biên. Tỷ lệ tiết kiệm khi chọn codec là
`1 - stored_len/raw_len`, khác với compression ratio dùng trong báo cáo.

Writer kiểm tra decode/hash trước khi đưa dữ liệu encoded vào pack; reader kiểm
stored length, CRC, decode bounds và raw SHA-256. Dữ liệu entropy cao hoặc đã
nén có thể giữ raw. Encoder failure có đường fallback và trạng thái kiểm chứng
riêng; không được ghi nhận một phép nén chưa thành công là saving.

Retry immutable pack phải gửi lại đúng bytes của generation đã chọn. Không
re-encode rồi mặc định zstd luôn tạo cùng bytes qua mọi toolchain/dependency.

- Code: [compression](../internal/engine/compression.go),
  [pack verification](../internal/pack/pack.go).
- Bằng chứng: [compression vectors](../tests/fixtures/engine/compression.json),
  [compression tests](../internal/engine/compression_test.go),
  [reader verification tests](../internal/readpath/verification_harness_test.go).

## 3. Pack, canonical location và race

Pipeline lookup theo lô; missing key và unprocessed/throttled lookup phải được
phân biệt. Một batch chưa xử lý đủ không được coi là complete miss. Chunk trùng
trong cùng request được tái sử dụng mà không tạo thêm một blob cho mỗi occurrence.

Pack v1 có target 48 MiB, trần 64 MiB và tối đa 16.384 entries. Nó còn đóng khi
body kết thúc, nên dataset nhỏ có thể tạo nhiều pack nhỏ và overhead cao. Pack
chứa payload cùng thông tin index/footer; sidecar là artifact bền vững riêng.
Không đồng nhất tổng canonical payload range với toàn bộ pack+sidecar bytes.

Thứ tự công bố là đăng ký Uploading dưới writer authority, ghi immutable pack
và sidecar, xác nhận Warm, rồi insert/CAS canonical location. Location gồm exact
pack generation, range, codec và CRC; location revision bảo vệ race khi chuyển
winner. Manifest không được dùng một location chưa đủ điều kiện durability/read.

Writer thua insert/CAS có thể để lại blob không được tham chiếu. Blob đó là
physical overhead/orphan candidate, không tự trở thành object và không biến mất
khỏi thống kê chỉ vì không còn canonical. GET/Copy đang giữ view cũ tiếp tục
được bảo vệ dù canonical location đã chuyển.

Bloom/LRU chỉ hỗ trợ tìm ứng viên. Quyền reuse phải đi qua kiểm tra location,
pin, fence và deadline; cache không thay metadata authority.

- Code: [publisher](../internal/engine/write/publisher.go),
  [session](../internal/engine/write/session.go),
  [coordination adapter](../internal/engine/write/coordinated.go),
  [pack format](../internal/pack/pack.go).
- Bằng chứng: [pack fixture](../tests/fixtures/format/pack.json),
  [publisher tests](../internal/engine/write/publisher_test.go),
  [native GET/Copy/GC flows](../internal/app/warm_gc_native_test.go).

## 4. Manifest, head và atomic metadata publication

Manifest pages mô tả ordered chunk references. Pages bền vững trước khi commit
head; head là điểm công bố object cho GET/HEAD/LIST. Pack, chunk row hoặc page
đã ghi mà chưa có committed head không được coi là live object.

Overwrite tạo revision mới theo expected head. Delete công bố tombstone.
Bucket incarnation ID phân biệt bucket được tạo lại dù giữ cùng tên. Reader
dùng head/view đã bind; không ghép pages hoặc ranges của hai revision.

Badger gom metadata trong transaction cục bộ. DynamoDB dùng native conditional
transaction, bao gồm authority/observed-row conditions, head, accounting, outbox
và outcome/receipt theo loại operation. Giới hạn 400 KiB/item, 100 actions và
4 MiB aggregate transaction là các giới hạn cần kiểm tra trên serialized plan;
không suy action budget từ sơ đồ transaction đơn giản trong tài liệu lịch sử.
Manifest/part/completion pages được phân trang để tránh nhét toàn bộ nội dung
vào một item hoặc transaction Complete.

Retry giữ logical operation identity và tra outcome theo `op_id`. Timeout không
chứng minh transaction chưa áp dụng. Outbox/journal mang authoritative mutations
cho recovery; refcount derived không thay thế head/root proof.

- Code: [manifest codec](../internal/manifest/codec.go),
  [Badger commit](../internal/metadata/badger/commit.go),
  [DynamoDB commit](../internal/metadata/dynamodb/store_commit.go),
  [DynamoDB transaction](../internal/metadata/dynamodb/store.go),
  [limits](../internal/metadata/dynamodb/limits.go).
- Bằng chứng: [commit vectors](../tests/fixtures/engine/commit.json),
  [metadata contracts](../internal/storage/contracttest),
  [serialized boundary tests](../internal/metadata/dynamodb/limits_test.go),
  [unknown outcome regression](../internal/metadata/dynamodb/review_regression_test.go).

## 5. Owner logical quota

Quota tính logical usage của live heads thuộc owner, không tính physical bytes
của pack, codec, orphan, retained versions hoặc cache. Dedup giữa owner không
làm owner thứ hai được ghi một object logical miễn phí.

Reservation bảo vệ phần tăng usage. Same-size overwrite ở quota đầy sử dụng
overwrite credit nhưng phải kiểm lại head/revision đã cấp credit khi commit.
Head đổi giữa reserve và commit không được tái dùng credit cũ. Multipart còn có
staging và publisher/part accounting; không cộng tổng attempted upload bytes
vào live usage. Complete, abort, replacement và retry phải giải ledger theo
operation identity để không trừ/cộng lặp.

- Code: [DynamoDB reservation](../internal/metadata/dynamodb/store_reservation.go),
  [quota](../internal/metadata/dynamodb/store_quota.go),
  [multipart bridge](../internal/metadata/dynamodb/multipart_bridge.go).
- Bằng chứng: [metadata contract suite](../internal/storage/contracttest),
  [Complete tests](../internal/object/complete_test.go),
  [multipart tests](../internal/object/multipart_test.go).

## 6. Cold, revival, copy và protection

Warm/IR reuse, body-based revival và cross-domain copy có quyết định khác nhau.
Revival giữ identity trong domain đích nhưng tạo Warm representation mới; archive
cũ có thể vẫn tồn tại do retention/recovery. Same-domain copy có thể lặp references;
cross-domain copy phải đọc/verify nguồn và dựng destination-domain identities.
Nguồn archive chưa đủ điều kiện đọc phải qua restore/admission phù hợp.

GC đánh dấu từ live heads, unfinished uploads, read/writer pins và recovery cuts.
Tuổi hoặc refcount riêng không đủ quyền xóa. Compaction Warm tạo destination mới,
CAS location và giữ generation cũ đến khi hết mọi bảo vệ. Unknown physical outcome
giữ gate/protection; trusted terminal provider và đúng original StepID mới cho
phép reconcile/takeover. Không suy terminal từ timeout hay HEAD đơn lẻ.

- Code: [copy chunk](../internal/engine/write/copy_chunk.go),
  [marker](../internal/lifecycle/gc_mark.go),
  [runtime provider](../internal/app/lifecycle_terminal.go),
  [S3 terminal evidence](../internal/storage/s3/lifecycle_terminal.go).
- Bằng chứng: [revival tests](../internal/engine/write/revival_policy_test.go),
  [native provider tests](../internal/app/lifecycle_terminal_native_test.go),
  [GC/recovery validation](testing/gc-recovery-validation.md).

## 7. Đọc stats/layout và báo cáo tradeoffs

Theo [storage statistics contract](contracts/storage-statistics.md), stats là
observation bounded/approximate. Logical lấy live heads; unique lấy từng live
`(domain, hash)`; physical gồm mọi listed retained version và control artifact
trong dedicated physical bucket. Layout có integrity metadata-only, không chứng
minh mọi blob đã được tải và hash-verify.

`dedup_ratio = logical / unique_raw`, `compression_ratio = unique_raw / unique_stored`,
`total_reduction = logical / listed_physical`. Mẫu số 0 trả null. Physical thiếu
coverage không được chuyển thành 0. Bucket unique totals có thể chồng lấn; chỉ
allocated physical cộng với global unallocated mới có invariant cộng tổng.

Revival có thể giữ nguyên dedup ratio nhưng tăng physical bytes. Pack nhỏ/control
bytes có thể làm total reduction dưới 1. Không lấy storage ratio để khẳng định
cost saving: request, DynamoDB, compute, network route, minimum storage duration
và temporary restore billing có đơn vị/cửa sổ riêng. Simulator là projection từ
input/pricing đã pin, không là hóa đơn AWS.

Dataset manifests và điều kiện workload được mô tả trong
[dataset procedure](../benchmarks/datasets/README.md) và
[baseline](../benchmarks/baseline.md). Cách tạo và kiểm chứng snapshots nằm trong
[artifact procedure](../benchmarks/engine-metadata-artifacts.md). Số đo lịch sử
phải giữ source commit/backend/scope gốc; không gắn sang release hiện tại.
