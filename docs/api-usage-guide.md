# Hướng dẫn sử dụng đầy đủ và đối chiếu API Miles

> Tài liệu hướng dẫn vận hành, cấu hình client, phân quyền, debug opt-in, error schemas thực tế và bảng đối chiếu API coverage cho **Dedup Storage Gateway (Miles)** (`cmd/miles`, `cmd/dsgctl`, `internal/api/s3`, `internal/api/admin`, `internal/auth`, `internal/multipart`).
> Tham chiếu thiết kế: [API v2](dedup-storage-gateway-api.md), [Kiến trúc v4](dedup-storage-gateway-architecture.md), [Local runtime](development/local-runtime.md), [Shared runtime](development/shared-runtime.md), [Cluster operations](development/cluster-operations.md), [Runbook lifecycle & recovery](runbooks/lifecycle-recovery-operations.md).

---

## 1. Kiến trúc bề mặt và tách biệt danh tính / credential

Miles dùng **một binary `miles` và một core contract** cho cả hai profile triển khai:

| Thành phần | Profile Cloud | Profile Edge | Mode Local / Benchmark (single-node dev) |
|---|---|---|---|
| **S3 API** | `0.0.0.0:8080` / private VPC IP sau ALB TLS `:443` (`routing.http.preserve_host_header.enabled=true`) | `127.0.0.1:8080` sau TLS proxy/local (`deploy/edge`) hoặc private VPC/LAN | `s3_address` loopback (VD: `127.0.0.1:8080`) |
| **Admin API** | `127.0.0.1:9000` qua bastion + SSH local forwarding (`sg-admin`) | `127.0.0.1:9000` (loopback hoặc SSH tunnel) | `admin_address` loopback (VD: `127.0.0.1:9000`) |
| **Health / Metrics** | Private IP `:9100` (ALB target check `/readyz`, monitoring VPC) | `127.0.0.1:9100` | `health_address` loopback/private (mặc định `127.0.0.1:9100`) |
| **Metadata & R1/R2** | DynamoDB 20 bảng dùng chung + S3 PackStore qua VPC Gateway Endpoints | DynamoDB 20 bảng dùng chung + S3 PackStore qua Internet HTTPS | Badger (`local_data_path`) + S3 loopback (Moto) hoặc benchmark HTTPS |
| **Clock / UTC proof** | Chrony `169.254.169.123` | Chrony NTP IPv4 đã phê duyệt (`utc_reference`) | Simulated local clock (không dùng cho proof production) |

### 1.1 Tuyệt đối không lẫn ba miền credential

Hệ thống tách biệt cứng ba loại credential. Gateway từ chối mọi thao tác dùng nhầm loại credential:

| Miền credential | Định dạng & nguồn cấp | Nơi sử dụng hợp lệ | Hành vi khi dùng nhầm bề mặt |
|---|---|---|---|
| **1. AWS IAM Backend** | Cloud: EC2 IMDSv2 instance profile.<br/>Edge: profile `miles-runtime` (`credential_process` IAM Roles Anywhere) trong `aws_config_file`.<br/>Local: dummy `mileslocal` / `pack_credentials_file`. | Chỉ nội bộ gateway gọi S3 PackStore, DynamoDB, KMS, Secrets Manager; và `dsgctl admin-token bootstrap` / `lifecycle-upgrade-gate` (provisioning role tạm). | Gửi IAM access key tới S3 API của Miles → `403 InvalidAccessKeyId`. Không dùng làm Admin bearer → `401 unauthorized`. |
| **2. DSG SigV4 S3 Key** | `access_key_id`: 20 ký tự `[A-Z0-9]`; `secret_access_key`: Base64 40 ký tự. Cấp qua `POST /admin/v1/credentials` (`dsgctl credentials create`). | Chỉ client S3 (`aws`, `boto3`, Go SDK, presigned URL) gọi S3 API (`:8080` / `:443`). | Gửi tới Admin API (`/admin/v1/*`) → `401 unauthorized`. Không có quyền gọi AWS S3/DynamoDB backend. |
| **3. DSG Admin Bearer Token** | `<token_id>.<secret>` (`token_id` 32 hex thường, `secret` 43 ký tự base64url). Bootstrap offline lần đầu, sau đó cấp qua `POST /admin/v1/admin-tokens`. | Chỉ `dsgctl` / Admin API (`/admin/v1/*`) qua loopback HTTP / SSH tunnel hoặc HTTPS. Lưu file regular `0600` (`--token-file` / `DSGCTL_TOKEN_FILE`). | Gửi `Authorization: Bearer ...` tới S3 API → `400 InvalidArgument` (không kích hoạt S3 auth hay `x-dsg-debug`). |

> **Quy tắc an toàn:** Khi chạy client trên máy operator hoặc Edge node, luôn cô lập profile/env của client khỏi IAM backend (`AWS_EC2_METADATA_DISABLED=true`, dùng profile riêng `[profile dsg-cloud]` / `[profile dsg-edge]` hoặc `AWS_SHARED_CREDENTIALS_FILE` riêng). Không dùng chung key backend làm key DSG.

---

## 2. Hướng dẫn cấu hình Client trên hai Profile (Cloud & Edge)

### 2.1 Cấu hình `~/.aws/config` và `~/.aws/credentials`

Cấu hình chuẩn cho AWS CLI (đã pin `2.36.41` / `2.39.3`) và `boto3` (`1.43.114`):

```ini
# ~/.aws/config (tách riêng profile client khỏi backend)
[profile dsg-cloud]
region = ap-southeast-1
s3 =
  addressing_style = path
  request_checksum_calculation = when_supported
  response_checksum_validation = when_supported

[profile dsg-edge]
region = ap-southeast-1
s3 =
  addressing_style = path
  request_checksum_calculation = when_supported
  response_checksum_validation = when_supported

# Profile chuyên dụng cho CopyObject < 5 GiB bằng lệnh high-level aws s3 cp/mv
[profile dsg-copy]
region = ap-southeast-1
s3 =
  addressing_style = path
  multipart_threshold = 5GB
  multipart_chunksize = 5GB
```

```ini
# ~/.aws/credentials (chứa DSG SigV4 key từ dsgctl credentials create, KHÔNG đặt IAM backend key)
[dsg-cloud]
aws_access_key_id = <DSG_ACCESS_KEY_ID>
aws_secret_access_key = <DSG_SECRET_ACCESS_KEY>

[dsg-edge]
aws_access_key_id = <DSG_ACCESS_KEY_ID>
aws_secret_access_key = <DSG_SECRET_ACCESS_KEY>

[dsg-copy]
aws_access_key_id = <DSG_ACCESS_KEY_ID>
aws_secret_access_key = <DSG_SECRET_ACCESS_KEY>
```

### 2.2 Biến môi trường theo Profile

```sh
# --- Profile Cloud (qua ALB HTTPS :443, verify TLS bằng CA riêng) ---
export AWS_PROFILE=dsg-cloud
export AWS_CA_BUNDLE=/private/miles-ca.pem
export AWS_EC2_METADATA_DISABLED=true
export ENDPOINT=https://cloud.miles.example:443

# --- Profile Edge (qua Edge TLS proxy / loopback, giữ nguyên Host header) ---
export AWS_PROFILE=dsg-edge
export AWS_CA_BUNDLE=/private/miles-ca.pem
export AWS_EC2_METADATA_DISABLED=true
export ENDPOINT=https://edge.miles.example:443
# Hoặc loopback local-dev (chỉ khi dev/test trên máy local):
# export ENDPOINT=http://127.0.0.1:8080
```

> Không dùng `--no-verify-ssl`. Luôn truyền `AWS_CA_BUNDLE` (hoặc `curl --cacert`) khi dùng CA nội bộ.

### 2.3 Các thao tác S3 cơ bản (P0/P1)

```sh
# Bucket P0/P1
aws --endpoint-url "$ENDPOINT" s3 mb s3://demo
aws --endpoint-url "$ENDPOINT" s3 ls
aws --endpoint-url "$ENDPOINT" s3api head-bucket --bucket demo
aws --endpoint-url "$ENDPOINT" s3api get-bucket-location --bucket demo
aws --endpoint-url "$ENDPOINT" s3 rb s3://demo   # bucket phải trống và không còn upload active

# Object P0/P1
aws --endpoint-url "$ENDPOINT" s3 cp small.txt s3://demo/dir/small.txt
aws --endpoint-url "$ENDPOINT" s3 cp s3://demo/dir/small.txt ./small.downloaded.txt
aws --endpoint-url "$ENDPOINT" s3 ls s3://demo --recursive
aws --endpoint-url "$ENDPOINT" s3api head-object --bucket demo --key dir/small.txt
aws --endpoint-url "$ENDPOINT" s3api get-object --bucket demo --key dir/small.txt --range bytes=0-1023 part.bin
aws --endpoint-url "$ENDPOINT" s3api get-object --bucket demo --key dir/small.txt --if-match '"<etag>"' verified.bin
aws --endpoint-url "$ENDPOINT" s3 rm s3://demo/dir/small.txt
aws --endpoint-url "$ENDPOINT" s3api delete-objects --bucket demo --delete '{"Objects":[{"Key":"a"},{"Key":"b"}],"Quiet":false}'
```

