# Local runtime, CLI và fixture runner

Local durable mode có cache node hai tầng: memory64MiB và disk1GiB tại
`<local_data_path>/cache`, node ID `local`. Cache là disposable, không là nguồn
recovery. Nếu process bị kill để lại `cache/owner.lock`, chỉ bỏ marker sau khi
xác nhận process cũ đã dừng; không xóa metadata/credentials. Stats/purge hook
có scope node; Admin cluster job vẫn thuộc W05. Xem
[cache node validation](../testing/cache-node-validation.md) cho limits,
admit-on-write, methodology và evidence.

## Chạy process

Yêu cầu Go1.27.2, GNU Make và Python3. Chạy từ repository root:

```sh
make run-dev
# terminal khác
curl -i http://127.0.0.1:9100/healthz
curl -i http://127.0.0.1:9100/readyz
curl http://127.0.0.1:9100/metrics
```

`run-dev` chọn `configs/dev.json` một cách explicit. Shared MetadataStore/PackStore/Cache ports được nối với in-memory fixtures hiện có. Fixture probes kiểm metadata bucket, pack HEAD và cache sentinel; clock readiness là giả lập của dev, không phải UTC uncertainty proof. Metrics có label `mode="dev"`, startup log cũng ghi rõ volatile fixtures. Không cấu hình `s3_address` thì process chỉ mở health listener; `mode=dev` vẫn không có durable storage hoặc distributed R1/R2.

Dev provisioning (W03-CHIEN-01) mint đúng một cặp SigV4 test credential mỗi lần khởi động process: access key 20 ký tự [A-Z0-9] và secret base64 của 30 byte CSPRNG (format API v2 3.5), gắn user `dev` — chủ của bucket probe — không có cờ system admin. Cặp này nằm trong `Dependencies.DevCredentials` để S3 listener dùng cho client tương ứng; nó không nằm trong repo, không được ghi log và runtime mode không mint. Credential backend AWS vẫn tách khỏi bảng này như mục 2.2 đã ghi.

`./bin/miles -version` và `-help` vẫn có. Không truyền config thì dùng runtime mode: process sống, `/healthz=200`, `/readyz=503` vì real storage/validated clock adapters chưa được wiring. `configs/cloud.example.json` minh họa cùng binary trong Cloud profile, không phải deployment đã hoàn tất. Không fallback sang fake ở runtime. Chế độ local dưới đây nối Badger/S3 thực tế ở một node, nhưng không cấp UTC uncertainty proof hoặc distributed authority cho runtime Cloud.

## Startup config và network boundary

File JSON tối đa64KiB, chỉ nhận các fields dưới; unknown/duplicate/null/wrong-type/trailing JSON reject trước listen:

| Field | Default | Điều kiện |
|---|---|---|
| `profile` | `edge` | `cloud` hoặc `edge`, cùng core |
| `mode` | `runtime` | `dev` volatile, `local` durable dev/Moto, `benchmark` Badger + S3 HTTPS thật, `runtime` chưa nối Cloud; development chỉ bind loopback |
| `s3_address` | không mở | Literal loopback IP:port; dev/local/benchmark, runtime bị từ chối khi validate. Local/benchmark gắn bucket và object P0; dev fixture chỉ gắn bucket |
| `admin_address` | không mở | Literal loopback IP:port; chỉ local/benchmark (cần IdentityStore Badger). Admin API bearer; Sealer AES-GCM với key 32 byte `<local_data_path>/credential-key` (tạo 0600 lần đầu); cursor key random mỗi process nên cursor mất khi restart. Token đầu tiên: `dsgctl admin-token bootstrap --local-data-path` khi gateway đã dừng |
| `region` | `ap-southeast-1` | Region SigV4 của gateway và local pack client |
| `local_data_path` | không có | Bắt buộc ở local/benchmark; directory không symlink, permission 0700 |
| `pack_endpoint` | không có | Local: HTTP literal loopback với port explicit; benchmark: HTTPS origin. Không userinfo/path/query |
| `pack_bucket` | không có | Bucket vật lý đã tạo sẵn; khác các bucket logical của gateway |
| `pack_credentials_file` | không có | Chỉ benchmark: file 0600, regular/non-symlink, credential tạm có session_token; không default AWS provider |
| `pack_kms_key_id` | không có | Chỉ benchmark: explicit SSE-KMS key, cùng giả định encryption với direct baseline |
| `health_address` | `127.0.0.1:9100` | Literal loopback/private IPv4/IPv6; reject wildcard/public/DNS names. Port0 dùng cho disposable tests, địa chỉ thật được log |
| `shutdown_seconds` |1800|1..1800; hết hạn trả lỗi incomplete, không claim flush xong |

