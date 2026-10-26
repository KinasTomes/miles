# W03-PHONG-01 — Warm GET/Range reader

Trạng thái: implementation và evidence để review; chưa sign-off Miles, chưa
freeze API. Baseline cuối `9a800ed` (main đã merge PR #31 và PR #28 của Hải).
PR #28 được fast-forward trong lúc triển khai, không sửa shared contract để né
validation mới; tombstone fixture tuân thủ yêu cầu không mang ETag/metadata.
Task chính thức:
[W03-PHONG-01](https://miles.extase.dev/tasks/W03-PHONG-01).

## Phạm vi bàn giao

`readpath.NewWarmReader` ghép Badger MetadataStore, physical S3 adapter, manifest
planner, physical LRU, shared policy evaluator và scheduler/admission. CRC từ
canonical metadata đi qua Badger serializer có kiểm presence; zero là giá trị
hợp lệ. Không suy expected CRC từ payload hoặc suy read rights từ cache.

Luồng:

```text
Authorize + bucket incarnation + pinned policy
  -> local resolving guard -> authoritative head/root -> manifest plan
  -> bounded canonical batches -> exact-generation pins -> revision recheck
  -> metadata eligibility for the entire requested Range
  -> admitted backend HEAD preflight for the entire physical set
  -> Ready(response metadata)
  -> per-occurrence reserve -> revalidate -> backend HEAD -> LRU/GetRange
  -> production ReadBlob -> best-effort verified cache fill
  -> revalidate -> ordered verified logical slices
  -> join workers / emitter return -> release reservations -> release pins
```

Reader giữ đủ logical occurrences, kể cả repeated ChunkKey. Range là span đã
normalize; HTTP parsing thuộc API consumer. Plan phải khớp committed descriptor
đối với object không rỗng. Manifest có giới hạn pages/chunks explicit trước load;
payload và decoder dùng admission riêng. Metadata batches tối đa 100 occurrences.
Một active-request ticket được lấy ngay sau authorization/policy binding, trước
head/manifest resolution, và giữ xuyên preflight/streaming đến cleanup. Nhờ vậy
active-request limits cũng giới hạn số manifest plans đồng thời; không chỉ số
requests đang fetch payload. `RunReadAhead` giữ public behavior, dùng chung helper
chạy trên ticket đã admit để không double-admit một Warm request.

## Đối chiếu checklist trên Miles

| Checklist | Implementation | Evidence |
|---|---|---|
| C1: adapters thật, read/decompress/verify, Range qua chunk | `warm.go`, `mapping.go`, `verify.go`; Badger + S3 + pack codecs hiện có | `TestWarmReaderRealAdaptersFullRangeCacheAndPurge`, `TestWarmReaderRangeAcrossGreedyManifestPages`, `TestWarmReaderActualSDKTransport` |
| C2: LRU/read-ahead/admission, generation/auth order, cache disposable | `warm.go`, `coordination/warm.go`; node/bucket/request accounting, exact physical keys | denial/deadline/Cold-before-cache, generation isolation, concurrent admission/read-ahead, purge/refetch và cancellation tests |
| C3: full/Range checksum, corruption giữa stream abort + metric | CRC32C → decode → raw length → SHA-256 qua pack verifier, fixed-cardinality `WarmMetrics` | full/Range đối chiếu byte/SHA oracle; `TestWarmReaderCorruptMiddleAbortsAndCounts`, backend fault tests |

`TestWarmReaderCanonicalHashMismatchWithValidStoredCRC` kiểm SHA-256 riêng:
canonical row và manifest có identity sai nhưng stored length/CRC hợp lệ, reader
phải fail tại content verification, không emit/cache bytes đó. Không dùng CRC
để thay SHA hoặc SHA để thay stored CRC.
`TestWarmReaderWholeRangeArchivePreflight` đặt pack thứ hai ở backend Glacier
trong khi metadata Warm: phải fail trước mọi cache lookup/payload GET/emit.
`TestWarmReaderSingleTicketAcrossReadyAndStreaming` kiểm request slot không bị
nhả giữa preflight và streaming.

Test dùng production pack writer/compressor, metadata persistence và production
verifier; không dùng fixture-comparison verifier làm bằng chứng. SDK seam mô phỏng
remote object service. Một test bổ sung dùng AWS SDK HTTP client thật đến
`httptest` backend. Đây không phải evidence deploy AWS hoặc distributed correctness.

## DECODER-MEMORY-V1

Consumer gọi `pack.EstimateDecodeMemory` metadata-only, không nhân bản công thức
workspace. Charges của một occurrence:

- `StoredBytes = S` cho buffer `FetchRange` có capacity S.
- `RawBytes = OutputBytes`, `WorkspaceBytes = WorkspaceBytes` của estimator.
- `IOBytes = S + TransportBytes`, một I/O slot; runtime cung cấp bound transport
  đã audit, không có production default.
- `CopyBytes = 2*(S+8192)` bao hai copy envelopes của LRU trên Go đã pin: append
  từ nil cấp phát theo length rồi làm tròn size class/page; đây không phải công
  thức decoder. LRU resident-byte capacity vẫn là ngân sách riêng.

Cache hit dùng cùng conservative estimate và verify lại. Full occurrence
reservation được giữ đến synchronous decode/Close, worker quiescence và emitter
trả về. Physical HEAD preflight cũng reserve trước I/O và release sau khi call
trả về; reservation scheduler bao lần fetch/decode/emit tiếp theo.

Không dùng benchmark allocations làm hard bound; không hứa bound RSS. Runtime
cần headroom cho allocator, garbage, shared pools và HTTP/TLS. `TransportBytes`
phải xuất phát từ transport cụ thể được runtime giới hạn/audit; một số dương do
caller chọn không tự chứng minh bound. Test SDK HTTP chỉ chứng minh execution
path, không chứng minh peak transport memory của mọi AWS deployment.

| Boundary/lifetime | Test |
|---|---|
| no-FCS, required workspace −1 / exact / +1 | `TestWarmReaderNoFCSAndExactWorkspaceBoundary` |
| chunk zstd 16 MiB, Range một byte vẫn verify toàn chunk | `TestWarmReaderMaximumChunkUsesAuditedEstimator` |
| insufficient decoder budget không fetch payload | `TestWarmReaderRejectsDecoderCostBeforePayload` |
| W=0/W>0, K=2 và concurrent same-bucket admission | full/range suite và `TestWarmReaderConcurrentAdmissionAndReadAhead` |
| cancel trong backend đang block | `TestWarmReaderCancellationQuiescesBeforeRelease` |
| emitter lỗi nhưng còn workers/output | `TestWarmReaderEmitterFailureRetainsPinAndCleansUp` |
| epoch-independent monotonic expiry, equality, domain mismatch | gate tests và expiry-during-fetch test |

Scheduler đã có deterministic out-of-order execution tests riêng. Warm reader
concurrency test chứng minh bounded outstanding work và ordered output, không
tuyên bố goroutine execution order deterministic.

## Generation và lifetime trong MVP một node

`coordination.WarmGate` là boundary một process, giữ resolving guard đến khi pin
đủ requested generations. Bind root không tự pin mọi generation. Pin và local
retirement admission dùng cùng critical section; không giữ global lock suốt GET.
Copied handles chia sẻ private lifecycle, duplicate release không double decrement.

Runtime phải dùng **một gate chung** và không cho physical maintenance bypass gate.
Local pins chỉ bảo vệ đối với actors tuân theo boundary này. Manifest Badger hiện
immutable và chưa có manifest reclamation port. Không được bật multi-node GC,
tiering hoặc destructive maintenance dựa vào local gate. Durable R1 lease/fence,
service incarnation/recovery và maintenance coordination tiếp tục ở
W03-PHONG-02; không dùng W2 fake coordinator làm production authority.

Không khởi tạo gate mới khi worker của gate cũ còn sống. Shutdown cần cancel,
join toàn bộ reads trước khi đóng store hoặc thay runtime. Cancellation không
được tự release generation đang được backend/decoder/emitter sử dụng.

## Bàn giao API và observability

- API consumer W03-CHIEN-02 gọi `Read`, nhận metadata qua `Ready`, normalize
  Range và xử lý headers theo API contract. `Ready` không phải lệnh commit HTTP
  headers; admission/fetch vẫn có thể fail sau đó.
- Emitter chỉ mượn verified slice đến khi trả về. Consumer muốn giữ bytes phải
  copy và tự account ownership. Reader chỉ emit requested slices, không speculative
  raw bytes ngoài Range.
- Lỗi sau khi đã phát prefix phải terminate stream; không thể thu hồi prefix
  hoặc viết thêm XML error như một response thành công. Corrupt chunk không emit
  partial raw bytes và không được cache fill. Corrupt cache evict exact generation,
  không fallback sang generation khác.
- Mount `WarmMetrics` trên runtime metrics mux. Counter names dùng prefix
  `miles_warm_read_` và suffix `_total`: requests, aborted, integrity_failures,
  cache_hits, cache_misses, emitted_bytes, cache_errors. Không có user/object labels.
  emitted_bytes đếm slices mà emitter nhận thành công, không đo TCP delivery.
- Bucket policy là immutable snapshot có revision/incarnation do caller pin.
  Không giả lập bucket policy persistence; phần đó thuộc W04.

## Validation

```text
go test -count=20 ./internal/readpath/... ./internal/coordination/...
go test ./...
go vet ./...
go test -race ./internal/readpath/... ./internal/coordination/...
git diff --check
```

Kiểm gofmt/whitespace cả file mới untracked. CI race suite được bổ sung
`internal/coordination/...`; workflow hiện diện không đồng nghĩa CI đã PASS cho
bản local. Không đánh dấu Miles Done trước review độc lập/sign-off.

Kết quả local ngày 26/10/2026 trên baseline `9a800ed`:

| Kiểm tra | Kết quả |
|---|---|
| Readpath/coordination tests ×20 | PASS |
| `go test ./...` | PASS |
| `go vet ./...` | PASS |
| gofmt và whitespace, gồm file mới | PASS |
| Race detector local | NOT VERIFIED — GCC/cgo Windows không hỗ trợ 64-bit |

Tests Badger và HTTP localhost chạy ngoài sandbox do sandbox chặn rename file
tạm và socket. Chưa có kết quả CI cho phần code local chưa push này.