### 2.4 Bảng Checksum P0/P1 (§2.7)

| Operation | Algorithm hỗ trợ | Ghi chú |
|---|---|---|
| `PutObject` | `Content-MD5` hoặc `CRC64NVME`, `CRC32`, `CRC32C`, `SHA1`, `SHA256` (header hoặc aws-chunked trailer) | Mặc định SDK/gateway dùng `CRC64NVME` (`FULL_OBJECT`); `SHA512`, `XXHASH*`, `x-amz-checksum-md5` bị từ chối `400 InvalidRequest` trước đọc body. |
| `CreateMultipartUpload` / `UploadPart` / `CompleteMultipartUpload` | `FULL_OBJECT`: `CRC64NVME`, `CRC32`, `CRC32C`<br/>`COMPOSITE`: `CRC32`, `CRC32C`, `SHA1`, `SHA256` | Không chọn algorithm → `CRC64NVME` + `FULL_OBJECT`. Chọn algorithm ngoài `CRC64NVME` mà bỏ trống `x-amz-checksum-type` → `COMPOSITE`. `SHA*` không hỗ trợ `FULL_OBJECT`; `CRC64NVME` không hỗ trợ `COMPOSITE`. |
| `GetObject` / `HeadObject` | Trả header checksum đã lưu khi gửi `x-amz-checksum-mode: ENABLED` (toàn object; **không** trả full-object checksum khi GET có `Range`). | Gửi `x-amz-checksum-mode` khác `ENABLED` hoặc ngoài GET/HEAD → `400 InvalidRequest`. |

---

## 3. Xác thực SigV4, Phân quyền & Header Debug `x-dsg-*`

### 3.1 Quy tắc SigV4 & Snapshot quyền

- Chỉ chấp nhận `AWS4-HMAC-SHA256` (header `Authorization` hoặc presigned query `X-Amz-*`). **SigV2 trả `501 NotImplemented`. Anonymous trả `403 AccessDenied`.**
- Bắt buộc `X-Amz-Date` (ISO 8601 basic, `Z`, lệch clock tối đa `±15 phút`, quá → `403 RequestTimeTooSkewed`) và `x-amz-content-sha256` (hex SHA-256, `UNSIGNED-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER`).
- `SignedHeaders` phải chứa `host` và mọi header `x-amz-*` có trong request (thiếu → `403 AccessDenied`).
- Canonical URI dùng raw path **nhận trực tiếp** (decode đúng một lần khi chọn `Route.Key`; `+` giữ nguyên `+`, không normalize `..` hay `//`; key phải là UTF-8 hợp lệ ≤ 1024 byte).
- Snapshot quyền (`auth.Snapshots`) cache tối đa **5 giây** (chặn thêm bởi `expires_at` của bearer token); mọi write transaction kiểm tra lại trạng thái user/credential/bucket/grant ngay lúc commit.

### 3.2 Ma trận phân quyền S3 & Admin

- **Bucket owner:** có cả `read` và `write` trên bucket mình tạo; quản lý grants và cache policy của bucket qua Admin API.
- **Grant nội bộ:** `read` và `write` độc lập (`write` không tự cấp `GET`/`HEAD`/`LIST`/`RestoreObject`).
- **`system_admin`:** chỉ quản trị hạ tầng và debug; **không có quyền đọc/ghi dữ liệu object ngầm định**. `GET`/`PUT`/`CopyObject`/`RestoreObject` của system admin vẫn yêu cầu là owner hoặc có grant tường minh (ngoại lệ vận hành duy nhất: `DeleteBucket`, `AbortMultipartUpload`, `ListParts`).

### 3.3 Gate `x-dsg-debug: 1` và các header `x-dsg-*`

**Gate bắt buộc (phải thỏa đồng thời cả 4 điều kiện):**
1. Header `x-dsg-debug` có đúng một giá trị `"1"` **và** nằm trong `SignedHeaders` (kể cả presigned URL).
2. Credential SigV4 có cả `system_admin=true` **và** `debug_allowed=true`.
3. Caller qua kiểm tra phân quyền dữ liệu bình thường (owner hoặc grant `read`/`write` tương ứng với operation).
4. Request đi qua cổng S3 bằng SigV4 (bearer Admin API không bao giờ mở debug S3).

**Các ca âm bắt buộc (không bao giờ lộ header vật lý):**
- `aws --debug` chỉ ghi log client, **không** gửi hay ký `x-dsg-debug`.
- User thường (kể cả bucket owner hoặc grantee có full read+write) tự ký `x-dsg-debug: 1`: request chạy bình thường (`200`), **không trả** bất kỳ `x-dsg-*` nào.
- `system_admin=true` nhưng `debug_allowed=false`: **không trả** `x-dsg-*`.
- `x-dsg-debug: 1` không nằm trong `SignedHeaders` (hoặc giá trị khác `"1"` / gửi nhiều giá trị): request vẫn hợp lệ, **bỏ qua** debug.
- `system_admin=true, debug_allowed=true` nhưng không là owner và không có grant: trả `403 AccessDenied`, **không trả** `x-dsg-*`.
- Query `?x-dsg-debug=1` (thường hoặc presigned): trả `501 NotImplemented`.

**Danh sách header `x-dsg-*` thực tế:**

| Header | Operation | Giá trị & ý nghĩa |
|---|---|---|
| `x-dsg-chunks-total` | `PutObject`, `UploadPart` | Tổng số lần xuất hiện chunk trong body logical sau decode `aws-chunked`. |
| `x-dsg-chunks-dup` | `PutObject`, `UploadPart` | Số chunk occurrences được reuse mà request không cần ghi blob mới (gồm dedup trước đó và chunk lặp trong cùng request). |
| `x-dsg-chunks-revived` | `PutObject`, `UploadPart` | Số identity `(domain, hash)` riêng biệt đã có ở trạng thái archive/Restored nhưng request này ghi bản `Warm` thay thế. |
| `x-dsg-bytes-logical` | `PutObject`, `UploadPart` | Tổng byte logical client gửi (sau decode `aws-chunked`). |
| `x-dsg-bytes-stored` | `PutObject`, `UploadPart` | Tổng byte pack + sidecar request thực PUT thành công (gồm header/footer và blob race thua; `0` nếu toàn bộ chunk đều được reuse). |
| `x-dsg-cache` | `GetObject` (`200`/`206`) | Trạng thái cache kế hoạch lúc pre-flight: `hit` (mọi chunk có sẵn trong cache node), `partial` (một phần), `miss` (không có hoặc object rỗng). |
| `x-dsg-restore` | `GetObject` / cross-domain `CopyObject` lỗi `403 InvalidObjectState` | `requested` (request vừa khởi tạo job restore), `in-progress` (pack đã có restore đang chạy); bỏ qua header này nếu `auto_restore=false` hoặc khởi tạo thất bại. |

**Ví dụ `boto3` bật `x-dsg-debug: 1` trước bước ký (`before-sign`):**

```python
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="https://cloud.miles.example:443",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)

def add_debug_header(request, **kwargs):
    request.headers["x-dsg-debug"] = "1"

s3.meta.events.register("before-sign.s3.PutObject", add_debug_header)
s3.meta.events.register("before-sign.s3.GetObject", add_debug_header)

put_resp = s3.put_object(Bucket="demo", Key="debug.bin", Body=b"demo-payload")
print({k: v for k, v in put_resp["ResponseMetadata"]["HTTPHeaders"].items() if k.startswith("x-dsg-")})
```

---

## 4. Quản trị Users, Grants, Policy, Credentials & Tokens (`dsgctl`)

### 4.1 Kết nối `dsgctl` trên Cloud & Edge

`dsgctl` luôn đặt global flags (`--endpoint`, `--token-file`, `--timeout`) **trước** subcommand, và flags của command **sau** positional arguments. Plain HTTP chỉ được chấp nhận cho literal loopback (`127.0.0.1` / `[::1]`); endpoint từ xa bắt buộc HTTPS. Token file phải là file regular, không symlink, permission `0600`.