Config này chỉ thuộc process startup, không mở thêm hot-policy endpoint, thay revisioned policy hoặc nhân đôi Clock/evaluator của Phong. Rút ngắn shutdown budget không kéo dài deadline/lease hoặc chứng minh old work đã xong. Runtime Cloud có thể bind địa chỉ VPC cụ thể; SG/reverse proxy phải giữ port9100 trong mạng quản trị, không forward nó qua public application endpoint. Chưa cấu hình TLS/ALB/ASG trong task local này.

## Health/readiness và metrics

- `GET /healthz`: process HTTP đang sống; không chạy backend query.
- `GET /readyz`:200 chỉ khi observation usable còn mới và node không draining. Initial/missing/failed/timeout/canceled/stale observation trả503. Probe chạy ngoài handlers mỗi2s, timeout1s, observation không được dùng từ tuổi5s trở đi. Callback phải honor context và kiểm dependency/Clock proof tương ứng; không infer clock synchronized từ `time.Now()`.
- `GET /metrics`: Prometheus text0.0.4, fixed process gauges/probe-failure counter, cached readiness/age/drain state. Không AWS request trên scrape; không raw error/secret/object-key/user-ID labels. Counter/gauge phản ánh những gì process thực sự quan sát, chưa phải toàn bộ storage/performance metrics của gateway.
- Các responses dùng no-store; invalid methods trả405, unknown paths404.

Readiness là routing signal, không là shared R1 authority. `app.Admission.Acquire`/release theo dõi local work và reject sau `Drain`; S3/Admin/job owners phải integrate guard này khi thêm handlers, đồng thời vẫn kiểm authorization, live dependency state, deadline và shared R1 conditions. Không dùng readyz hoặc ALB để thay request admission.

SIGINT/SIGTERM đóng local admission trước, readyz503 ngay khi process xử lý drain. Process chờ admitted work, shutdown mọi HTTP listener và probe worker trong cùng bounded budget. Chỉ đóng Badger khi work/probe đã quiescent; timeout không đóng backend bên dưới work còn chạy. Timeout force-close HTTP và exit nonzero với `incomplete shutdown`; không báo durable pack/outbox flush nếu chưa có implementation/proof. Không có Cloud deregistration/lifecycle hook ở local bootstrap này.

## Một node local với Badger và S3

`configs/local.example.json` là cấu hình khởi đầu. Sửa `pack_endpoint` theo port Moto do Docker Compose xuất ra; tạo bucket vật lý `pack_bucket` trên endpoint đó trước khi chạy. Backend client dùng dummy credentials cố định, bỏ qua AWS profile/environment/IMDS, proxy và redirect; không chạy được với endpoint AWS thật.

```sh
make build
./bin/miles -config configs/local.example.json
```

Lần đầu local mode mở Badger (giữ process lock), tạo `credentials.json` permission 0600 trong `local_data_path`, rồi gọi one-time Badger Bootstrap để provision đúng một user dev và credential không có admin scope. Restart giữ nguyên credential và bucket identity; Bootstrap conflict không được dùng để ghi đè user/credential hay quyền đã đổi. Badger cung cấp trạng thái marker bootstrap để runtime chỉ tạo credential khi installation chưa được provision. File credential thiếu/hỏng/không khớp store làm startup thất bại; installation đã bootstrap không được tạo file thay thế hoặc reset metadata để tự chữa. Khôi phục file gốc sẽ dùng lại metadata cũ. Secret vẫn được sync trước transaction; restart ở giữa hai bước dùng lại file đã chuẩn bị. Giữ cả metadata directory và credential file khi sao lưu dev installation. Không copy installation này sang runtime production hoặc thêm endpoint online bypass bearer.

S3 client dùng `access_key_id`/`secret_access_key` trong file riêng này, tách khỏi dummy credential của backend. Secret không được in vào startup log. Với mode dev volatile, credential chỉ nằm trong `Dependencies.DevCredentials`; dùng local mode khi cần client truy cập và restart bền vững.

