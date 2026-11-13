# Cache scope API validation

Baseline `dde10b3`. Hard dependencies (cache node W04, compose runtime Cloud/Edge
và Jobs/Nodes W05) được xác nhận Done trên backlog ngày 2026-11-03.

## Checklist mapping

1. GET `/admin/v1/cache/stats` đọc shared Nodes samples; POST `/admin/v1/cache/purge`
   tạo existing `cache_purge` parent/child jobs. Shared Cloud/Edge wiring cung cấp
   cache samples ở heartbeat và cùng Operations service cho mọi gateway. Stats
   và purge đều system admin; purge giữ existing transactional actor recheck.
2. Per-node denominator/hit ratio và aggregate weighted ratio; sample age, boot,
   service incarnation, zero denominator và unavailable node được hiển thị riêng.
   Incomplete aggregates chỉ bao gồm valid reporting nodes. Samples không atomic
   và không đưa bearer sang peer. Limits/freshness/error contract trong API doc.
3. Native Cloud/Edge integration exercise public routes trên ba independent
   runtime instances, job status từ gateway khác, stale/offline partial failure,
   node-scope unavailable purge và cancellation. Existing cache/read-ahead/auth
   tests tiếp tục áp dụng. Thêm cache-loss restart/refill và generation sau
   canonical location CAS giống publication boundary của compaction.

## Tests

- `TestCacheHTTPSharedSamplesWeightedRatioAndPurge`: actual Badger records + HTTP,
  9/10 và 1/90 tạo aggregate10/100 (không phải mean); empty, stale, missing sample,
  foreign service, historical boot; strict scope/query/body validation, owner và
  unauthenticated rejection, cross-server job status.
- `TestOperationalMutationsRecheckActorAfterAdmission`: cache purge cùng các
  operational mutations recheck revoked token/disabled user ở metadata mutation.
- `TestNodeCacheNativeRoundTripAndOldRecord`: native typed counters roundtrip;
  Nodes record cũ không có cache sample vẫn đọc được, báo unavailable.
- `TestNativeSharedCloudEdgeRuntime`: real shared wiring, DynamoDB Local + Moto,
  independent Cloud/Edge instances, dedicated stats/purge routes, per-node
  partial results, shared job visibility. Purge không thay logical quota;
  Range read sau purge lấy đúng immutable durable bytes và refill được cache.
- `TestCacheNodeLossRefillsFromDurableOrigin`: close/reopen disposable disk cache
  dưới node mới, refetch verified origin, request sau hit; durable metadata giữ nguyên.
- `TestCacheGenerationAfterCompactionPublication`: RegisterUploadingPack,
  MarkPackWarm và real CompareAndSwapChunkLocation tới generation mới; old cached
  bytes không satisfy canonical location mới, verified refill rồi hit generation mới.
  Physical SDK seam phục vụ immutable entry bytes; không chạy full compactor.
- Existing `TestNodePromotionStatsGenerationAndPurge`, concurrent purge/fill,
  read-ahead scheduling/cancellation, `TestTwoTierCacheDiskHitAndRevokedAuthorization`
  và WarmReader boundary tests chứng minh auth trước cache hit và pin/generation.

## Environment and limits

Linux Go1.27.2, pinned DDB Local3.3.0 và Moto5.1.15 từ repository compose.
Fixture project riêng `miles-cache-contract`; Moto chia network namespace của
DDB fixture để test nhận explicit loopback endpoints8000/5000. Không AWS
credentials thật, không deployed resources hoặc chi phí AWS mới. Fixtures được
dọn riêng sau kiểm thử; không chạm container/workload khác.

Native invocation: `MILES_DDB_LOCAL_REQUIRED=1`,
`MILES_DDB_LOCAL_ENDPOINT=http://127.0.0.1:8000`,
`MILES_RECOVERY_S3_ENDPOINT=http://127.0.0.1:5000`, rồi
`go test -race -count=1 ./internal/app -run '^TestNativeSharedCloudEdgeRuntime$'`.

Acceptance ở đây là native development integration. Không thay live Cloud/Edge
deployment evidence của dependency bằng emulator và không claim đã đo stats/purge
trên các node AWS thật. Rollout binary mới cần heartbeat đầu tiên để có sample;
old nodes thiếu field vẫn hiển thị unavailable. Purge không là refill barrier.

## Recorded result (2026-11-03)

Code/test revision `1633cdf`: `make check` PASS trên clean Linux clone (full Go
suite, vet/build, 32 Python tests, doc-name và gofmt checks). Native three-runtime
Cloud/Edge integration với dedicated cache routes PASS cùng race detector.
Full affected `go test -race -count=1 -timeout=10m` PASS cho cache, jobs, readpath,
engine/write, object, app, api/admin và metadata/dynamodb; native endpoints nêu
trên bắt buộc trong lượt chạy. Whitespace check PASS. Live deployed AWS cache
acceptance chưa chạy trong task này.