```sh
# Profile Cloud: mở SSH tunnel tới node khỏe trong VPC qua bastion
ssh -N -L 127.0.0.1:19000:<gateway_private_ip>:9000 <ssh_user>@<bastion_host>
# Hoặc dùng helper kiểm soát readiness & tunnel rotation:
python3 -B scripts/admin_tunnel.py --ssh-config /private/ssh-config \
  --target cloud-a --target cloud-b --token-file /private/owner-token --user-id owner

export DSGCTL_ENDPOINT=http://127.0.0.1:19000
export DSGCTL_TOKEN_FILE=/private/system-admin-token

# Profile Edge (trên máy Edge hoặc qua SSH tunnel tới Edge):
export DSGCTL_ENDPOINT=http://127.0.0.1:9000
export DSGCTL_TOKEN_FILE=/private/system-admin-token
```

### 4.2 Bootstrap offline lần đầu

Chạy **khi mọi gateway đang dừng**, dùng role provisioning tách biệt (không truyền vào gateway runtime):

```sh
# Local Badger mode
./bin/dsgctl admin-token bootstrap \
  --user ops --ttl 24h --token-file /private/system-admin-token \
  --local-data-path /var/lib/miles-local

# Shared Cloud/Edge (dùng operator runtime config có credential_process tạm của provisioning role)
./bin/dsgctl admin-token bootstrap \
  --runtime-config /private/operator-startup.json \
  --user ops --ttl 24h --token-file /private/system-admin-token
```

- Tạo file `0600` (từ chối nếu đã tồn tại), chỉ in `{"token_id":...,"expires_at":...}` ra stdout.
- Chạy lần thứ hai trên cùng installation sẽ bị từ chối và không ghi đè state.

### 4.3 Luồng đầy đủ: User → Credentials → Owner Token → Grants → Bucket Policy

```sh
# 1. System admin tạo user mới (mặc định default_domain="default", logical_limit=1 TiB)
./bin/dsgctl user create alice --default-domain default --logical-limit 107374182400
./bin/dsgctl user create bob

# 2. Cấp SigV4 key cho client S3 (secret chỉ ghi vào --out file mới mode 0600, stdout bị [REDACTED])
./bin/dsgctl credentials create --user alice --out /private/alice-s3-secret.json
./bin/dsgctl credentials create --user bob --out /private/bob-s3-secret.json

# Cấp SigV4 key cho admin có quyền debug x-dsg-* (bắt buộc kèm --system-admin)
./bin/dsgctl credentials create --user ops --system-admin --debug-allowed --out /private/ops-debug-s3.json

# 3. Cấp bearer token riêng cho chủ bucket alice (system_admin=false)
./bin/dsgctl admin-token create --user alice --ttl 24h --out /private/alice-admin-token

# 4. Alice (owner) dùng token của mình lấy bucket_id và cấp/đổi/thu hồi grant cho bob
export DSGCTL_TOKEN_FILE=/private/alice-admin-token
./bin/dsgctl bucket show demo
# Lấy bucket_id từ output (VD: bkt-demo-001)

# Tạo grant mới: bắt buộc --expected-revision 0 và --expected-bucket-id
./bin/dsgctl grant set demo bob --read=true --write=false \
  --expected-revision 0 --expected-bucket-id bkt-demo-001

# Cập nhật grant hiện có (revision 1 -> 2): thêm quyền write
./bin/dsgctl grant set demo bob --read=true --write=true \
  --expected-revision 1 --expected-bucket-id bkt-demo-001

# Liệt kê grants & đọc logical usage của chính mình
./bin/dsgctl grant ls demo --limit 100
./bin/dsgctl usage alice --detail

# Chỉnh bucket cache policy (chỉ cho phép cache, reject tiering/compression/domain)
./bin/dsgctl bucket-policy get demo
./bin/dsgctl bucket-policy set demo --admit-on-write=true --readahead-chunks 4 \
  --expected-revision 1 --expected-bucket-id bkt-demo-001

# Thu hồi grant (không dùng grant set --read=false --write=false; API bắt buộc DELETE)
./bin/dsgctl grant rm demo bob --expected-bucket-id bkt-demo-001
```

> **Lưu ý khi dùng `mode=local`:** trong chế độ single-node dev `local`, `LocalAuthority` chỉ cho phép user `dev` thực hiện ghi S3 object; thao tác ghi từ grantee khác chạy trên shared Cloud/Edge hoặc metadata runtime đầy đủ.
>
> **Lưu ý bucket policy revision 1 trên Shared runtime:** sau khi `CreateBucket`, nên gọi `dsgctl bucket-policy get <bucket>` (hoặc `GET /admin/v1/buckets/{b}/policy`) để materialize revision `1` trước khi chỉnh tiếp với `--expected-revision 1`.

### 4.4 Lệnh vận hành System Admin

```sh
export DSGCTL_TOKEN_FILE=/private/system-admin-token

./bin/dsgctl stats
./bin/dsgctl stats --bucket demo
./bin/dsgctl layout demo dir/small.txt
./bin/dsgctl policy get
./bin/dsgctl policy set --file policy-patch.json --expected-revision 1
./bin/dsgctl cost report --scenario A --profile cloud --workload cloud-tiering-boundary
./bin/dsgctl cost report --scenario B --profile edge --workload edge-revival
./bin/dsgctl cost report --scenario A --profile cloud   # đánh giá snapshot stats live

./bin/dsgctl pack ls --state Cold --storage-class GLACIER_IR --limit 50
./bin/dsgctl pack show <pack_id>
./bin/dsgctl pack restore <pack_id> <generation> --days 2 --tier Bulk
./bin/dsgctl pack promote <pack_id> <generation>

./bin/dsgctl cache stats --scope cluster
./bin/dsgctl cache stats --scope node --node edge-1
./bin/dsgctl cache purge --scope node --node edge-1

./bin/dsgctl job ls --limit 50
./bin/dsgctl job run scrub --dry-run
./bin/dsgctl job show <job_id>
./bin/dsgctl job cancel <job_id>

./bin/dsgctl node ls
./bin/dsgctl node drain <node_id>

# Fault injection (chỉ chạy khi enable_fault_injection=true; scope chỉ hỗ trợ "node")
./bin/dsgctl fault ls
./bin/dsgctl fault add --type s3_5xx --scope node --node <target_node_id> --rate 0.5 --duration 60
./bin/dsgctl fault rm <fault_id>
```

---

## 5. Hướng dẫn Presigned URL

### 5.1 Yêu cầu bắt buộc

1. **Bắt buộc SigV4 (`s3v4`)**: trong `boto3`, phải cấu hình `Config(signature_version="s3v4", s3={"addressing_style": "path"})`. `boto3` mặc định nếu không đặt `signature_version` có thể sinh SigV2 (`AWSAccessKeyId`/`Signature`/`Expires`) và bị DSG trả `501 NotImplemented`.
2. **Hạn sử dụng `X-Amz-Expires`:** số nguyên từ `1` đến `604800` giây (tối đa 7 ngày); ngoài khoảng → `400 InvalidArgument`; hết hạn (`now - date > expires`) → `403 AccessDenied`.
3. **Host & Path preservation:** chữ ký gắn chặt `Host` (kể cả `:443` nếu client ký có port) và path; proxy/ALB không được viết lại `Host` hay normalize path.
4. **Hiệu lực thu hồi (≤ 5 giây):** URL còn hạn vẫn bị từ chối nếu credential bị `DELETE /credentials/{id}`, user bị `disabled`, hoặc grant bị thu hồi (snapshot hết hạn tối đa 5 giây).
5. **Hoisted headers & ràng buộc:** các query `x-amz-*` đã ký (như `X-Amz-Checksum-Sha256`, `X-Amz-Meta-*`, `X-Amz-Copy-Source`) được kiểm tra y hệt header thường. Gửi song song header và query `x-amz-*` có giá trị khác nhau → `400 InvalidArgument`. `response-content-type` / `response-content-disposition` / ... là P2 → `501 NotImplemented`.

### 5.2 Ví dụ AWS CLI, boto3 và curl

```sh
# 1. Sinh presigned GET URL bằng AWS CLI và tải bằng curl
URL=$(aws --endpoint-url "$ENDPOINT" s3 presign s3://demo/dir/small.txt --expires-in 900)
curl --fail --cacert "$AWS_CA_BUNDLE" -o downloaded.txt "$URL"
```

```python
import hashlib, base64, boto3, requests
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="https://cloud.miles.example:443",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)

# Presigned PUT kèm SHA-256 checksum trong query (DSG verify trước khi commit)
data = b"presigned verified payload"
sha_b64 = base64.b64encode(hashlib.sha256(data).digest()).decode()
put_url = s3.generate_presigned_url(
    "put_object",
    Params={
        "Bucket": "demo",
        "Key": "presigned.bin",
        "ContentLength": len(data),
        "ChecksumSHA256": sha_b64,
    },
    ExpiresIn=900,
)
resp = requests.put(
    put_url,
    data=data,
    headers={"x-amz-checksum-sha256": sha_b64},
    verify="/private/miles-ca.pem",
)
resp.raise_for_status()
```

