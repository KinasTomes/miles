# Read path — implementation, evidence và integration gates

## Trạng thái sau khi tích hợp Badger metadata

W02-PHONG-02 đã được nghiệm thu phạm vi prototype fake metadata/S3. Protected reader và C3 production tiếp tục ở W3; không thêm dependency ngược để chặn nghiệm thu W2.

- Production primitives: `admission.go`, `readahead.go`, `fetch.go`, `verify.go`, `mapping.go`; Physical LRU nằm trong `internal/cache`, manifest/range planner trong `internal/engine`.
- `MapCanonicalRange` kiểm ChunkKey/raw length, expected location revision, exact PackRef/generation và range trong pack size trước khi chuyển sang `pack.Entry`/`CacheKey`. Đây là kiểm tra snapshot metadata, không phải atomic pin, authority token hoặc CRC presence proof.
- Planner đối chiếu PageCount/ChunkCount/PageIndex khi head có descriptor. Legacy head không có descriptor vẫn chỉ có structural validation. DescriptorVerified không chứng minh page authenticity hoặc R1 protection; empty all-zero descriptor không được nâng thành evidence nguồn độc lập.
- Badger PR #23 đã kiểm CRC field presence bằng pointer khi decode: missing/null bị reject, zero hợp lệ. Evidence nằm trong `internal/metadata/badger/db.go`, `durability_test.go` và shared canonical CAS suite. Không suy rộng guarantee này sang mọi MetadataStore hoặc R2 serializer.
- `verified_scheduler_test.go` dùng production writer/verifier với fixture bindings; `reader_integration_test.go` cũ dùng test double. Hai loại evidence không tương đương. Decoder cost của fixture dùng `pack.EstimateDecodeMemory`; transport/cache copies vẫn là fixture charges, chưa phải bound cho backend production.

### Các gate còn chặn orchestration

1. **R1 / Phong và Hưng:** chưa có concrete Read View, atomic root bind, exact generation pins, revision validation cùng retirement và release/quiescence. `MetadataStore.GetChunks` không thay thế pin. Không xây authority boolean hoặc chuyển sang triển khai task coordinator trong task này.
2. **Decoder / Hưng:** W03-HUNG-04 bàn giao `pack.EstimateDecodeMemory`, source-derived bounds, ownership và lifetime trong [decoder-memory.md](../../docs/contracts/decoder-memory.md). Scheduler harness dùng estimator thật và kiểm admission/cancellation. W03-PHONG-01 tích hợp vào production executor, cộng fetch/cache capacities và giữ reservation/pins tới quiescence; không dùng lại allowance 1 MiB của fixture cũ.
3. **Orchestration consumer:** phải Authorize (bao gồm Read=true và bucket incarnation), acquire/bind/pin, preflight toàn requested Range bằng shared policy trước cache/I/O, rồi revalidate trước emit. Chưa có production entrypoint nối toàn chuỗi đó; evidence authorization trước cache của prototype không thay R1/pins production.
4. **Bucket cache:** pinned bucket policy điều khiển read-ahead; resource quotas node/bucket/request đã có. `admit_on_write=false` không tắt read cache. Cache identity vẫn physical và có thể chia sẻ giữa bucket sau authorization; chưa thêm quota/default hoặc admission policy không có trong contract.

Race suite Linux trong CI đã được bổ sung `internal/readpath`/`internal/cache`. Không coi workflow tồn tại là check đã PASS cho branch; workflow chỉ chạy khi push main hoặc có pull request.

## Hồ sơ Phase 3A.0 (lịch sử)

Phần Phase 3A.0 bên dưới là **test-only harness**. Các tuyên bố pending của phần lịch sử chỉ áp dụng cho harness/category adapter, không phủ nhận production verifier `internal/pack` và lower-level adapter đã bổ sung sau đó.

## Evidence và giới hạn

- `verification_fixtures_test.go`: 23 vectors với stored bytes, raw oracle, expected metadata, test budgets và expected category riêng biệt.
- `verification_harness_test.go`: self-check các dữ kiện của vectors. Raw/zstd valid dùng lại `small` trong `tests/fixtures/format/pack.json`; không tạo pack codec mới. Zstd được đối chiếu bằng thư viện đã pin, không viết strict decoder tham chiếu thay Hải.
- `io_harness_test.go`: bọc shared `fake.Packs` để ghi exact range/call count và inject short response, oversized response, backend error, cancellation tại channel barrier. Fake pack chỉ có 8 bytes; fault wrapper giới hạn đọc fixture. Không có authority/pins giả.
- `TestProductionVerificationPending` được SKIP có lý do. Helper `runVerificationContract` chỉ dành cho adapter thật sau này, hiện chưa được gọi. Self-check PASS không chứng minh verifier reject đúng, allocation cap được enforce hoặc reader biết xử lý I/O faults.

## Vectors và thứ tự

Expected order của byte verification: **stored length → CRC-32C → decode → raw length → SHA-256**. Resource/metadata prechecks phải diễn ra trước allocation khi có thể. Các categories trong harness là vocabulary test đề xuất, không mở rộng shared error taxonomy.

