# W02-HUNG-03 — Local runtime và fixture runner

## Chạy process

Yêu cầu Go1.27.2, GNU Make và Python3. Chạy từ repository root:

```sh
make run-dev
# terminal khác
curl -i http://127.0.0.1:9100/healthz
curl -i http://127.0.0.1:9100/readyz
curl http://127.0.0.1:9100/metrics
```

`run-dev` chọn `configs/dev.json` một cách explicit. Shared MetadataStore/PackStore/Cache ports được nối với in-memory fixtures hiện có. Fixture probes kiểm metadata bucket, pack HEAD và cache sentinel; clock readiness là giả lập của dev, không phải UTC uncertainty proof. Metrics có label `mode="dev"`, startup log cũng ghi rõ volatile fixtures. Process này chỉ mở health port; S3/Admin listeners, durable storage, R1/R2 và chính sách Clock/Policy W02-PHONG-01 chưa được implement ở task này.

Dev provisioning (W03-CHIEN-01) mint đúng một cặp SigV4 test credential mỗi lần khởi động process: access key 20 ký tự [A-Z0-9] và secret base64 của 30 byte CSPRNG (format API v2 3.5), gắn user `dev` — chủ của bucket probe — không có cờ system admin. Cặp này nằm trong `Dependencies.DevCredentials` để task nối S3 listener (W03-HUNG-03) đưa ra phía client tương ứng; nó không nằm trong repo, không được ghi log và runtime mode không mint. Credential backend AWS vẫn tách khỏi bảng này như mục 2.2 đã ghi.

`./bin/miles -version` và `-help` vẫn có. Không truyền config thì dùng runtime mode: process sống, `/healthz=200`, `/readyz=503` vì real storage/validated clock adapters chưa được wiring. `configs/cloud.example.json` minh họa cùng binary trong Cloud profile, không phải deployment đã hoàn tất. Không fallback sang fake ở runtime. Real adapter factory phải cung cấp shared ports và bounded dependency/clock probe từ owners tương ứng trước khi runtime có thể ready.

## Startup config và network boundary

File JSON tối đa64KiB, chỉ nhận các fields dưới; unknown/duplicate/null/wrong-type/trailing JSON reject trước listen:

| Field | Default | Điều kiện |
|---|---|---|
| `profile` | `edge` | `cloud` hoặc `edge`, cùng core |
| `mode` | `runtime` | `dev` phải chọn explicit; dev chỉ bind loopback |
| `health_address` | `127.0.0.1:9100` | Literal loopback/private IPv4/IPv6; reject wildcard/public/DNS names. Port0 dùng cho disposable tests, địa chỉ thật được log |
| `shutdown_seconds` |1800|1..1800; hết hạn trả lỗi incomplete, không claim flush xong |

Config này chỉ thuộc process startup, không mở thêm hot-policy endpoint, thay revisioned policy hoặc nhân đôi Clock/evaluator của Phong. Rút ngắn shutdown budget không kéo dài deadline/lease hoặc chứng minh old work đã xong. Runtime Cloud có thể bind địa chỉ VPC cụ thể; SG/reverse proxy phải giữ port9100 trong mạng quản trị, không forward nó qua public application endpoint. Chưa cấu hình TLS/ALB/ASG trong task local này.

## Health/readiness và metrics

- `GET /healthz`: process HTTP đang sống; không chạy backend query.
- `GET /readyz`:200 chỉ khi observation usable còn mới và node không draining. Initial/missing/failed/timeout/canceled/stale observation trả503. Probe chạy ngoài handlers mỗi2s, timeout1s, observation không được dùng từ tuổi5s trở đi. Callback phải honor context và kiểm dependency/Clock proof tương ứng; không infer clock synchronized từ `time.Now()`.
- `GET /metrics`: Prometheus text0.0.4, fixed process gauges/probe-failure counter, cached readiness/age/drain state. Không AWS request trên scrape; không raw error/secret/object-key/user-ID labels. Counter/gauge phản ánh những gì process thực sự quan sát, chưa phải toàn bộ storage/performance metrics của gateway.
- Các responses dùng no-store; invalid methods trả405, unknown paths404.

Readiness là routing signal, không là shared R1 authority. `app.Admission.Acquire`/release theo dõi local work và reject sau `Drain`; S3/Admin/job owners phải integrate guard này khi thêm handlers, đồng thời vẫn kiểm authorization, live dependency state, deadline và shared R1 conditions. Không dùng readyz hoặc ALB để thay request admission.

SIGINT/SIGTERM đóng local admission trước, readyz503 ngay khi process xử lý drain. Process chờ admitted work, shutdown HTTP server và probe worker trong cùng bounded budget. Timeout force-close HTTP và exit nonzero với `incomplete shutdown`; không báo durable pack/outbox flush nếu chưa có implementation/proof. Không có Cloud deregistration/lifecycle hook ở local bootstrap này.

## Test không cần container

```sh
make check
go test -race ./internal/config ./internal/app ./internal/observability ./cmd/miles
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
4. `finally` teardown đúng Compose project, dừng process và xóa temporary files; không dùng persistent volumes hoặc mount host credentials.

Runner loại AWS environment/profile/session/role configuration được kế thừa, chỉ dùng dummy credentials và explicitly validated `http://127.0.0.1:<port>` endpoints. Credential/config files trỏ temp/devnull, metadata credential lookup tắt. Không cho endpoint AWS/public/private-remote, không dùng shared developer table/bucket. DynamoDB telemetry tắt. Không chạy fixture runner lên production hoặc thay endpoint để trỏ AWS thật.

Để giữ stack cho consumer dev tự thử:

```sh
docker compose -f deploy/local/compose.yaml -p miles-local up -d
docker compose -f deploy/local/compose.yaml -p miles-local port dynamodb 8000
docker compose -f deploy/local/compose.yaml -p miles-local port s3 5000
# khi xong: chỉ dọn project này
docker compose -f deploy/local/compose.yaml -p miles-local down --volumes
```

Published ports chỉ bind127.0.0.1 và được cấp ngẫu nhiên. Stack fixtures không tự trở thành runtime storage adapter: wiring Badger/DynamoDB/S3 do storage owners thực hiện sau. Moto/DynamoDB Local không chứng minh IAM, KMS, PITR, S3 durability/archive, multi-node R1/R2 hoặc Cloud/Edge production behavior. Full storage acceptance suite hiện hữu vẫn chạy riêng qua `make check`.

Images được pin bằng tag và manifest digest trong `deploy/local/compose.yaml`; update cần rerun fixture tests. Nguồn: [AWS DynamoDB Local](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.DownloadingAndRunning.html), [Moto server mode](https://docs.getmoto.org/en/latest/docs/server_mode.html), [Go HTTP Shutdown](https://pkg.go.dev/net/http#Server.Shutdown).
