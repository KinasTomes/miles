# Cache node, policy và scope validation

Task: W04-PHONG-01 — Cache hoàn chỉnh, policies và scope contract.
Baseline: `origin/main` tại `6bdef45`.

## Checklist và boundary

| Checklist | Implementation và evidence |
|---|---|
| Bounded LRU/read-ahead/admit-on-write, generation invalidation, node stats | `cache.LRU`, `cache.Disk`, `cache.Node`; existing admitted read-ahead scheduler; object policy → write session → StorePublisher; `Node.Stats` |
| Cluster purge fan-out hooks, metric methodology | `PurgeClient`, `FanoutPurge` với immutable target list do caller cung cấp và kết quả từng node; không tự discover node hoặc trả cluster aggregate |
| Authorization/revoke, memory/disk eviction, Range amplification | Shared Cache contract cho hai tầng; disk LRU/corruption/ownership/concurrency; real Badger/S3-adapter read tests; object PUT→GET policy tests; publisher fault/dedup loser tests |

Hard prerequisites W03-PHONG-01 và W04-HAI-01 đã Done khi đọc board;
W04-HUNG-02 còn Todo là soft dependency. W04 chuẩn bị typed hooks; Jobs/Nodes,
Admin routes `scope=node|cluster` và durable cluster purge job thuộc W05.
Không tự thay status/checklist/sign-off trên board.

## Node cache và giới hạn

WarmReader nhận shared `storage.Cache` thay concrete LRU. Existing
Authorize→ReadView→Resolve→Pin→lifecycle preflight→cache→verify→emit không đổi.
Disk hit vẫn decode/CRC/SHA qua reader; cached bytes không cấp quyền đọc, không
thay origin durability hoặc recovery root. Admission budget/read-ahead window
hiện tại tiếp tục giới hạn owned copies/decoder/transport/workspace riêng.

Node tier1 là strict memory LRU, tier2 là strict disk LRU. Read miss ở memory
có thể hit disk và promote best-effort vào memory; hai tầng lưu physical stored
bytes theo exact PackRef/generation/offset/length, không relabel generation.
Node serialize tier operations/purge, không giữ cache lock qua auth/network.
Disk I/O là synchronous và bounded theo entry size; không coi context cancel là
bằng chứng một filesystem syscall đã bị ngắt. Shared methods kiểm cancellation
ở lock boundary, trước publication và sau read trước trả bytes.

CapacityBytes là payload bytes, MaxEntries giới hạn index metadata, MaxCopyBytes
là per-entry read/copy/admission cap. Oversized entry bypass không đuổi hết useful
data. Không claim bound process RSS hoặc total allocator/filesystem overhead.
Disk removes victims trước file create để giới hạn cả payload đang ghi. Deletion
lỗi giữ accounting/index, không giả vờ đã giải phóng capacity. Disk checksum sai
hoặc immutable-key conflict reject/evict exact generation, giữ generation khác.
Node conflict evict cả hai tầng. `Evict` và purge chỉ resident-only; later Put
có thể refill. Không unpin, revoke quyền, xóa pack gốc hoặc hủy copies caller giữ.

Local bootstrap dùng memory64MiB/1024entries, disk1GiB/16384entries,
MaxCopyBytes4MiB mỗi tầng, tại `<local_data_path>/cache`, node ID `local`.
Đây là explicit dev profile, không production sizing/benchmark hoặc globally
unique cluster node identity. Cloud/Edge phải supply capacity/node identity phù
hợp trước Jobs/Nodes integration. Object service assembler truyền cùng Node vào
WarmReader.Cache và object.Options.Cache để có read/write admission chung.

Disk directory là dedicated disposable directory, không dùng chung user data.
Exclusive `owner.lock` ngăn hai process cùng sở hữu. Entry filename chỉ là hash
của key, không path từ object/user. Startup validate directory inventory trước
khi bỏ old hash-named entries; unknown files/symlinks reject. Close xóa entries
và owner marker. Crash để lại marker thì startup fail closed: operator chỉ được
bỏ marker sau khi xác nhận process cũ đã dừng. Không tự suy PID/timeout là proof.
Cache không fsync hoặc claim persisted recovery; crash không ảnh hưởng durable
object. Cache directory cần ở private data path và không được process khác sửa.

## Write policy