Readiness local probe đọc credential record từ Badger, HEAD bucket vật lý qua đúng SDK client, mỗi lượt có context timeout riêng tối đa 1 giây, cache observation dưới 5 giây. Client truyền pack không có timeout 1 giây toàn cục; upload/download và đọc body giữ deadline của caller. Không PUT sentinel hoặc tự tạo lại backend bị mất. LRU trong RAM được validate khi khởi tạo với cap 64 MiB/1024 entry, mỗi copy tối đa 4 MiB; không có backend riêng cần probe và miss không làm node unready. Metrics local vẫn mang `mode="dev"`, log ghi `mode="local"` và simulated clock; đây không phải Clock uncertainty, S3 object integrity hoặc R1/R2 production proof. Runtime thiếu proof vẫn ready503.

Mỗi S3 request nhận hard context deadline 30 phút ngay tại admission; read/write socket deadline dùng cùng mốc, không refresh theo traffic/retry. Không dùng `readyz` để thay request gate. Drain chặn request mới bằng S3 XML ServiceUnavailable/503; request đã nhận giữ admission đến khi handler trả về, kể cả context đã hủy. `Dependencies.Packs` chỉ expose `coordination.ProtectPackStore`: DeletePack/ChangeStorageClass vẫn bị chặn như PR #37; raw SDK adapter không được trả ra consumer. S3 wiring nằm tại composition root `cmd/miles/s3.go`; API owner bổ sung handler tại đây, không để package app phụ thuộc ngược vào API.

Local/benchmark hiện nối PUT/GET/HEAD/Range/LIST/DELETE qua object service thật, managed writer, SDK packs và WarmReader. Badger mở RequireProducers=true; producer ghi immutable payload/outbox trong cùng transaction. Mỗi mutation có fence riêng, history được lưu bền vững, incarnation mới chặn writer cũ. Quota dev khởi tạo 1 GiB một lần, không reset usage khi restart. Upgrade installation cũ chỉ tự thêm accounting/namespace khi không có record object/upload; store đã có dữ liệu mà thiếu baseline phải dừng, không coi là rỗng. Read-ahead/budgets và validated policy cố định nằm trong `internal/app/local_objects.go`; local mode dùng memory/disk cache node và default admit-on-write từ #43. Benchmark mode giữ memory LRU 64 MiB, không disk và tắt admit-on-write để giữ profile đã đo. Chưa có Admin policy updates hoặc exporter/epoch sealing/physical GC.

`configs/benchmark.example.json` là cấu hình chạy S3 HTTPS có chủ đích; local mode vẫn từ chối AWS endpoint và credential thật. Benchmark mode là single-node development, không mở production runtime hoặc cấp distributed UTC/R1/R2 proof. Provisioning, phí và cleanup dữ liệu test cần duyệt riêng. Xem [benchmark runbook](../../benchmarks/warm-harness.md).

## Nền CLI Admin

```sh
./bin/dsgctl --endpoint http://127.0.0.1:19000 --token-file /private/admin-token --timeout 30s node ls
```

Token file là raw bearer format API §6.4, permission đúng 0600, regular file không symlink; có thể có một newline cuối. CLI không nhận raw token argument, không dùng SigV4 và chỉ gửi bearer đến `/admin/v1/`. Plain HTTP chỉ cho literal loopback/tunnel; endpoint remote bắt buộc HTTPS với certificate verification. Không follow redirect hoặc environment proxy; response tối đa 2 MiB, timeout bao phủ cả đọc response. Error không in response body hay transport URL/token; reflected bearer trong JSON success được redact.

Transport hỗ trợ `stats`, `pack ls`, lệnh danh tính/quyền/policy (`user`, `usage`,
`bucket show`, `grant`, `policy`, `bucket-policy`, `credentials`, `admin-token`),
`job ls/show/run/cancel` và `node ls/drain`. Đặt global flags trước command; list nhận
`--limit` và `--cursor`, job run nhận `--dry-run`, `--params-file`, `--policy-revision`.
Secret/token do `credentials create`/`admin-token create` trả chỉ ghi vào file `--out`
mới (O_EXCL, 0600), stdout bị che. Xem [Jobs/Nodes contract](../contracts/jobs-and-nodes.md),
[validation](../testing/jobs-nodes-metrics-validation.md) và
[Admin identity validation](../testing/admin-identity-validation.md).