> Lưu ý: khi presign có `x-amz-checksum-sha256` trong `X-Amz-SignedHeaders`, client gọi `PUT` phải gửi đúng header tương ứng hoặc truyền qua query không xung đột. Nếu byte gửi bị sai checksum → gateway trả `400 BadDigest` và không công bố object.

---

## 6. Hướng dẫn Multipart Upload (P1)

### 6.1 Giới hạn và vòng đời

| Ràng buộc | Giá trị | Lỗi khi vi phạm |
|---|---|---|
| Số lượng part / upload | `1 .. 10000` | `400 InvalidArgument` (`Part number must be an integer between 1 and 10000`) |
| Kích thước mỗi part | `UploadPart` tối đa `5 GiB`; mọi part **không phải cuối** khi `Complete` phải `>= 5 MiB` | `400 EntityTooLarge` / `400 EntityTooSmall` |
| Tổng kích thước object | Tối đa `100 GiB` | `400 EntityTooLarge` |
| Số upload Active / owner | Tối đa `16` | `403 QuotaExceeded` |
| Staging headroom / owner | `bytes_logical_used + reserved + staged <= logical_limit + 100 GiB` | `403 QuotaExceeded` |
| Thời hạn upload Active | `24 giờ` từ `CreateMultipartUpload` | `404 NoSuchUpload` |
| Lưu giữ Complete receipt | `7 ngày` từ lần `Complete` commit thành công (kể cả qua hạn 24h) | `404 NoSuchUpload` sau 7 ngày |

### 6.2 Quyền và Upload Binding

- Mọi operation dùng `uploadId` (`UploadPart`, `Complete`, `Abort`, `ListParts`) gắn chặt `(bucket_id, key, initiator)`. Dùng nhầm bucket, nhầm key hoặc bucket vừa xóa tạo lại → `404 NoSuchUpload`.
- **`UploadPart` & `CompleteMultipartUpload`:** yêu cầu `write` **và** caller đúng là `initiator` (owner hoặc system admin cũng không upload/complete hộ initiator).
- **`ListParts`:** `(read && initiator) || owner || system_admin`.
- **`AbortMultipartUpload`:** `(write && initiator) || owner || system_admin`.

### 6.3 Thứ tự Complete, Checksum & Idempotency

- Hỗ trợ upload nhiều part song song hoặc sai thứ tự; cùng `partNumber` người publish cuối thắng khi upload còn `Active`.
- Trong `CompleteMultipartUpload`, danh sách `Part` phải khác rỗng và `PartNumber` tăng nghiêm ngặt (`1 < 2 < ...`). Nếu upload dùng mode `COMPOSITE`, bắt buộc đủ `1..N` và mỗi `<Part>` phải mang `<Checksum...>` khớp giá trị đã verify khi `UploadPart`.
- Retry `Complete` với **cùng canonical input** trong vòng 7 ngày trả lại đúng receipt ban đầu (`200 OK`, không cộng quota lần hai). Đổi danh sách part hoặc checksum khác input đã freeze → `400 InvalidRequest`.
- Nếu đang `Completing` trong lease `60s + 5s grace` → `409 OperationAborted` (client retry sau vài giây). Abort một upload đang `Completing` hoặc `Completed` → `409 OperationAborted`.

### 6.4 Ví dụ AWS CLI & s3api

```sh
# High-level (tự chuyển multipart khi file >= multipart_threshold, mặc định 8 MiB)
aws --endpoint-url "$ENDPOINT" s3 cp large.tar s3://demo/large.tar

# Low-level s3api từng bước
UPLOAD_ID=$(aws --endpoint-url "$ENDPOINT" s3api create-multipart-upload \
  --bucket demo --key manual.bin --checksum-algorithm CRC64NVME --query UploadId --output text)

ETAG1=$(aws --endpoint-url "$ENDPOINT" s3api upload-part \
  --bucket demo --key manual.bin --part-number 1 --upload-id "$UPLOAD_ID" \
  --body part1.bin --query ETag --output text)

aws --endpoint-url "$ENDPOINT" s3api list-parts \
  --bucket demo --key manual.bin --upload-id "$UPLOAD_ID"

aws --endpoint-url "$ENDPOINT" s3api complete-multipart-upload \
  --bucket demo --key manual.bin --upload-id "$UPLOAD_ID" \
  --multipart-upload "{\"Parts\":[{\"PartNumber\":1,\"ETag\":$ETAG1}]}"

# Hoặc hủy upload
aws --endpoint-url "$ENDPOINT" s3api abort-multipart-upload \
  --bucket demo --key manual.bin --upload-id "$UPLOAD_ID"
```

---

## 7. Hướng dẫn giới hạn CopyObject & Workarounds (P1 / P2)

### 7.1 Giới hạn thực thi

- **Single `CopyObject` (`PUT /{dst_bucket}/{dst_key}` + `x-amz-copy-source`):** kích thước object nguồn tối đa **`5 GiB` (`5,368,709,120` byte)**. Nguồn `> 5 GiB` → `400 InvalidRequest`.
- **`UploadPartCopy` là P2 (`501 NotImplemented`)** và `GetObjectTagging` ngoài phạm vi (`501 NotImplemented`).
- **Tác động tới `aws s3 cp` / `aws s3 mv` giữa hai key S3:**
  - Khi `size < multipart_threshold` (mặc định AWS CLI `8 MiB = 8,388,608` byte): `aws s3 cp/mv` gọi `CopyObject` đơn → **thành công**.
  - Khi `size >= multipart_threshold` (`>= 8 MiB` mặc định): AWS CLI `2.36.41` / `2.39.3` gọi `GetObjectTagging` rồi `UploadPartCopy` → **thất bại với `501 NotImplemented`**.
- **Quyền:** cần `read` trên bucket nguồn **và** `write` trên bucket đích; quota tính vào chủ bucket đích theo dung lượng logical (bản copy cùng domain không ghi thêm pack mới).

### 7.2 Cách dùng chuẩn theo từng ngưỡng kích thước

| Kích thước nguồn | Cách thực hiện khuyến nghị | Ghi chú |
|---|---|---|
| `< 5 GiB` (lệnh `aws s3 cp` / `mv`) | Dùng profile riêng `dsg-copy` đặt `multipart_threshold = 5GB`:<br/>`aws --profile dsg-copy --endpoint-url "$ENDPOINT" s3 cp s3://src/file s3://dst/file` | Không sửa threshold ở profile mặc định để giữ nguyên benchmark multipart upload. Lưu ý `aws s3 mv` không nguyên tử (copy xong mới `DeleteObject` nguồn). |
| `<= 5 GiB` (kể cả đúng `5 GiB`) | Dùng lệnh `s3api copy-object` gọi trực tiếp `CopyObject`:<br/>`aws --endpoint-url "$ENDPOINT" s3api copy-object --bucket dst --key dst-key --copy-source "src/src%20key"` | `copy-source` phải URL-encode khi key có khoảng trắng/ký tự đặc biệt. |
| `> 5 GiB` | Không có server-side copy ở P1: tải xuống rồi upload lại qua multipart:<br/>`aws --endpoint-url "$ENDPOINT" s3 cp s3://src/huge.bin ./tmp.bin && aws --endpoint-url "$ENDPOINT" s3 cp ./tmp.bin s3://dst/huge.bin` | Nếu nguồn ở `GLACIER` chưa restore, phải `restore-object` trước khi tải về. |

### 7.3 Metadata Directive, Self-copy, ETag và Cross-domain

- `x-amz-metadata-directive=COPY` (mặc định): giữ nguyên HTTP metadata nguồn (`Content-Type`, `x-amz-meta-*`, ...), giữ ETag và checksum descriptor nguồn như opaque metadata (kể cả multipart ETag `-N` / `COMPOSITE`). **Self-copy với `COPY`** (cùng `bucket_id` và `key`) trả **`400 InvalidRequest`**.
- `x-amz-metadata-directive=REPLACE`: thay toàn bộ HTTP metadata từ header request (mặc định `Content-Type: application/octet-stream` nếu không gửi). **Self-copy với `REPLACE`** hợp lệ (`200 OK`, tạo revision mới, quota delta = `0`).
- **Đổi checksum algorithm khi Copy (`x-amz-checksum-algorithm` khác nguồn):** thuộc P2 → trả `400 InvalidRequest` trước khi mutate.
- **Cùng miền vs Khác miền:**
  - *Cùng domain:* copy chỉ tham chiếu metadata trong miền đó (thành công ngay cả khi nguồn đang ở `GLACIER` Cold; bản copy kế thừa tính cần restore nếu đọc).
  - *Khác domain:* tra từng chunk ở domain đích trước → reuse chunk đích còn đọc được (`Warm`/`GLACIER_IR`/`Restored` đủ deadline + `120s`) → chỉ tải, verify SHA-256 và ghi pack `Warm` đích cho chunk thiếu. Nếu chunk nguồn cần đọc nằm ở `GLACIER` chưa restore → trả `403 InvalidObjectState` (kèm `auto_restore` nếu bật).