Object PUT pin một validated bucket policy snapshot (bucket ID/revision) trước
prepare; forward AdmitOnWrite và shared cache qua WriteSession vào Publisher.
`false` không chủ động write-cache; read-through vẫn bật. `true` best-effort cache
entry sau finalized pack+sidecar/Warm và canonical location resolution.
Cache bytes là exact stored slice từ complete pack, không raw logical body.
Dedup loser không cache dưới winner location khi writer không có winner bytes.
Cache failure/oversize/bypass không đảo kết quả durable publication; cancellation
và publication vẫn được existing session/metadata protocol kiểm độc lập.
Aborted/orphan bytes có thể resident nhưng không tạo logical reachability.

## Stats methodology và purge hook contract

`Node.Stats` luôn `Scope=node`, có NodeID; counters cumulative từ instance start.
Purge không reset counters. Một logical Get tính một lookup dù memory miss→disk
hit→memory promotion. Hit/Miss chỉ tính successful lookup; Errors tính cache
Get/Put errors; Put attempts độc lập lookups. HitRatio=Hits/(Hits+Misses), zero
samples trả0 cùng denominator0 rõ ràng. Memory/Disk báo occupancy/capacity,
entries/max entries và capacity evictions từng tầng. Một physical range có thể
resident ở cả hai tầng: không cộng thành unique cached bytes.

`miles_warm_read_backend_bytes_total` đếm complete successful RangeGet payload
bytes trước decode; không bao HEAD/sidecar metadata, failed/truncated transport,
request headers hoặc billed AWS bytes. `emitted_bytes_total` là logical bytes đã
emit thành công. Range amplification test dùng **delta backend bytes / delta
emitted bytes cho cùng request interval**, với output bytes và fetch ranges được
SDK seam đo độc lập. Không trình bày lifetime ratio như latency/S3 cost hoặc
cluster measurement. Read-ahead chỉ chạy intersecting plan; Range1byte có thể
cần nguyên compressed chunk nhưng không tự fetch ngoài requested plan.

PurgeNode yêu cầu exact local ID. Caller phải authorize system admin trước hook;
cache package không là HTTP auth boundary. FanoutPurge nhận1..256unique targets
do Jobs/Nodes freeze, gọi bounded transport sequentially và giữ per-node error,
kể cả failure/cancellation. Không chọn lại target membership giữa retry. Caller
cần capture job/node incarnation, deadlines và durable retry/progress ở W05.
Một node lỗi không biến thành all-cluster success; không có aggregate metric từ
partial/stale samples. Strong refill barrier không được hứa bởi hook này.

## Verification

- `TestNodeSharedCacheContract`, `TestDiskBoundedLRUAndCorruption`,
  `TestNodePromotionStatsGenerationAndPurge`, `TestNodeConflictEvictsBothTiers`,
  `TestDiskOwnershipAndClose`, `TestNodeConcurrentPurgeAndFillBudgets`.
- `TestFanoutPurgePartialFailureAndCancellation`: exact per-node results,
  duplicate target rejection trước dispatch và canceled node reporting.
- `TestPublisherWriteAdmissionExactBytesAndFailureIsolation` và dedup race:
  true/false, exact stored bytes, incomplete pack, cache failure, losing location.
- `TestObjectWriteCachePolicyAndReadThrough`: real object/session/publisher,
  Badger, SDK seam, shared Node PUT→GET; false vẫn read-fill, repeat GET hits.
- `TestTwoTierCacheDiskHitAndRevokedAuthorization`: memory purge→disk hit
  không fetch origin; revoke authoritative credential→new read bị deny trước
  cache lookup. Existing test giữ admitted stream semantics sau revoke.
- `TestTwoTierRangeAmplificationBoundedByIntersectingChunks`: w0/w4,
  Range1byte last chunk, exact SDK bytes/counters, disk hit0origin, purge refetch.

Windows Go1.27.2: cache/readpath/object/write tests chạy được. Local bootstrap
baseline yêu cầu Unix0700 và directory Sync; không giảm yêu cầu bảo mật để làm
test Windows pass. Không claim live AWS, distributed purge hoặc cluster stats.

Kết quả thực chạy 28/10/2026 trên official `golang:1.27.2-bookworm`, workspace
mount read-only: `go test ./...` PASS (gồm local bootstrap/reopen),
`go test -race ./internal/cache ./internal/readpath ./internal/engine/write
./internal/object ./internal/app` PASS, `go vet ./...` PASS và
`go build ./cmd/...` PASS. Windows vet/build và6 Python boundary tests PASS;
gofmt/doc-name/whitespace kiểm trước commit. SDK seam thay S3 service; không
chạy AWS hoặc DynamoDB Local integration profile trong lượt này.