Có valid raw/zstd và nonempty CRC-zero; stored tamper, wrong stored length/CRC/raw length/hash, unsupported codec, truncated frame, trailing byte, concatenated frames, output vượt expected raw length, explicit stored/raw budgets; thêm vectors kiểm length-before-CRC, CRC-before-decode và raw-length-before-hash.

Raw chunk rỗng không hợp lệ theo Pack/Pages §2 và Engine chunk contract: `raw_len > 0`. Object rỗng không có chunk, đã thuộc planner tests; vì vậy không tạo valid empty-chunk vector.

CRC-zero payload là `ab9be09b` (hex), 4 bytes. Generator giải ánh xạ affine CRC-32C trên GF(2), rồi kiểm bằng Go Castagnoli table và oracle bitwise độc lập với polynomial phản chiếu `0x82f63b78`, init/xorout chuẩn. Không dùng checksum bằng 0 làm missing marker.

Malformed zstd vectors được cập nhật stored length/CRC khi cần để lỗi đi tới frame/decode stage. Vector output-limit dùng frame golden giải ra 4096 bytes nhưng expected raw length=1: nhỏ, hữu hạn, không tạo bomb. Toàn bộ test budgets 8192 bytes chỉ phục vụ fixtures, không phải production defaults. Các negative outcomes hiện là **expected**, chưa là actual verifier evidence.

## Handoff cần review

- Hải/Hưng: authoritative CRC presence, zero hợp lệ, schema/version/persistence; checksum không được suy từ SHA-256.
- Hải: API verification tái sử dụng sau khi PR #22 được chấp nhận; strict single-frame/trailing/dictionary handling và output cap theo raw_len, kể cả frame không có FCS.
- Consumer: adapter chuyển actual errors sang categories; kiểm allocation/resource limits bằng instrumentation phù hợp, không chỉ assert error sau decode.
- Phong/coordinator consumer: exact pinned target, authority/deadline checks và cache integration ở phases sau. Harness này không chứng minh distributed correctness.

Chưa cần AWS hoặc Cloud cluster. Không có backend fallback/quarantine, read orchestration, policy defaults hoặc API freeze trong Phase 3A.0.

## Xử lý review H-R01/H-R02/H-R03

- Positive codec self-check dùng pure helper: raw/CRC-zero phải là `raw`, zstd phải là `zstd`. Mutation tests đảo codec của cả ba positives và yêu cầu helper reject.
- Inventory được kiểm trước `t.Run`: đúng 23 vectors, tên không rỗng/trùng và đủ required baseline/new names. Có mutation tests cho thiếu vector, tên trùng/rỗng hoặc thay baseline bằng tên lạ; filter subtest không thay kết quả inventory.
- Thêm 5 zstd probes; length và CRC luôn được cập nhật để không che mất header/decode/resource premise:
  - Dictionary ID=7: descriptor `0x61`, ID hiện diện, FCS=4096 và RLE body đầy đủ. Không cung cấp dictionary; ID không đồng nghĩa dictionary đã được cung cấp. Expected category `decode` vẫn chỉ là test vocabulary.
  - Window: non-single-segment, WD=`0x78`, window=32 MiB; FCS/body vẫn 4096. Kiểm header-only, không decode frame này.
  - Content: single-segment FCS=16 MiB+1; 129 RLE blocks hợp lệ biểu diễn đúng tổng đó trong 525 stored bytes. Chỉ kiểm layout, header và tổng block sizes, **không decode hoặc allocate output 16 MiB**. Canonical expected content vẫn 4096 zero bytes: untrusted frame cố vượt cả expected output và hard content bound; resource rejection phải xảy ra trước raw-length/hash checks. Đây không phải vector chỉ sai CRC hay truncated header.
  - No-FCS: descriptor `0x00`, WD=`0x10` (4 KiB), không dictionary/FCS, RLE body giải ra 4096 bytes, metadata expected raw length=1. Kiểm `HasFCS=false`, đếm RLE output độc lập và đối chiếu bounded library decode; chưa chứng minh production output cap.
  - Corrupt frame: header/FCS hợp lệ, block type=3 (reserved), CRC mới khớp stored bytes. Kiểm reserved bits độc lập và xác nhận bounded library decode báo lỗi; production rejection vẫn pending.
- Premise checks dùng literal byte layout/RLE arithmetic cho đúng các probes cố định, đối chiếu `zstd.Header.Decode`; không phải standalone strict decoder. Mutation tests reserved header bit chứng minh malformed header không bị nhầm thành dictionary/window/content premise hợp lệ.

Các categories vẫn là đề xuất test adapter, chưa là error API được freeze. Resource checks có thể diễn ra trước decode; resource/content/window/output được phân biệt bằng từng vector premise, không tự thêm production error codes. `TestProductionVerificationPending` tiếp tục SKIP. I/O wrapper không thay đổi trong lượt fix này.