---

## 8. Hướng dẫn Cold Storage & RestoreObject (P1)

### 8.1 Hành vi theo lớp lưu trữ

| Lớp pack chứa dữ liệu | `GET` | `HEAD` (`x-amz-restore`) | `RestoreObject` |
|---|---|---|---|
| `STANDARD` / `STANDARD_IA` / `GLACIER_IR` | Đọc ngay (`200`/`206`) | Không trả `x-amz-restore` | `400 InvalidRequest` (`object has no GLACIER data`) |
| `GLACIER` chưa restore | `403 InvalidObjectState` trước header (kích hoạt `auto_restore` nếu policy bật) | Không trả `x-amz-restore` | `202 Accepted` (bắt đầu restore ít nhất 1 pack) |
| `GLACIER` đang restoring | `403 InvalidObjectState` | `x-amz-restore: ongoing-request="true"` | `409 RestoreAlreadyInProgress` |
| `GLACIER` đã Restored (còn hạn ≥ deadline + `120s`) | Đọc ngay (`200`/`206`) | `x-amz-restore: ongoing-request="false", expiry-date="<min expiry>"` | `200 OK` nếu mọi pack đã đủ `Days`; `202 Accepted` nếu gia hạn hoặc restore lại pack hết hạn |

- `HEAD` chỉ là snapshot thông tin (tổng hợp expiry **nhỏ nhất** trên các pack `GLACIER` hiện tại của object); `HEAD` không tự kích hoạt restore.
- `RestoreObject` nhận `Days` từ **`1` đến `30`** (giới hạn DSG) và `GlacierJobParameters.Tier` thuộc `Standard` (mặc định) | `Bulk` | `Expedited`. Ngoài khoảng hoặc thiếu `Days` → `400 InvalidRequest`. `Type=SELECT`, `SelectParameters`, `OutputLocation` → `501 NotImplemented`.
- `PUT` / `UploadPart` có dữ liệu trùng chunk đang ở `GLACIER` hoặc `Restored` sẽ tự động **revive** ghi bản `Warm` mới từ body client (không cần restore).

```sh
# Kiểm tra trạng thái archive trước khi đọc
aws --endpoint-url "$ENDPOINT" s3api head-object --bucket demo --key archive.tar

# Yêu cầu restore 3 ngày với Tier Standard (hoặc Bulk / Expedited)
aws --endpoint-url "$ENDPOINT" s3api restore-object \
  --bucket demo --key archive.tar \
  --restore-request '{"Days":3,"GlacierJobParameters":{"Tier":"Standard"}}'

# Poll HEAD tới khi ongoing-request="false" rồi mới GET
aws --endpoint-url "$ENDPOINT" s3api head-object --bucket demo --key archive.tar
aws --endpoint-url "$ENDPOINT" s3 cp s3://demo/archive.tar ./archive.tar
```

---

## 9. Error Schemas thực tế của API

### 9.1 S3 API Error Schema (XML — `internal/api/s3/errors.go`)

Mọi phản hồi lỗi S3 đều mang các header chung:
- `Server: DedupStorageGateway`
- `X-Amz-Request-Id: <16 uppercase hex>`
- `X-Amz-Id-2: <16 uppercase hex>`
- `Content-Type: application/xml` (riêng method `HEAD` **không có body** và không đặt `Content-Type`/`Content-Length` XML)
- Nếu `x-dsg-debug: 1` qua được toàn bộ gate và lỗi là `InvalidObjectState`, kèm `X-Dsg-Restore: requested|in-progress`.
- Nếu lỗi xảy ra **sau khi** đã gửi HTTP `200`/`206` và header của `GetObject`, gateway abort stream (`http.ErrAbortHandler`) — **không** nối XML error vào cuối stream dữ liệu.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>backup.tar</Key><BucketName>demo</BucketName><RequestId>4442587FB7D0A2F9</RequestId></Error>
```

*(Các phần tử `<Key>` và `<BucketName>` có `omitempty`, chỉ xuất hiện khi gắn với bucket/key tương ứng.)*

**Bảng đầy đủ 33 mã lỗi S3 thực thi (`internal/api/s3/errors.go`):**

| `Code` | HTTP | Message mặc định | Khi xảy ra |
|---|---|---|---|
| `AccessDenied` | 403 | `Access Denied` | Thiếu chữ ký, thiếu `host`/`x-amz-*` trong `SignedHeaders`, presigned hết hạn, user `disabled`, thiếu quyền owner/grant/initiator. |
| `InvalidAccessKeyId` | 403 | `The access key ID you provided does not exist in our records.` | Access key không tồn tại hoặc đã `revoked`. |
| `SignatureDoesNotMatch` | 403 | `The request signature we calculated does not match the signature you provided.` | Sai HMAC-SHA256 chữ ký request hoặc chunk/trailer. |
| `RequestTimeTooSkewed` | 403 | `The difference between the request time and the server's time is too large.` | Lệch `X-Amz-Date` quá `±15 phút` (hoặc future skew ở presigned). |
| `InvalidObjectState` | 403 | `The operation is not valid for the object's storage class.` | Đọc/copy `GLACIER` pack chưa restore hoặc bản Restored không đủ deadline + `120s`. |
| `QuotaExceeded` | 403 | `The logical storage quota of the bucket owner is exceeded.` | Vượt `logical_limit`, vượt cap `16` Active upload hoặc staging `limit + 100 GiB`. |
| `NoSuchBucket` | 404 | `The specified bucket does not exist.` | Bucket không tồn tại hoặc đang deleting. |
| `NoSuchKey` | 404 | `The specified key does not exist.` | Object không tồn tại (hoặc tombstone) khi caller có quyền `read`. |
| `NoSuchUpload` | 404 | `The specified upload does not exist.` | `uploadId` sai định dạng, không tồn tại, lệch `bucket_id`/`key`, hết hạn `Active`, `Aborted`/`Aborting`, hoặc `Completed` quá 7 ngày. |
| `MethodNotAllowed` | 405 | `The specified method is not allowed against this resource.` | HTTP method không hợp lệ ở resource level (service/bucket/object). |
| `BucketAlreadyOwnedByYou` | 409 | `Your previous request to create the named bucket succeeded and you already own it.` | `CreateBucket` trùng tên bucket chính mình đang sở hữu. |
| `BucketAlreadyExists` | 409 | `The requested bucket name is not available.` | `CreateBucket` trùng tên bucket của user khác. |
| `RestoreAlreadyInProgress` | 409 | `Object restore is already in progress.` | `RestoreObject` không khởi tạo mới pack nào vì restore đang chạy. |
| `OperationAborted` | 409 | `A conflicting operation is currently in progress against this resource.` | Tranh chấp CAS revision/metadata, `Complete`/`UploadPart`/`Abort` đụng upload đang `Completing` trong lease+grace. |
| `BucketNotEmpty` | 409 | `The bucket you tried to delete is not empty.` | `DeleteBucket` khi còn object sống hoặc multipart upload hoạt động. |
| `InvalidRequest` | 400 | `Invalid request.` | Tổ hợp query/method sai, checksum/type không hợp lệ, `CreateBucket` sai region, self-copy `COPY`, copy `> 5 GiB`, `RestoreObject` cho Warm/IR hoặc `Days` ngoài `1..30`, đổi `Complete` input, thiếu `Content-Length`/digest. |
| `InvalidBucketName` | 400 | `The specified bucket is not valid.` | Tên bucket vi phạm quy tắc S3 (`3..63` ký tự, IP-like, tiền tố/hậu tố cấm). |
| `KeyTooLongError` | 400 | `Your key is too long.` | Object key vượt `1024` UTF-8 bytes. |
| `InvalidArgument` | 400 | `Invalid argument.` | Sai header `Authorization`/presign/`Expires`/`X-Amz-Date`, `list-type != 2`, `partNumber` ngoài `1..10000`, key không phải UTF-8, `max-keys`/`continuation-token`/`encoding-type`/`fetch-owner`/`x-amz-copy-source` sai, gửi Bearer token vào S3. |
| `MalformedXML` | 400 | `The XML you provided was not well-formed or did not validate against our published schema.` | XML lỗi cú pháp, sai root element, trống/quá key ở `Delete`, thiếu/lặp `PartNumber`/`ETag` ở `Complete`. |
| `BadDigest` | 400 | `The Content-MD5 you specified did not match what we received.` | Sai `Content-MD5`, sai `x-amz-checksum-*` trailer/header, hoặc mismatch part/object checksum khi `Complete`. |
| `XAmzContentSHA256Mismatch` | 400 | `The provided 'x-amz-content-sha256' header does not match what was computed.` | Hash SHA-256 payload không khớp `x-amz-content-sha256`. |
| `IncompleteBody` | 400 | `You did not provide the number of bytes specified by the Content-Length HTTP header.` | Body thực tế ngắn/dài hơn `Content-Length` hoặc `x-amz-decoded-content-length`. |
| `InvalidPart` | 400 | `One or more of the specified parts could not be found.` | Part không tồn tại hoặc `ETag` không khớp part head khi `Complete`. |
| `InvalidPartOrder` | 400 | `The list of parts was not in ascending order.` | `PartNumber` không tăng nghiêm ngặt (hoặc thiếu `1..N` ở `COMPOSITE`). |
| `EntityTooSmall` | 400 | `Your proposed upload is smaller than the minimum allowed object size.` | Part không phải cuối `< 5 MiB` khi `Complete`. |
| `EntityTooLarge` | 400 | `Your proposed upload exceeds the maximum allowed object size.` | Single PUT/part `> 5 GiB`, tổng Complete `> 100 GiB`, `x-amz-meta-*` `> 2 KiB`, hoặc XML vượt giới hạn. |
| `PreconditionFailed` | 412 | `At least one of the preconditions you specified did not hold.` | `GetObject` với `If-Match` khác ETag hiện hành. |
| `InvalidRange` | 416 | `The requested range is not satisfiable.` | `Range: bytes=...` sai cú pháp, `start >= size`, multi-range, hoặc range trên object rỗng. |
| `NotImplemented` | 501 | `A header or query you provided implies functionality that is not implemented.` | Mọi tính năng P2 / ngoài phạm vi (bảng mục 10.4), `SigV2`, `Type=SELECT`, query lạ. |
| `InternalError` | 500 | `We encountered an internal error. Please try again.` | Lỗi nội bộ không phân loại trước header. |
| `SlowDown` | 503 | `Please reduce your request rate.` | Điều tiết tải (khi áp dụng). |
| `ServiceUnavailable` | 503 | `Service is unable to handle request.` | Node đang drain, backend DynamoDB/S3 unavailable, R1 busy/fenced, hoặc `clock_uncertainty_exceeded`. |