Local/benchmark mở Admin API khi có `admin_address`. Token đầu tiên cấp offline khi
gateway dừng: `dsgctl admin-token bootstrap --user <id> --token-file <path>
--local-data-path <local_data_path>` (lần hai bị từ chối, không ghi gì); sau đó system
admin cấp thêm token/credential qua API. Route stats/packs/jobs/nodes chưa phục vụ:
router404 không có Admin error envelope báo unsupported; JSON404 có `error.code` trả
`adminclient.ErrNotFound`. Error vẫn bounded/redacted, exit nonzero, không giả success.
W05-HUNG-01/02 nối runtime Cloud/Edge, shared jobs/nodes và executors thật.

## Test không cần container

```sh
make check
go test -race ./internal/config ./internal/app ./internal/observability ./internal/adminclient ./internal/secretfile ./cmd/miles ./cmd/dsgctl
```

Unit/HTTP tests kiểm config/privacy boundary, initial/unusable/stale/canceled readiness, no request/scrape-triggered probe, concurrent admission/drain, completion vs timeout và shared dev adapter failure. Python unit tests kiểm local-only endpoint và tách real AWS credentials/profile khỏi fixture subprocess.

## DynamoDB Local + test S3 + process smoke

Cần Docker Engine/Compose với local Unix context và AWS CLI v2 có S3 conditional-write options. Runner dùng CLI, không thêm AWS SDK hoặc boto3 vào ứng dụng. Chạy:

```sh
make test-local
```

Một lượt tạo Compose project UUID riêng, pinned multi-architecture images, ephemeral loopback ports và in-memory data, rồi:

1. DynamoDB Local: tạo fixture table riêng; transaction head/event all-or-none, conditional conflict không đổi head/không leak event; consistent lookup và cleanup.
2. Moto test S3: bucket riêng, conditional-create không overwrite, HEAD length, Range/If-Match, precondition failure và cleanup.
3. Build/run binary thật: Edge/dev và Cloud/dev ready200; Cloud/runtime thiếu adapters ready503 nhưng health200; metrics đúng mode; SIGTERM exit0 và log shutdown complete.
4. Edge/local: Badger + SDK S3 local + LRU + S3 listener thật. AWS CLI ký CreateBucket, PUT/GET/HEAD/Range/LIST/DELETE; GET đối chiếu hash và đọc lại sau restart. Restart giữ credential/bucket/object, backend mất → ready503/health200, metadata request vẫn dùng gate riêng. Missing object trả NoSuchKey, credential backend bị từ chối. CLI command chưa hỗ trợ exit nonzero.
5. `finally` teardown đúng Compose project, dừng process và xóa temporary files; không dùng persistent volumes hoặc mount host credentials.

Runner loại AWS environment/profile/session/role configuration được kế thừa, chỉ dùng dummy credentials và explicitly validated `http://127.0.0.1:<port>` endpoints. Credential/config files trỏ temp/devnull, metadata credential lookup tắt. Không cho endpoint AWS/public/private-remote, không dùng shared developer table/bucket. DynamoDB telemetry tắt. Không chạy fixture runner lên production hoặc thay endpoint để trỏ AWS thật.

Để giữ stack cho consumer dev tự thử:

```sh
docker compose -f deploy/local/compose.yaml -p miles-local up -d
docker compose -f deploy/local/compose.yaml -p miles-local port dynamodb 8000
docker compose -f deploy/local/compose.yaml -p miles-local port s3 5000
# khi xong: chỉ dọn project này
docker compose -f deploy/local/compose.yaml -p miles-local down --volumes
```

Published ports chỉ bind127.0.0.1 và được cấp ngẫu nhiên. Local mode đã wiring Badger/S3 một node; DynamoDB Local vẫn chỉ kiểm transaction fixtures, chưa phải networked MetadataStore adapter. Moto/DynamoDB Local không chứng minh IAM, KMS, PITR, S3 durability/archive, multi-node R1/R2 hoặc Cloud/Edge production behavior. Full storage acceptance suite hiện hữu vẫn chạy riêng qua `make check`.

Images được pin bằng tag và manifest digest trong `deploy/local/compose.yaml`; update cần rerun fixture tests. Nguồn: [AWS DynamoDB Local](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.DownloadingAndRunning.html), [Moto server mode](https://docs.getmoto.org/en/latest/docs/server_mode.html), [Go HTTP Shutdown](https://pkg.go.dev/net/http#Server.Shutdown).