---

### 9.2 Admin API Error Schema (JSON — `internal/api/admin`)

Mọi response Admin mang `X-Request-Id: <32 lowercase hex>`. Mọi lỗi do handler Admin phát sinh dùng envelope JSON:

```json
{
  "error": {
    "code": "bucket_id_mismatch",
    "message": "expected_bucket_id is not the current bucket_id of this bucket",
    "request_id": "e19cb12e8ff5800cf06b05aa30c2192f"
  }
}
```

- Lỗi `401 unauthorized` kèm header `WWW-Authenticate: Bearer`.
- Route chưa đăng ký trên `ServeMux` (như `/admin/v1/nonexistent`) trả HTTP `404` trần của router **không có** JSON envelope; sai HTTP method trả `405` trần. `dsgctl` / `adminclient` ánh xạ cả hai thành `ErrUnsupported`.
- Thứ tự đánh giá lỗi (§6.5.1): `(1) Bearer 401` → `(2) Bucket {b} 404` → `(3) Role 403` → `(4) Query/JSON/Precondition schema 400` → `(5) Target resource 404` → `(6) expected_bucket_id 409` → `(7) Revision CAS 409`.

**Bảng đầy đủ mã lỗi JSON Admin thực tế:**

| `error.code` | HTTP | Route / điều kiện phát sinh |
|---|---|---|
| `invalid_request` | 400 | JSON sai/trùng key/`null`/unknown field (gồm elevation fields `owner`/`system_admin` ở grant hay `tiering`/`compression`/`dedup_domain` ở bucket policy), sai kiểu/regex/range, thiếu precondition (`expected_revision` / `If-Match`), query param lạ trên body route, `limit` ngoài `1..1000`, `debug_allowed` thiếu `system_admin`, `read=write=false` hoặc grant cho owner, `logical_limit < used+reserved`, `POST /faults` disabled / cluster scope / lệch `node_id`. |
| `invalid_cursor` | 400 | Cursor sai format, `kid` lạ, sai MAC, hết hạn 1 giờ, hoặc đổi principal/resource/filter/bucket_id. |
| `too_many_targets` | 400 | Cluster fan-out (`POST /jobs` / `POST /cache/purge`) vượt 64 active nodes. |
| `unauthorized` | 401 | Thiếu/sai format `Bearer`, sai secret, token `revoked`/hết hạn, hoặc user `disabled` (kèm `WWW-Authenticate: Bearer`). |
| `forbidden` | 403 | Token user gọi route `system_admin`, hoặc truy cập bucket/usage/token của user khác. |
| `bucket_not_found` | 404 | Bucket `{b}` hoặc `?bucket=` không tồn tại / đang deleting. |
| `user_not_found` | 404 | User không tồn tại (`GET`/`PATCH /users/{id}`, `/usage`, `PUT grant`, `POST credentials/admin-tokens`). |
| `grant_not_found` | 404 | `DELETE /buckets/{b}/grants/{user_id}` khi grant chưa có hoặc đã tombstone. |
| `credential_not_found` | 404 | `DELETE /credentials/{access_key_id}` không tồn tại. |
| `token_not_found` | 404 | `DELETE /admin-tokens/{token_id}` không tồn tại (khi caller là system admin). |
| `pack_not_found` | 404 | `GET /packs/{id}` hoặc `POST /packs/{id}/{gen}/restore\|promote` không có pack/generation. |
| `job_not_found` | 404 | `GET`/`DELETE /jobs/{id}` không tồn tại. |
| `node_not_found` | 404 | `GET /cache/stats?scope=node`, `POST /cache/purge` hoặc `POST /cluster/nodes/{id}/drain` không tìm thấy node. |
| `object_not_found` | 404 | `GET /objects/layout` key không tồn tại / đã bị xóa. |
| `fault_not_found` | 404 | `DELETE /faults/{id}` rule không tồn tại. |
| `user_exists` | 409 | `POST /users` trùng `user_id`. |
| `bucket_id_mismatch` | 409 | `expected_bucket_id` khác `bucket_id` hiện hành của bucket `{b}`. |
| `revision_conflict` | 409 | Revision hiện tại khác `expected_revision` / `If-Match`. |
| `revision_exhausted` | 409 | Cần ghi thay đổi khi revision đã đạt `2^53 - 1`. |
| `job_type_unavailable` | 501 | `POST /jobs`, `POST /packs/{id}/{gen}/restore\|promote`, `POST /cache/purge` khi executor tương ứng chưa được cài đặt ở runtime đang chạy (ví dụ thiếu audited typed genesis cho `gc`/`compaction`/`recovery_checkpoint`). |
| `unavailable` | 503 | Gateway đang drain, snapshot stats/observation chưa sẵn sàng/stale, `Operations`/`PackInventory`/`CacheStatistics` chưa gắn (mode `local`), hoặc metadata tạm không đọc được. |
| `internal_error` | 500 | Lỗi nội bộ không phân loại. |

### 9.3 Schema thực tế các route vận hành mới nhất (`faults`, `stats`, `layout`, `packs`, `jobs`, `cache`, `nodes`)

- **`POST /admin/v1/faults`** (chỉ khi `enable_fault_injection=true` và `node_id` khớp gateway nhận request):
  - Request: `{"type":"s3_5xx|ddb_throttle|kill_node|corrupt_pack","scope":"node","node_id":"<node_id>","rate":0.0..1.0,"duration_seconds":1..86400}`
  - Response `201`: `{"fault_id":"flt-...","type":"...","scope":"node","node_id":"...","rate":...,"duration_seconds":...,"created_at":"...","expires_at":"..."}`
- **`GET /admin/v1/faults`**: `200` trả JSON array `FaultRule[]` (`[]` nếu không có).
- **`DELETE /admin/v1/faults/{id}`**: `204 No Content`.
- **`GET /admin/v1/stats` & `GET /admin/v1/buckets/{b}/stats`**: `200` `stats.Summary` / `stats.BucketSummary` gồm `bytes_logical`, `bytes_unique_raw`, `bytes_unique_stored`, `bytes_stored`, `dedup_ratio`, `compression_ratio`, `total_reduction`, `objects`, `chunks`, `snapshot_id`, `as_of`, `collection_started_at`, `consistency`, `allocation_methodology`, `physical_version_scope`, `physical_complete`, `listed_bytes_by_category`, `unallocated_physical_bytes`, `temporary_restored_billed_bytes`, `write_counters_since`, `revived_chunks_total`, `revived_bytes_stored_total`, `orphaned_chunks_total`, `orphaned_bytes_stored_total`, `packs`.
- **`GET /admin/v1/objects/layout?bucket={b}&key={k}`**: `200` `{"integrity","bucket","domain_id","key","revision","size","chunk_occurrences","chunk_occurrences_shared_with_other_objects","packs":[...],"as_of","consistency"}`.
- **`GET /admin/v1/packs` & `GET /admin/v1/packs/{id}`**: `200` `{"items":[Pack...],"next_cursor":"..."}` với `Pack` gồm `pack_id`, `generation`, `state`, `storage_class`, `state_revision`, `bytes`, `live_bytes`, `live_ratio`, `protected_until`, `min_retention_until`, `restored_until`, `readable`, `as_of`, `consistency`.
- **`GET /admin/v1/cache/stats?scope=node&node_id=...` | `?scope=cluster`**: `200` `{"scope","complete","observed_at","nodes":[...],"included_nodes","hits","misses","denominator","hit_ratio"}`.
- **`GET /admin/v1/cluster/nodes`**: `200` `{"items":[Node...],"next_cursor":"..."}` với `Node` gồm `node_id`, `incarnation`, `service_incarnation`, `profile`, `address`, `ready`, `draining`, `drain_applied`, `drain_state`, `active_requests`, `applied_policy_revision`, `heartbeat_at`, `cache`.
- **`GET /admin/v1/cost/report`**: query tùy chọn `region`, `as_of`, `scenario` (`A|B`), `profile` (`cloud|edge`, `A` đi với `cloud`, `B` đi với `edge`), `from`, `to` (RFC 3339), `bucket`, `domain` (từ chối `400`), `workload` (`cloud-tiering-boundary|edge-revival|live_gateway_snapshot`), `policy`/`candidate_policy` (`warm_standard_only|warm_glacier_ir|warm_glacier_flexible`). Response `200` `CostReportResponse` (`report_type`, `schema_version`, `inputs`, `baseline`, `dsg_storage`, `dsg_requests_network`, `infrastructure`, `revival_copy`, `results`, `omissions`, `limitations`).

---

## 10. Checklist đối chiếu API Coverage (P0/P1, `dsgctl`, Health & Unsupported P2)

### 10.1 S3 API — P0 (8) và P1 (12) — 20/20 bắt buộc

| Ưu tiên | Operation | Method & Path / Trigger | Implementation (`Resolve` + Handler + `cmd/miles`) | Trạng thái |
|---|---|---|---|---|
| P0 | `ListBuckets` | `GET /` | `s3.Resolve` + `s3.Buckets.ListBuckets` + `cmd/miles` | **Implemented & Wired** |
| P0 | `CreateBucket` | `PUT /{bucket}` | `s3.Resolve` + `s3.Buckets.CreateBucket` + `cmd/miles` | **Implemented & Wired** |
| P0 | `HeadBucket` | `HEAD /{bucket}` | `s3.Resolve` + `s3.Buckets.HeadBucket` + `cmd/miles` | **Implemented & Wired** |
| P0 | `PutObject` | `PUT /{bucket}/{key}` | `s3.Resolve` + `s3.Objects.PutObject` + `cmd/miles` | **Implemented & Wired** |
| P0 | `GetObject` (+ Range) | `GET /{bucket}/{key}` | `s3.Resolve` + `s3.Objects.GetObject` + `cmd/miles` | **Implemented & Wired** |
| P0 | `HeadObject` (+ Range) | `HEAD /{bucket}/{key}` | `s3.Resolve` + `s3.Objects.HeadObject` + `cmd/miles` | **Implemented & Wired** |
| P0 | `DeleteObject` | `DELETE /{bucket}/{key}` | `s3.Resolve` + `s3.Objects.DeleteObject` + `cmd/miles` | **Implemented & Wired** |
| P0 | `ListObjectsV2` | `GET /{bucket}?list-type=2` | `s3.Resolve` + `s3.Objects.ListObjectsV2` + `cmd/miles` | **Implemented & Wired** |
| P1 | `CreateMultipartUpload` | `POST /{bucket}/{key}?uploads` | `s3.Resolve` + `multipart.Handlers.Create` + `cmd/miles` | **Implemented & Wired** |
| P1 | `UploadPart` | `PUT /{bucket}/{key}?partNumber=N&uploadId=ID` | `s3.Resolve` + `multipart.Handlers.UploadPart` + `cmd/miles` | **Implemented & Wired** |
| P1 | `CompleteMultipartUpload` | `POST /{bucket}/{key}?uploadId=ID` | `s3.Resolve` + `multipart.Handlers.Complete` + `cmd/miles` | **Implemented & Wired** |
| P1 | `AbortMultipartUpload` | `DELETE /{bucket}/{key}?uploadId=ID` | `s3.Resolve` + `multipart.Handlers.Abort` + `cmd/miles` | **Implemented & Wired** |
| P1 | `ListParts` | `GET /{bucket}/{key}?uploadId=ID` | `s3.Resolve` + `multipart.Handlers.ListParts` + `cmd/miles` | **Implemented & Wired** |
| P1 | `DeleteObjects` | `POST /{bucket}?delete` | `s3.Resolve` + `s3.Objects.DeleteObjects` + `cmd/miles` | **Implemented & Wired** |
| P1 | `CopyObject` | `PUT /{bucket}/{key}` + `x-amz-copy-source` | `s3.Resolve` + `s3.Objects.CopyObject` + `cmd/miles` | **Implemented & Wired** (cùng & khác domain) |
| P1 | `RestoreObject` | `POST /{bucket}/{key}?restore` | `s3.Resolve` + `s3.Objects.RestoreObject` + `cmd/miles` | **Implemented & Wired** |
| P1 | `DeleteBucket` | `DELETE /{bucket}` | `s3.Resolve` + `s3.Buckets.DeleteBucket` + `cmd/miles` | **Implemented & Wired** |
| P1 | `GetBucketLocation` | `GET /{bucket}?location` | `s3.Resolve` + `s3.Buckets.GetBucketLocation` + `cmd/miles` | **Implemented & Wired** |
| P1 | Presigned GET/PUT | Query `X-Amz-*` (SigV4) | `s3.parseSigV4` + `s3.presignedHeaders` + `cmd/miles` | **Implemented & Wired** |
| P1 | `GetObject` + `If-Match` | `GET /{bucket}/{key}` + `If-Match` | `s3.Objects.GetObject` (`checkIfMatch` → 412) + `cmd/miles` | **Implemented & Wired** |

### 10.2 Admin API (`/admin/v1`) — 28 nhóm route trong API §6.2

| # | Method & Path | Quyền | Implementation (`internal/api/admin/server.go`) | Trạng thái |
|---|---|---|---|---|
| 1 | `POST /users`, `GET /users` | System admin | `createUser`, `listUsers` | **Implemented** |
| 2 | `GET /users/{id}`, `PATCH /users/{id}` | System admin | `getUser`, `patchUser` | **Implemented** |
| 3 | `GET /users/{id}/usage` | Self \| System admin | `usage` | **Implemented** |
| 4 | `GET /buckets/{b}` | Owner \| System admin | `getBucket` | **Implemented** |
| 5 | `GET /buckets/{b}/grants` | Owner \| System admin | `listGrants` | **Implemented** |
| 6 | `PUT /buckets/{b}/grants/{user_id}`, `DELETE ...` | Owner \| System admin | `putGrant`, `deleteGrant` | **Implemented** |
| 7 | `GET /policy`, `PUT /policy` | System admin | `getPolicy`, `putPolicy` | **Implemented** |
| 8 | `GET /buckets/{b}/policy`, `PUT /buckets/{b}/policy` | Owner \| System admin | `getBucketPolicy`, `putBucketPolicy` | **Implemented** (cache only) |
| 9 | `GET /stats` | System admin | `stats` | **Implemented** |
| 10 | `GET /buckets/{b}/stats` | System admin | `bucketStats` | **Implemented** |
| 11 | `GET /objects/layout` | System admin | `layout` | **Implemented** |
| 12 | `GET /packs` | System admin | `listPacks` | **Implemented** (local mode 503) |
| 13 | `GET /packs/{id}` | System admin | `showPack` | **Implemented** (local mode 503) |
| 14 | `POST /packs/{id}/{generation}/restore` | System admin | `restorePack` | **Implemented** (local mode 503) |
| 15 | `POST /packs/{id}/{generation}/promote` | System admin | `promotePack` | **Implemented** (local mode 503) |
| 16 | `POST /jobs`, `GET /jobs` | System admin | `createJob`, `listJobs` | **Implemented** (local mode 503; shared runtime đủ 12 loại khi có typed genesis) |
| 17 | `GET /jobs/{id}`, `DELETE /jobs/{id}` | System admin | `getJob`, `cancelJob` | **Implemented** (local mode 503) |
| 18 | `GET /cache/stats` | System admin | `cacheStats` | **Implemented** (local mode 503) |
| 19 | `POST /cache/purge` | System admin | `purgeCache` | **Implemented** (local mode 503) |
| 20 | `GET /cost/report` | System admin | `costReport` | **Implemented** |
| 21 | `POST /credentials`, `GET /credentials` | System admin | `createCredential`, `listCredentials` | **Implemented** |
| 22 | `DELETE /credentials/{access_key_id}` | System admin | `deleteCredential` | **Implemented** |
| 23 | `POST /admin-tokens`, `GET /admin-tokens` | System admin | `createToken`, `listTokens` | **Implemented** |
| 24 | `DELETE /admin-tokens/{token_id}` | Owner token \| System admin | `deleteToken` | **Implemented** |
| 25 | `GET /cluster/nodes` | System admin | `listNodes` | **Implemented** (local mode 503) |
| 26 | `POST /cluster/nodes/{id}/drain` | System admin | `drainNode` | **Implemented** (local mode 503) |
| 27 | `POST /faults`, `GET /faults` | System admin (`enable_fault_injection`) | `createFault`, `listFaults` | **Implemented** (`scope=node`) |
| 28 | `DELETE /faults/{id}` | System admin (`enable_fault_injection`) | `deleteFault` | **Implemented** |

### 10.3 Health / Metrics (`:9100`) và `dsgctl` CLI Coverage

**Health / Metrics (`internal/observability/http.go`):**
- `GET /healthz` → **Implemented** (`200`)
- `GET /readyz` → **Implemented** (`200` khi probe fresh `<5s` và không draining; `503` khi chưa sẵn sàng/stale/draining)
- `GET /metrics` → **Implemented** (Prometheus text `0.0.4`)

**`dsgctl` (`cmd/dsgctl`):**

| Lệnh CLI | Route / Backend | Trạng thái |
|---|---|---|
| `stats [--bucket b]` | `GET /admin/v1/stats` \| `/buckets/{b}/stats` | **Implemented** |
| `layout <bucket> <key>` | `GET /admin/v1/objects/layout?bucket=...&key=...` | **Implemented** |
| `user create\|ls\|show\|set`, `usage <user> [--detail]` | `/admin/v1/users...` | **Implemented** |
| `bucket show <bucket>`, `grant ls\|set\|rm <bucket> <user>` | `/admin/v1/buckets/{b}...` | **Implemented** |
| `policy get\|set`, `bucket-policy get\|set <bucket>` | `/admin/v1/policy`, `/buckets/{b}/policy` | **Implemented** |
| `credentials create\|ls\|rm` | `/admin/v1/credentials...` | **Implemented** |
| `admin-token create\|ls\|revoke` | `/admin/v1/admin-tokens...` | **Implemented** |
| `admin-token bootstrap [--local-data-path \| --ddb-table-prefix --region \| --runtime-config]` | Offline transaction (`BootstrapAdmin` / `ProvisionShared`) | **Implemented** (`--recover` trả lỗi explicit chưa hỗ trợ) |
| `lifecycle-upgrade-gate --ddb-table-prefix --region` | Offline read-only V1 inspection | **Implemented** |
| `pack ls\|show\|restore\|promote` | `/admin/v1/packs...` | **Implemented** |
| `cache stats\|purge --scope cluster \| --scope node --node <id>` | `/admin/v1/cache/...` | **Implemented** |
| `job ls\|show\|run\|cancel` | `/admin/v1/jobs...` | **Implemented** |
| `node ls\|drain` | `/admin/v1/cluster/nodes...` | **Implemented** |
| `fault ls\|add\|rm` | `/admin/v1/faults...` | **Implemented** (`scope=node`) |
| `cost report` | `GET /admin/v1/cost/report` | **Implemented** (`--region --as-of --scenario --profile --from --to --bucket --domain --workload`) |
| `rebuild-index` | Offline guarded library workflow (`internal/recovery`) | **Unsupported trong `dsgctl` (`ErrUnsupported`)**; xem [runbook](runbooks/lifecycle-recovery-operations.md) |
| `sim run` | Offline simulator binary riêng (`cmd/costsim`) | **Unsupported trong `dsgctl` (`ErrUnsupported`)**; dùng `bin/costsim` |

### 10.4 Danh sách P2 & Ngoài phạm vi bị từ chối tường minh (không bỏ qua âm thầm)

| Nhóm / Tính năng | Biểu hiện HTTP | HTTP Status | Error Code | Ghi chú |
|---|---|---|---|---|
| `ListObjects` v1 | `GET /{bucket}` (không `list-type=2`) | `501` | `NotImplemented` | P2 (`s3.Resolve`) |
| `ListMultipartUploads` | `GET /{bucket}?uploads` | `501` | `NotImplemented` | P2 (`s3.Resolve`) |
| `UploadPartCopy` | `PUT /{bucket}/{key}?partNumber&uploadId` + `x-amz-copy-source` | `501` | `NotImplemented` | P2 (`s3.Resolve`); thay bằng `CopyObject` ≤ 5 GiB hoặc tải về + multipart |
| `GetObjectAttributes` | `GET /{bucket}/{key}?attributes` | `501` | `NotImplemented` | P2 (`s3.Resolve`) |
| `GET`/`HEAD` với `?partNumber` | `GET\|HEAD /{bucket}/{key}?partNumber=N` | `501` | `NotImplemented` | P2 (`s3.Resolve`) |
| Response overrides presigned | `?response-content-type`, `response-content-disposition`, `response-cache-control`, `response-content-encoding`, `response-content-language`, `response-expires` | `501` | `NotImplemented` | P2 (`s3.Resolve`) |
| Conditionals còn lại | `If-Match` ngoài `GetObject` (`HEAD`/`PUT`/`DELETE`), `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since`, `x-amz-copy-source-if-*` | `501` | `NotImplemented` | P2 (`s3.checkHeaders`) |
| Đổi checksum khi `CopyObject` | `PUT` có `x-amz-copy-source` và `x-amz-checksum-algorithm` khác nguồn | `400` | `InvalidRequest` | P2 (`object.ErrCopyChecksumChange`) |
| Checksum algorithm ngoài matrix | `SHA512`, `XXHASH*`, `x-amz-checksum-md5`, hoặc sai cặp `FULL_OBJECT`/`COMPOSITE` | `400` | `InvalidRequest` | P2 (`s3.checkChecksum`) |
| `SigV2` | `Authorization: AWS ...` hoặc `?AWSAccessKeyId=...` | `501` | `NotImplemented` | Ngoài phạm vi (`s3.parseSigV4` / `Resolve`) |
| Browser `POST Object` | `POST /{bucket}` | `501` | `NotImplemented` | Ngoài phạm vi (`s3.Resolve`) |
| `RestoreObject` SELECT / Output | `<Type>SELECT</Type>`, `<SelectParameters>`, `<OutputLocation>` | `501` | `NotImplemented` | Ngoài phạm vi (`s3.Objects.RestoreObject`) |
| S3 Versioning, ACL, Bucket Policy, Lifecycle, Tagging, Object Lock, Select, SSE-C, Redirect, ExpectedBucketOwner, Append | `?versioning`/`?versions`/`?versionId`, `?acl`, `?policy`, `?lifecycle`, `?tagging`, `?object-lock`/`?legal-hold`/`?retention`, `?select`, `x-amz-server-side-encryption-customer-*`, `x-amz-acl` ngoài `private`/`bucket-owner-full-control`, `x-amz-tagging-directive: REPLACE`, `x-amz-expected-bucket-owner`, `x-amz-website-redirect-location`, `x-amz-write-offset-bytes` | `501` | `NotImplemented` | Ngoài phạm vi (`s3.Resolve` / `checkHeaders`) |
| Query debug parameter | `?x-dsg-debug=1` | `501` | `NotImplemented` | Chặn leo thang debug qua query (`s3.Resolve`) |
| `GLACIER` `DEEP_ARCHIVE` / `TIER` không hỗ trợ | `policy set` đặt `DEEP_ARCHIVE` hoặc tier lạ | `400` | `invalid_request` | Tùy chọn hệ thống chưa bật |
| `gRPC CacheService` (`:7000`) | Consistent-hashing ring | — | — | Tùy chọn hệ thống, chưa bật trong binary hiện hành |
