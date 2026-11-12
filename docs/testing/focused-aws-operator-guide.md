# Hướng dẫn tự chạy AWS để kiểm tra GET và shutdown

Tài liệu này hướng dẫn người vận hành chạy thử, giữ bằng chứng rồi bàn giao để
review. Scope là GET cancellation và shutdown khi Outbox bị throttling. Không
điều tra lại PUT 409, không chạy lại toàn bộ recovery/GC/alarm hay thay node.
Đối chiếu [kết quả lượt trước](focused-clock-shutdown-aws-validation.md),
[runbook đã kiểm chứng](edge-aws-runbook.md) và
[phần việc còn lại](../planning/clock-conflict-aws-diagnostics.md#remaining-work-after-the-focused-recheck).

## 1. Chạy từ đâu?

**Có: các lệnh repository chạy từ thư mục gốc của worktree đang chứa bản sửa.**
Không cần `cd` về repo chính. Các worktree dùng chung Git history nhưng có file
và branch riêng; build nhầm worktree là kiểm thử nhầm source.

| Nhãn trong hướng dẫn | Máy chạy | Thư mục |
|---|---|---|
| **OPERATOR** | Máy đang mở terminal/Codex | Gốc worktree; plan/log/secret nằm ngoài Git |
| **EDGE** | EC2 Edge qua SSH/SSM | `/home/ec2-user/miles-acceptance`; service tại `/opt/miles/current` |
| **CLIENT** | EC2 client riêng | `/home/ec2-user/miles-acceptance` |
| **CONSOLE** | Trình duyệt | AWS Console đúng account và role |

Cloud, Edge, dataset và chương trình gây tải đều chạy trên AWS. Máy Oracle chỉ
build/điều phối/thu evidence. Biến shell trên OPERATOR **không tự truyền** sang
EDGE/CLIENT; khai báo lại đầu vào ở mỗi máy theo mục tương ứng.

**OPERATOR — terminal điều phối, Bash:**

```bash
cd /home/ubuntu/miles-worktrees/clock-journal-reliability
export REPO="$PWD"
git branch --show-current
git status --short
git log -1 --oneline
```

Nếu chạy trên máy khác, thay đường dẫn bằng checkout chứa đúng commit. Không
`git pull`, đổi branch, sửa file hay deploy binary mới giữa một phép so sánh.

## 2. Phần nào đã có, phần nào phải chuẩn bị trước?

Repository có build release, Terraform foundation/fleet, installer, benchmark,
telemetry, Admin tunnel và bộ kiểm inventory/teardown. **Chưa có một lệnh trong
repo dựng trọn bộ môi trường thử nghiệm hai Cloud + Edge + client.** Các lượt
trước còn dùng overlay Terraform và script private để dựng Edge/client, PKI,
offline RPM, bootstrap và IAM tạm.

Thư mục private của lượt đã đóng trên máy hiện tại là:

```text
/home/ubuntu/.local/state/miles/clock-journal-reliability/focused-recheck-20261112T023551Z
```

Chỉ dùng nó để **đọc và đối chiếu implementation/evidence**. Không chạy lại
`apply-*.py`, `automatic-closeout.py`, `begin-final-cleanup.py` hoặc script purge
của lượt cũ: chúng có account, resource ID, đường dẫn, deadline và grant của lượt
đã kết thúc. Không copy cả thư mục vì có secret/state. Không tái dùng saved plan.

Trước khi bắt đầu lượt mới, người chuẩn bị hạ tầng cần bàn giao **gói private
mới** với các đầu vào dưới đây. Thiếu một mục thì chuẩn bị mục đó trước khi apply;
không tự suy một tên resource từ lượt cũ.

| Đầu vào của lượt mới | Nội dung cần có |
|---|---|
| `backend.hcl` | Đúng bucket/key/Region/profile state; giữ bootstrap ngoài workload |
| `$TF_ROOT` | Bản cấu hình Terraform đã freeze, gồm overlay Edge/client đã review |
| `foundation.tfvars.json` | Chưa bật Cloud ASG; đúng account, boundary, AMI, network |
| `bootstrap-hosts.tfvars.json` | Thêm hai máy private Edge/client và đường HTTPS Edge |
| `active.tfvars.json` | Fleet hai Cloud, ACM, release SHA/VersionId; native caps đúng |
| Host/bootstrap bundle | RPM đã kiểm chữ ký, signing helper, startup JSON, TLS và Roles Anywhere riêng, lệnh cài từng máy |
| IAM activation/rollback | Policy và expiry mới, baseline, quyền workload/audit/cleanup đã kiểm |
| `ssh-config` | Alias `edge`, `client`, `cloud-a`, `cloud-b`; host keys pin từ kênh AWS tin cậy |
| Cleanup bundle | Cấu hình cleanup của chính graph này, inventory cả resource ngoài Terraform, lệnh restore IAM |
| `approval-review.md` | Số VM/đĩa, giá, thời gian, quyền tạm, dữ liệu được xóa và ngoại lệ giữ lại |

Các tên file trên là **hợp đồng bàn giao của gói chạy**, không phải file đã có
sẵn trong clone. Lệnh ở các mục 3–5 nhận gói này làm đầu vào. Nếu muốn chỉ từ
clone mà tự provision mọi thứ, cần đưa phần provisioning private vào repo thành
một thay đổi riêng; tài liệu này không giả vờ rằng phần đó đã có.

Để chốt nguyên nhân shutdown, bản sửa mới còn cần **log phân loại lỗi từng flush
attempt**, bên cạnh stage. Bản `376d130` chỉ có stage; chạy nguyên bản vẫn kiểm
được hành vi nhưng có thể chưa đủ RCA. Chuẩn bị logging trước, commit/test rồi
mới build. GET cần proxy byte/request-ID log ở mục 6. Không nới clock guard,
Outbox cap hay timeout để tạo PASS.

## 3. Tạo thư mục lượt chạy và đăng nhập

**OPERATOR:**

```bash
umask 077
export RUN_ID="focused-$(date -u +%Y%m%dT%H%M%SZ)"
export RUN="$HOME/.local/state/miles/manual-aws/$RUN_ID"
mkdir -m 700 -p "$RUN"
export AWS_REGION=ap-southeast-1
export AWS_DEFAULT_REGION="$AWS_REGION"
export AWS_PAGER=""
export DEPLOY_PROFILE=miles-dev
export IAM_PROFILE=miles-iam-bootstrap
```

Giữ terminal này mở; terminal mới cần khai báo lại các biến. Đặt Account ID và
deploy role ARN thật trong file đầu vào riêng, không copy account từ tài liệu.

```bash
export AWS_ACCOUNT_ID='REPLACE_WITH_ACCOUNT_ID'
export DEPLOY_ROLE_ARN='REPLACE_WITH_DEPLOY_ROLE_ARN'
export TF_ROOT="$RUN/source/infra/terraform/cloud/environments/dev"
export TF_DATA_DIR="$RUN/terraform-data"
export TF_PROFILE=miles-dev-terraform
```

### Đăng nhập bằng trình duyệt và authorization code

Đây là thao tác do người vận hành chủ động chạy. Nếu nhờ agent tạo phiên, báo
agent tạo link mới rồi điền code như trước.

**OPERATOR — terminal riêng, không ghi transcript khi nhập code:**

```bash
aws --version
aws login --profile miles-dev --remote
```

1. CONSOLE: đăng nhập IAM user bằng MFA. Trên account hiện tại là `hung-deploy`.
2. Switch back rồi Switch role mới sang deploy role; account hiện tại hiển thị
   `MilesDevDeploy`. Account khác dùng user/role của account đó.
3. Bấm URL CLI vừa in trong terminal, chọn danh tính **deploy role**.
4. Lấy authorization code, dán vào prompt CLI đang chờ và Enter. Không dán code
   vào chat, `.env`, Git hoặc run log. Code/link cũ không dùng lại.
5. Kiểm tra danh tính:

```bash
aws sts get-caller-identity --profile "$DEPLOY_PROFILE" \
  --region "$AWS_REGION" > "$RUN/deploy-identity.json"
AWS_PROFILE="$DEPLOY_PROFILE" python3 infra/terraform/cloud/scripts/check-deploy-identity.py \
  --role-arn "$DEPLOY_ROLE_ARN"
```

Nếu cần phiên quản trị IAM, mở CONSOLE riêng và login bằng profile
`miles-iam-bootstrap` theo cùng cách. Với account hiện tại, root chỉ IAM/audit;
mọi plan/apply/workload dùng deploy role. Phiên đăng nhập không tự cấp thêm quyền.

AWS access credentials có thể xoay vòng khoảng 15 phút. Không yêu cầu login mới
chỉ vì một lần export sắp hết hạn; chỉ xử lý reauth khi lệnh thật báo hết phiên.

### Profile Terraform

**OPERATOR:**

```bash
aws_cli="$(command -v aws)"
aws configure set credential_process \
  "$aws_cli configure export-credentials --profile $DEPLOY_PROFILE --format process --region $AWS_REGION" \
  --profile "$TF_PROFILE"
aws configure set region "$AWS_REGION" --profile "$TF_PROFILE"
export AWS_PROFILE="$TF_PROFILE"
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
```

Backend HCL dùng cùng `$TF_PROFILE`; provider không assume thêm role lần nữa.
Chi tiết bootstrap account mới ở
[operator IAM](../../infra/terraform/cloud/bootstrap/operator/README.md).

## 4. Freeze source, release và baseline

**OPERATOR — worktree phải sạch và mọi sửa logging đã commit:**

```bash
cd "$REPO"
test -z "$(git status --porcelain)" || { echo 'Worktree chưa sạch'; false; }
git rev-parse HEAD > "$RUN/source-commit.txt"
python3 -B deploy/build-release.py --arch arm64 \
  --out "$RUN/miles-linux-arm64.tar.gz" > "$RUN/release.json"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
  -o "$RUN/warmbench" ./cmd/warmbench
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
  -o "$RUN/datasetgen" ./cmd/datasetgen
go build -o "$RUN/dsgctl" ./cmd/dsgctl
sha256sum "$RUN/miles-linux-arm64.tar.gz" "$RUN/warmbench" \
  "$RUN/datasetgen" > "$RUN/build-sha256.txt"
```

Release chỉ đóng gói `miles`/`dsgctl`, nên phải build hai công cụ benchmark riêng.
Kiểm file hash sau khi chuyển sang host. Không thay release sau khi bắt đầu tải.

Trước tạo auditor hay bất kỳ resource thử mới, dùng danh tính audit đã được cấp
quyền đọc để chụp baseline. `$BASELINE_PROFILE` là profile audit/root của gói mới,
không mặc định deploy role có quyền đọc đủ account:

```bash
export BASELINE_PROFILE="$IAM_PROFILE"
python3 -B scripts/capture_volume_baseline.py \
  --profile "$BASELINE_PROFILE" --region "$AWS_REGION" \
  --account-id "$AWS_ACCOUNT_ID" --run-id "$RUN_ID" \
  --output "$RUN/volume-baseline.json" > "$RUN/volume-baseline.sha256"
python3 -B scripts/capture_account_inventory.py \
  --profile "$BASELINE_PROFILE" --region "$AWS_REGION" \
  --account-id "$AWS_ACCOUNT_ID" --run-id "$RUN_ID" \
  --iam-role "${DEPLOY_ROLE_ARN##*/}" \
  --output "$RUN/account-baseline.json" > "$RUN/account-baseline.sha256"
```

Gói IAM phải liệt kê thêm role baseline khác cần đối chiếu bằng `--iam-role`.
Các file baseline chỉ tạo một lần, không ghi đè sau khi đã provision.

## 5. Dựng AWS theo ba stage

Gói mới cần review lại plan/IAM/chi phí/cleanup trước bước tính phí. Lượt trước
được duyệt tối đa 10 USD và bốn giờ; đó là **số tham chiếu đã hết hiệu lực**.
T0 là lần tạo tài nguyên tính phí đầu tiên. Ghi deadline mới trước apply và để
ít nhất 50 phút cuối cho cleanup. Dừng tải mới khi không còn đủ thời gian.

**OPERATOR — sau khi gói mới đã chuẩn bị, nhập đúng backend và stack:**

```bash
test -f "$TF_ROOT/main.tf"
test -f "$RUN/backend.hcl"
terraform -chdir="$TF_ROOT" init -input=false -reconfigure \
  -lockfile=readonly -backend-config="$RUN/backend.hcl"
AWS_PROFILE="$DEPLOY_PROFILE" python3 infra/terraform/cloud/scripts/check-deploy-identity.py \
  --role-arn "$DEPLOY_ROLE_ARN"
terraform -chdir="$TF_ROOT" plan -input=false -lock-timeout=60s \
  -var-file="$RUN/foundation.tfvars.json" -out="$RUN/foundation.tfplan"
terraform -chdir="$TF_ROOT" show -no-color "$RUN/foundation.tfplan" \
  > "$RUN/foundation-plan.txt"
terraform -chdir="$TF_ROOT" show -json "$RUN/foundation.tfplan" \
  > "$RUN/foundation-plan.json"
```

Review account, actions và ownership rồi mới chạy apply saved plan đó:

```bash
terraform -chdir="$TF_ROOT" apply -input=false -lock-timeout=60s \
  "$RUN/foundation.tfplan"
terraform -chdir="$TF_ROOT" output -json > "$RUN/foundation-outputs.json"
```

Lặp đúng `plan → show → review → identity check → apply` với:

| Stage | Var file | Việc phải hoàn tất trước stage kế |
|---|---|---|
| Foundation | `foundation.tfvars.json` | 20 bảng, S3 versioning, network, IAM/boundary; Cloud chưa chạy |
| Hosts | `bootstrap-hosts.tfvars.json` | Edge/client private qua overlay, SSM, host-key pin, RPM và TLS; bootstrap installation khi chưa có gateway chạy |
| Active | `active.tfvars.json` | Installation bootstrap xác nhận; Edge dùng Roles Anywhere; release S3 key/VersionId/SHA đã kiểm; ACM ISSUED |

Các tên saved plan tương ứng là `bootstrap-hosts.tfplan` và `active.tfplan`,
không ghi đè `foundation.tfplan`. Cài/bootstrap host theo **lệnh trong gói mới**,
đối chiếu [shared runtime](../development/shared-runtime.md),
[Cloud](../../deploy/cloud/README.md) và [Edge](../../deploy/edge/README.md).
Bootstrap chỉ một lần trên backend rỗng; outcome không rõ phải reconcile trước
khi retry. Không reset dữ liệu để vượt lỗi.

Sau Active, ghi `terraform output -json` vào `$RUN/active-outputs.json`. Gói bàn
giao phải cung cấp các biến đã đối chiếu với outputs/API:

```text
EDGE_INSTANCE_ID, CLIENT_INSTANCE_ID, CLOUD_ASG, TARGET_GROUP_ARN, VPC_ID,
TABLE_PREFIX, DATA_BUCKET, EDGE_ENDPOINT, CA_BUNDLE, SYSTEM_TOKEN, AUDIT_ROLE,
KMS_KEY_ID, SECRET_ARN
```

Không dùng query đoán vị trí `Reservations[0]` để chọn máy. Với Cloud có nhiều
incarnation, chọn node hiện tại Ready và không Draining. Cloud owner SSH chỉ
forward Admin, không có shell/sudo.

**OPERATOR — kiểm hạ tầng trước tải:**

```bash
aws ssm describe-instance-information --profile "$DEPLOY_PROFILE" \
  --region "$AWS_REGION" > "$RUN/ssm-nodes.json"
aws elbv2 describe-target-health --profile "$DEPLOY_PROFILE" \
  --region "$AWS_REGION" --target-group-arn "$TARGET_GROUP_ARN" \
  > "$RUN/cloud-target-health.json"
aws dynamodb describe-table --profile "$DEPLOY_PROFILE" \
  --region "$AWS_REGION" --table-name "${TABLE_PREFIX}Outbox" \
  --query 'Table.{status:TableStatus,cap:OnDemandThroughput}' \
  > "$RUN/outbox-cap.json"
```

Đủ hai Cloud healthy, Edge ready, cùng history/backend và source. Giữ Chunks read
512, Outbox/đọc bảng khác 128, tất cả write cap 128 nếu đang so với lượt trước.
Đọc lại native sau bootstrap và apply: bootstrap từng đặt cap về 10 trong lượt
cũ. Không dùng `ItemCount` từ DescribeTable làm bằng chứng số event hiện tại.

## 6. Bật logging và telemetry trước request

**EDGE — vào máy bằng SSH config mới của lượt:**

```bash
# OPERATOR:
ssh -F "$RUN/ssh-config" edge

# EDGE:
umask 077
mkdir -p "$HOME/miles-acceptance"
cd "$HOME/miles-acceptance"
sudo /opt/miles/current/bin/miles -version
sudo systemctl show miles.service -p InvocationID -p ActiveState -p MainPID
curl --fail --silent --show-error http://127.0.0.1:9100/readyz
sudo chronyc -n tracking
sudo chronyc -n ntpdata
sudo python3.12 /opt/miles/current/scripts/edge_host_telemetry.py probe \
  > telemetry-probe.json
```

Trước mỗi trial, chọn `TRIAL` riêng như `get-c1-01`, không ghi đè artifact cũ:

```bash
# EDGE, terminal giám sát giữ mở:
export TRIAL=get-c1-01
sudo systemd-run --unit="miles-telemetry-$TRIAL" \
  --property=TimeoutStopSec=15 \
  python3.12 /opt/miles/current/scripts/edge_host_telemetry.py run \
  --output-file="/run/miles/$TRIAL-telemetry.jsonl" --duration=1800
sudo systemctl is-active "miles-telemetry-$TRIAL"
sudo wc -l "/run/miles/$TRIAL-telemetry.jsonl"
```

Đợi vài giây, chạy lại `wc -l` và kiểm JSON có mẫu Chrony/OS hợp lệ tăng lên,
không chỉ process còn sống. Chỉ dispatch sau probe và growing samples đạt.
Nếu cần quá 30 phút, tạo window mới trước tải; không để recorder tự hết giữa bài.

### Proxy byte log cho GET

**EDGE:** thêm `log_format` trong `http {}` và `access_log` trong đúng server Edge.
Thay `access_log off` đang có ở server đó; không đổi route/Host/buffering/timeout.
Lưu cấu hình trước/sau, chạy `nginx -t` bằng đúng binary/config mà proxy unit dùng:

```nginx
log_format miles_trace escape=json
  '{"time":"$time_iso8601","request_id":"$upstream_http_x_amz_request_id",'
  '"status":$status,"body_bytes":$body_bytes_sent,'
  '"upstream_bytes":"$upstream_response_length",'
  '"request_seconds":$request_time,"upstream_seconds":"$upstream_response_time"}';

# Bên trong server Edge hiện có:
access_log /var/log/nginx/miles-trace.jsonl miles_trace;
```

Không ghi URI/query, Authorization, token hay body. Log có thể chỉ xuất hiện khi
kết nối đã hoàn tất. Gói host phải ghi đúng unit/config path; không mặc định unit
tên `nginx` vì lượt trước dùng proxy unit riêng. Reload bằng lệnh của unit đó sau
khi config test thành công. Cần log request ID/stage và lỗi flush attempt đã được
phân loại ở gateway; không dump raw backend messages/keys để bù thiếu log.

## 7. Kịch bản GET: một C1 rồi C4 nếu cần

Mục tiêu là ghép client result với gateway request ID và proxy bytes. Không tự
lặp thêm hai vòng clock. C1/C4 là concurrency 1/4; chạy C4 sau khi C1 đạt và còn
đủ thời gian. Mỗi trial dùng user/domain/bucket riêng. Không bật retry client.

### Tạo identity và chuyển công cụ

**OPERATOR — terminal tunnel giữ mở:**

```bash
cd "$REPO"
python3 -B scripts/admin_tunnel.py --ssh-config "$RUN/ssh-config" \
  --target edge --token-file "$SYSTEM_TOKEN" --user-id operator \
  --local-port 19001
```

**OPERATOR — terminal khác, khai báo lại `$RUN`/`$REPO` và các biến:**

```bash
export TRIAL=get-c1-01
export TEST_USER="get-$(date -u +%H%M%S)-c1"
"$RUN/dsgctl" --endpoint http://127.0.0.1:19001 --token-file "$SYSTEM_TOKEN" \
  user create "$TEST_USER" --default-domain "$TEST_USER" --logical-limit 2147483648
"$RUN/dsgctl" --endpoint http://127.0.0.1:19001 --token-file "$SYSTEM_TOKEN" \
  credentials create --user "$TEST_USER" --out "$RUN/$TRIAL-secret.txt" \
  > "$RUN/$TRIAL-credential-metadata.json"
export TRIAL
python3 - <<'PYCREDS'
import json, os
from pathlib import Path
r, t = Path(os.environ['RUN']), os.environ['TRIAL']
metadata = json.loads((r / (t + '-credential-metadata.json')).read_text())
keys = {'access_key_id': metadata['access_key_id'],
        'secret_access_key': (r / (t + '-secret.txt')).read_text().strip()}
with (r / (t + '-credentials.json')).open('x') as out:
    json.dump(keys, out)
PYCREDS
scp -F "$RUN/ssh-config" "$RUN/warmbench" "$RUN/datasetgen" \
  "$RUN/$TRIAL-credentials.json" client:/home/ec2-user/miles-acceptance/
```

`dsgctl --out` lưu **chuỗi secret**, không lưu nguyên response JSON. Đoạn Python
ở trên ghép access-key ID và secret thành đúng schema benchmark, không in key.
Thư mục đích phải tạo trước trên CLIENT; CA bundle cũng chuyển từ gói mới.
Credentials ở đây là **Miles S3 client key**, không phải AWS deploy credentials.

### Dataset và config

**CLIENT — đăng nhập bằng `ssh -F "$RUN/ssh-config" client` từ OPERATOR:**

```bash
umask 077
cd "$HOME/miles-acceptance"
chmod 700 warmbench datasetgen
# Chỉ sinh một lần; thư mục dataset phải chưa tồn tại, dùng chính user benchmark.
./datasetgen --profile backup30 --seed focused-get --out "$PWD/dataset"
sha256sum dataset/manifest.json > dataset-manifest.sha256
```

Gói `backup30` dùng cho so sánh với lượt trước; kiểm dung lượng disk và manifest
trước khi chạy. Build/hash phải khớp OPERATOR. Điền config riêng, file 0600:

```json
{
  "target": "miles-warm",
  "endpoint": "https://REPLACE_WITH_EDGE_HOST",
  "region": "ap-southeast-1",
  "bucket": "REPLACE_WITH_TEST_USER",
  "credentials_file": "/home/ec2-user/miles-acceptance/get-c1-01-credentials.json",
  "kms_key_id": "",
  "environment": {
    "profile": "edge",
    "topology": "two Cloud, Edge and client in private AWS VPC",
    "host": "t4g.small CPU Standard",
    "cpu": "Graviton2, 2 vCPU",
    "disk": "20 GiB encrypted gp3",
    "network": "private Singapore VPC; client to Edge HTTPS",
    "metadata": "dynamodb",
    "gateway_commit": "REPLACE_WITH_SOURCE_COMMIT",
    "policy_revision": "1",
    "encryption": "HTTPS and backend SSE-KMS",
    "cache_state": "fresh domain",
    "isolation": "fresh synthetic user/domain/bucket for this trial",
    "async_work": "shared journal and workers enabled"
  }
}
```

Lưu thành `get-c1-01-config.json`; cấu hình trust CA/DNS trên CLIENT từ gói mới.
Không dùng `--no-verify-ssl` hoặc `curl -k` để vượt TLS. Khai báo CLIENT:

```bash
export TRIAL=get-c1-01
export CA_BUNDLE="$PWD/ca-bundle.pem"
export AWS_CA_BUNDLE="$CA_BUNDLE"
export SSL_CERT_FILE="$CA_BUNDLE"
chmod 600 "$TRIAL-config.json" "$TRIAL-credentials.json"
```

Tạo bucket qua **Miles endpoint** bằng Miles credentials. Không nhầm với bucket
S3 vật lý của backend; không chạy `aws s3api create-bucket` mặc định vào AWS.
Đoạn dưới chỉ giữ key trong subprocess RAM, không in ra terminal:

```bash
python3 - <<'PY'
import json, os, subprocess
from pathlib import Path
t = os.environ['TRIAL']
c = json.loads(Path(t + '-config.json').read_text())
keys = json.loads(Path(c['credentials_file']).read_text())
env = {k: v for k, v in os.environ.items() if not k.startswith('AWS_')}
env.update(AWS_ACCESS_KEY_ID=keys['access_key_id'],
           AWS_SECRET_ACCESS_KEY=keys['secret_access_key'],
           AWS_CONFIG_FILE='/dev/null', AWS_SHARED_CREDENTIALS_FILE='/dev/null',
           AWS_EC2_METADATA_DISABLED='true', AWS_MAX_ATTEMPTS='1',
           AWS_CA_BUNDLE=os.environ['CA_BUNDLE'], AWS_PAGER='')
p = subprocess.run(['aws', '--region', c['region'], '--endpoint-url', c['endpoint'],
                    's3api', 'create-bucket', '--bucket', c['bucket']],
                   env=env, capture_output=True, timeout=60)
Path(t + '-create-bucket.out').write_bytes(p.stdout)
Path(t + '-create-bucket.err').write_bytes(p.stderr)
raise SystemExit(p.returncode)
PY
```

Nếu thất bại, đọc outcome trước khi retry. Không gọi Admin bucket-policy GET/PUT
trước PUT đầu tiên trên bucket mới; GET policy cũng có thể materialize default.

### Chạy workload và kết thúc window

**CLIENT — recorder EDGE đã chạy trước đó:**

```bash
date -u +%FT%T.%NZ > "$TRIAL-start.txt"
timeout --signal=TERM --kill-after=125s 1200s \
  ./warmbench --config "$TRIAL-config.json" --manifest dataset/manifest.json \
  --out "$PWD/$TRIAL" --concurrency 1 --read-repeats 3 \
  --fail-fast --drain-timeout 120s --retain-objects \
  > "$TRIAL-stdout.log" 2> "$TRIAL-stderr.log"
trial_exit=$?
printf '%s\n' "$trial_exit" > "$TRIAL-exit.txt"
```

Không chạy tiếp khi exit khác 0 hoặc result báo failed/interrupted. Lưu result
và samples dù bài lỗi. Với dataset này, một trial hoàn chỉnh có 90 PUT, 90 HEAD,
360 GET đúng SHA. `--retain-objects` giữ dữ liệu phục vụ bài shutdown, phải dọn sau.
Chạy C4 bằng identity/config/output/recorder window mới và `--concurrency 4`;
không đổi `--out` thành thư mục đã tồn tại.

**EDGE — sau response cuối cùng, lấy `started_at`/`finished_at` từ result:**

```bash
export TRIAL=get-c1-01
export SINCE='REPLACE_WITH_RESULT_STARTED_AT'
export UNTIL='REPLACE_WITH_RESULT_FINISHED_AT'
sudo systemctl stop "miles-telemetry-$TRIAL"
sudo systemctl show "miles-telemetry-$TRIAL" \
  -p Result -p ExecMainStatus > "$TRIAL-recorder-status.txt"
sudo python3.12 /opt/miles/current/scripts/edge_host_telemetry.py summarize \
  --input-file "/run/miles/$TRIAL-telemetry.jsonl" \
  --since "$SINCE" --until "$UNTIL" > "$TRIAL-telemetry-summary.json"
sudo journalctl -u miles.service --since "$SINCE" --until "$UNTIL" \
  -o json --no-pager > "$TRIAL-gateway.jsonl"
sudo tar -czf "$TRIAL-chrony.tar.gz" -C /var/log/chrony .
sudo cp /var/log/nginx/miles-trace.jsonl "$TRIAL-proxy.jsonl"
```

File tạo bởi sudo thuộc root; dùng sudo/scp qua kênh được duyệt hoặc xuất về
OPERATOR bằng `ssh ... 'sudo cat ...' > "$RUN/..."`. Không chmod secret/log thành
world-readable để copy. Giữ raw telemetry ngoài `/run` trước reboot/terminate.

PASS client và PASS clock là hai verdict riêng. GET đủ bytes/SHA nhưng log
`canceled` vẫn giữ để đối chiếu request ID, status, body/upstream bytes và thời
điểm; không tự thêm retry hay coi mất dữ liệu. Recorder exit 1 phải đọc summary,
không tự kết luận chronyd crash.

## 8. Kịch bản shutdown không gây tải đọc

Chạy trên installation disposable, ngừng client mutations và workload khác trước.
Không chạy pulse ở bài này. Cần inventory/pending snapshot trước và sau ở gói
evidence để biết có bao nhiêu việc phải flush; shutdown lúc không có pending chỉ
là clean-stop smoke, không chứng minh chịu tải.

**EDGE — ghi invocation trước khi stop:**

```bash
cd "$HOME/miles-acceptance"
sudo systemctl show miles.service -p InvocationID -p MainPID -p ActiveState \
  > before-stop.txt
invocation="$(sudo systemctl show miles.service -p InvocationID --value)"
test -n "$invocation"
printf '%s\n' "$invocation" > stopped-invocation.txt
date -u +%FT%T.%NZ > stop-requested-at.txt
sudo systemctl stop --no-block miles.service
# Xem tiến độ; Ctrl-C chỉ dừng journal follow, không dừng shutdown.
sudo journalctl -f "_SYSTEMD_INVOCATION_ID=$invocation" -o short-iso-precise
```

Sau khi process đã dừng:

```bash
sudo systemctl show miles.service -p ActiveState -p SubState -p Result \
  -p ExecMainCode -p ExecMainStatus -p MainPID > after-stop.txt
sudo journalctl "_SYSTEMD_INVOCATION_ID=$invocation" -o json --no-pager \
  > stopped-invocation.jsonl
```

PASS cần process dừng, exit 0 và `shutdown complete` thuộc **chính invocation
vừa ghi**, cùng dữ liệu đã reconcile. `systemctl stop --no-block` trả 0 chỉ là
nhận yêu cầu, không phải shutdown thành công. `failed`/exit 1 hoặc thiếu proof
phải ghi FAIL, giữ dữ liệu/log trước cleanup. Không restart để xóa dấu lỗi.

## 9. Kịch bản shutdown cùng native Outbox throttling

Đây là phép thử riêng, cần native-capacity load được chốt trong gói mới. Bài
`ddb_throttle` giả lập không thay thế nó. Giữ bảng Outbox read cap 128 và cùng
binary/profile. Không đổi từ 8 lên 10 phút trong phép so sánh này.

Để so với lỗi trước cần **cả historical epoch và current epoch có events**.
Lượt trước có historical 1.242 events và current 405 events. Chỉ vài PUT trên
installation rỗng không tương đương. Ghi số producer/event/progress trước bài.
Cho writer tạo tải bằng benchmark, chờ cut tự nhiên nếu cần rồi tạo current
events; không sửa gate, clock hoặc metadata trực tiếp để ép cut. Thời gian đợi
cut phải tính vào deadline. Nếu chưa có history phù hợp, ghi rõ bài nhỏ hơn thay
vì tuyên bố tái hiện đủ điều kiện.

Trước pulse: dừng dispatch/mutations, snapshot metadata khi chưa gây tải, ghi
invocation Edge. Các observer full Scan cũng tiêu tốn read cap; không chạy Scan
thu snapshot song song rồi coi đó là điều kiện nền không có tải đọc.

**CLIENT — dùng AWS profile ngắn hạn được cấp quyền Scan đúng Outbox, không dùng
Miles client key và không dùng root.** Gói mới cung cấp `$CLIENT_READ_PROFILE`,
`$AWS_REGION`, `$TABLE_PREFIX`; kiểm identity và cap trước:

```bash
cd "$HOME/miles-acceptance"
aws sts get-caller-identity --profile "$CLIENT_READ_PROFILE" --region "$AWS_REGION"
export OUTBOX_TABLE="${TABLE_PREFIX}Outbox"
aws dynamodb describe-table --profile "$CLIENT_READ_PROFILE" \
  --region "$AWS_REGION" --table-name "$OUTBOX_TABLE" \
  --query 'Table.OnDemandThroughput'
```

Đồng bộ với EDGE: người vận hành gửi stop như mục 8 rồi chạy pulse ở CLIENT,
ghi UTC của cả hai. Đây là lệnh gây tải chỉ đọc 120 giây, bốn worker, page limit
1.024 item/AWS tối đa 1 MiB, không SDK retry. Log để riêng trên máy test:

```bash
mkdir -m 700 pulse
export CLIENT_READ_PROFILE AWS_REGION OUTBOX_TABLE
date -u +%FT%T.%NZ > pulse/started-at.txt
pids=()
for worker in 1 2 3 4; do
  timeout --signal=TERM --kill-after=5s 120s bash -c '
    err="$2/last-$1.err"
    while :; do
      AWS_MAX_ATTEMPTS=1 AWS_RETRY_MODE=standard aws dynamodb scan \
        --profile "$CLIENT_READ_PROFILE" --region "$AWS_REGION" \
        --table-name "$OUTBOX_TABLE" --consistent-read --limit 1024 \
        --projection-expression event_id --return-consumed-capacity TOTAL \
        --no-paginate --no-cli-pager --cli-connect-timeout 3 --cli-read-timeout 5 \
        --query "{count:Count,units:ConsumedCapacity.CapacityUnits}" --output json \
        2> "$err"
      code=$?
      cat "$err" >&2
      if [ "$code" -ne 0 ]; then
        grep -Eq "ThrottlingException|ProvisionedThroughputExceededException|RequestLimitExceeded" \
          "$err" || exit 1
        sleep 0.2
      fi
    done
  ' _ "$worker" "$PWD/pulse" \
    > "pulse/worker-$worker.out" 2> "pulse/worker-$worker.err" &
  pids+=("$!")
done
for pid in "${pids[@]}"; do
  wait "$pid"
  printf '%s %s\n' "$pid" "$?" >> pulse/worker-exits.txt
done
date -u +%FT%T.%NZ > pulse/finished-at.txt
grep -Ec 'ThrottlingException|ProvisionedThroughputExceededException|RequestLimitExceeded' \
  pulse/worker-*.err > pulse/throttle-response-counts.txt
```

Chạy block này trong Bash không bật `set -e`: `timeout` hết 120 giây trả 124 là
đúng kỳ vọng. Exit code từng worker nằm trong `pulse/worker-exits.txt`. Worker tự
dừng khi gặp lỗi khác throttle. Người giám sát dừng các worker còn lại bằng
`kill -TERM "${pids[@]}"` từ chính shell đang chạy khi thấy AccessDenied,
ExpiredToken hoặc sai resource; không dùng `pkill` chung. Không chạy thêm pulse
để vượt burst capacity nếu chưa còn đủ thời gian/quyền/chi phí.
Số đếm từ stderr là phản hồi CLI, cần đối
chiếu native CloudWatch; zero không chứng minh thử đúng điều kiện throttled.

**OPERATOR — đọc metric đúng window, sau khi chờ dữ liệu metric xuất hiện:**

```bash
aws cloudwatch get-metric-statistics --profile "$DEPLOY_PROFILE" \
  --region "$AWS_REGION" --namespace AWS/DynamoDB \
  --metric-name ReadMaxOnDemandThroughputThrottleEvents \
  --dimensions "Name=TableName,Value=${TABLE_PREFIX}Outbox" \
  --start-time "$SINCE" --end-time "$UNTIL" --period 60 --statistics Sum \
  > "$RUN/outbox-native-throttle-metrics.json"
```

Đặt `$SINCE`/`$UNTIL` theo shutdown/pulse thực tế, gồm vài phút sau pulse. AWS có
thể còn throttle sau khi load đã dừng. Không gán mọi metric của bảng cho đúng
một gateway/request. Thu exit/proof Edge như mục 8, log classified attempts,
snapshot trước/sau và kiểm payload SHA/receipts trước purge.

PASS cần có native throttle, shutdown sạch trong deadline hiện hành, progress
và cut/seal được kiểm đúng scope. Peer hoàn tất sau Edge timeout không làm Edge
PASS. Không có native throttle thì ghi **điều kiện chưa được exercise**. Hiện chưa
có CLI `VerifyCut` tổng quát trong repo; gói mới phải bàn giao verifier đã kiểm
source/hash nếu cần proof cut độc lập. Không dùng snapshot lease Released thay
cho proof shutdown hoặc S3 receipt.

## 10. Thu evidence và cleanup

**OPERATOR — trước terminate/purge:**

```bash
mkdir -p "$RUN/evidence/client" "$RUN/evidence/edge"
scp -r -F "$RUN/ssh-config" client:/home/ec2-user/miles-acceptance/get-c1-01 \
  "$RUN/evidence/client/"
ssh -F "$RUN/ssh-config" edge \
  'sudo journalctl -u miles.service -o json --no-pager' \
  > "$RUN/evidence/edge/gateway-all.jsonl"
ssh -F "$RUN/ssh-config" edge \
  'sudo tar -czf - -C /var/log/chrony .' \
  > "$RUN/evidence/edge/chrony.tar.gz"
ssh -F "$RUN/ssh-config" edge \
  'sudo cat /var/log/nginx/miles-trace.jsonl' \
  > "$RUN/evidence/edge/proxy.jsonl"
```

Thu thêm mọi trial result/samples/config đã che key, raw recorder JSONL, summary,
probe, invocation/status, pulse logs, metric readback, nguồn/config hashes và
metadata/payload reconciliation. File private chứa credentials không gửi vào PR.
Muốn tôi review thì bàn giao đường dẫn run directory cùng source và danh sách
gate PASS/FAIL; trên máy khác cần chuyển bộ evidence đã che secret.

### Freeze tài nguyên trước destroy

**OPERATOR — dùng audit role của lượt đã được bàn giao:**

```bash
terraform -chdir="$TF_ROOT" state pull > "$RUN/pre-destroy.tfstate"
terraform -chdir="$TF_ROOT" show -json > "$RUN/pre-destroy-state.json"
terraform -chdir="$TF_ROOT" output -json > "$RUN/pre-destroy-outputs.json"
python3 -B scripts/freeze_terraform_inventory.py \
  --state-json "$RUN/pre-destroy-state.json" --run-id "$RUN_ID" \
  --account-id "$AWS_ACCOUNT_ID" --region "$AWS_REGION" \
  --inventory-json "$RUN/verifier-inventory.json" \
  --output "$RUN/frozen-inventory.json"
```

`verifier-inventory.json` là mapping tài nguyên cho verifier do gói mới bàn giao,
bao gồm ACM/SLR ngoài Terraform; không phải file `frozen-inventory.json` vừa xuất
(danh sách state resources có schema khác). Phải reconcile mọi VM/đĩa cũ và mới.
Sau đó bind hashes, deploy/audit role và ngoại lệ đã được duyệt:

```bash
export AUDIT_PROFILE='REPLACE_WITH_RUN_AUDITOR_PROFILE'
export VOLUME_SHA256="$(sha256sum "$RUN/volume-baseline.json" | cut -d ' ' -f 1)"
python3 -B scripts/capture_runtime_inventory.py \
  --profile "$AUDIT_PROFILE" --region "$AWS_REGION" \
  --account-id "$AWS_ACCOUNT_ID" --run-id "$RUN_ID" --vpc-id "$VPC_ID" \
  --volume-baseline "$RUN/volume-baseline.json" --volume-baseline-sha256 "$VOLUME_SHA256" \
  --asg "$CLOUD_ASG" --instance "$EDGE_INSTANCE_ID" --instance "$CLIENT_INSTANCE_ID" \
  --output "$RUN/pre-destroy-runtime.json"
export STATE_SHA256="$(sha256sum "$RUN/pre-destroy-state.json" | cut -d ' ' -f 1)"
export RUNTIME_SHA256="$(sha256sum "$RUN/pre-destroy-runtime.json" | cut -d ' ' -f 1)"
export ACCOUNT_SHA256="$(sha256sum "$RUN/account-baseline.json" | cut -d ' ' -f 1)"
python3 -B scripts/prepare_teardown_manifest.py \
  --state-json "$RUN/pre-destroy-state.json" --state-sha256 "$STATE_SHA256" \
  --inventory-json "$RUN/verifier-inventory.json" \
  --runtime-json "$RUN/pre-destroy-runtime.json" --runtime-sha256 "$RUNTIME_SHA256" \
  --account-baseline "$RUN/account-baseline.json" --account-baseline-sha256 "$ACCOUNT_SHA256" \
  --run-id "$RUN_ID" --account-id "$AWS_ACCOUNT_ID" --region "$AWS_REGION" \
  --deploy-role "${DEPLOY_ROLE_ARN##*/}" --audit-role "$AUDIT_ROLE" \
  --approve-pending-deletion "kms_key=$KMS_KEY_ID" \
  --approve-pending-deletion "secret=$SECRET_ARN" \
  --output "$RUN/frozen-manifest.json"
```

Hai `--approve-pending-deletion` chỉ dùng khi đúng hai ID mới đó đã được duyệt;
không thay bằng wildcard hoặc key/secret baseline. Chụp runtime inventory khi ASG
và node còn hiện đầy đủ, trước scale-to-zero/terminate; giữ cả ID máy đã thay.
Không coi Terraform state rỗng là đã dọn hết account.

Trình tự cleanup đã kiểm chứng:

1. Ngừng mutations; ghi invocation, scale ASG min/desired về 0 theo kế hoạch
   cleanup, Edge SIGTERM. Thu proof trước khi máy mất. Chờ controller/drain.
2. Chỉ khi runtime đã dừng mới xuất snapshot cuối, reconcile outcome và purge
   synthetic S3 versions/delete markers/multipart đã được duyệt.
3. Dùng **bản cleanup riêng của chính stack có overlay**, review plan gỡ bảo vệ
   và saved destroy plan. Không `terraform destroy` ở root bootstrap hoặc dùng
   copy thiếu module Edge/client. Chi tiết ở
   [full teardown](../../infra/terraform/cloud/full-teardown.md).
4. Xóa certificate/resource ngoài state thuộc lượt, khôi phục IAM baseline,
   gỡ grants/auditor và role do lượt tạo. KMS/secret mới chờ xóa 30 ngày theo duyệt.
5. Chạy strict final audit. Giữ session audit đến khi audit accepted, sau đó
   logout và dừng đúng tunnel/process của lượt.

Lệnh cuối, sau khi gói mới đã tạo đủ frozen inputs và hashes:

```bash
python3 -B scripts/verify_teardown_absence.py --final-audit \
  --profile "$IAM_PROFILE" --region "$AWS_REGION" \
  --manifest "$RUN/frozen-manifest.json" \
  --state-json "$RUN/pre-destroy-state.json" --state-sha256 "$STATE_SHA256" \
  --runtime-json "$RUN/pre-destroy-runtime.json" --runtime-sha256 "$RUNTIME_SHA256" \
  --volume-baseline "$RUN/volume-baseline.json" \
  --account-baseline "$RUN/account-baseline.json" \
  --output-ledger "$RUN/final-absence-summary.json"
```

Chỉ hoàn tất khi `accepted=true`, không có unapproved residual/error và không
thiếu resource baseline. `PASS_WITH_PENDING` chỉ hợp lệ với đúng key/secret mới
đã duyệt và bind trong manifest. `UNVERIFIED` không phải PASS. Không lấy số 117/88
của lượt trước làm số bắt buộc cho graph mới.

```bash
aws logout --profile "$DEPLOY_PROFILE"
aws logout --profile "$IAM_PROFILE"
```

Xóa code/URL login và temporary credentials thuộc lượt, gỡ riêng auditor profile,
dừng đúng tunnel đã ghi PID. Giữ evidence private; không xóa cache AWS hoặc kill
mọi process của người khác. Đối soát chi phí khi billing cập nhật, không coi dự
toán là hóa đơn cuối hoặc hard cap.

## 11. Lỗi thường gặp

| Hiện tượng | Kiểm tra và cách xử lý |
|---|---|
| URL login 400 | Dùng URL đầy đủ vừa tạo, đúng browser session. Nếu link chat bị biến dạng, dùng dòng URL trực tiếp từ CLI. Tạo phiên/link mới khi phiên chờ đã hết; không lặp code cũ. |
| Login xong vẫn hết phiên | Switch back, đăng nhập lại IAM user/MFA nếu cần rồi Switch role mới; chọn đúng role trên trang cấp quyền. Kiểm STS. |
| Đăng nhập nhầm user | ARN là `user/...` thay vì assumed deploy role: dừng trước plan/apply, lấy code role mới. |
| AccessDenied | Ghi action/resource, kiểm IAM + boundary + VPC endpoint policy và expiry; không retry liên tục hoặc dùng root chạy workload. |
| Hết vCPU/loại máy không cho launch | Kiểm quota/AMI/architecture/Free Tier restrictions; review thay sizing trước apply, không tự thay instance type giữa phép đo. |
| CLI dùng được, Terraform không | Kiểm backend/provider đều dùng profile `credential_process`, không còn env credentials cũ, không assume role lần hai. |
| ASG thay node liên tục | Kiểm release key **và** VersionId/SHA, secret/backend/bootstrap, clock, readiness và target port. Không sửa readyz thành 200. |
| SSM/RPM/DNS timeout | Kiểm private routes, SG/endpoints và policy. Endpoints không thay Internet package repo; dùng RPM bundle đã ký. Không mở Admin/metrics ra Internet để chữa. |
| Dataset permission denied | Sinh bằng `ec2-user` chạy benchmark; không chmod toàn dataset world-readable. Hash lại manifest sau xử lý ownership. |
| Benchmark config/output bị từ chối | JSON phải đúng schema, secret file 0600, đủ khai báo environment; `--out` phải là thư mục mới. |
| TLS/SigV4 lỗi | Kiểm CA, hostname/SAN, DNS và giờ; giữ Host/raw URI qua proxy. Không bỏ verify TLS, rewrite URL hoặc retry PUT mù. |
| R1 gate Closing trước trial | Đợi cut tự nhiên/recovery, giữ deadline; không ép gate Open. Nếu chờ vượt ngân sách thì cleanup. |
| Recorder exit 1/UNVERIFIED | Xem sampling errors, khoảng giữa sample, cuối window, reference age và native Chrony logs. Tách lỗi thiếu evidence với clock stale thật. |
| GET client PASS nhưng server canceled | Ghép request ID với proxy status/bytes/upstream bytes và samples; giữ kết quả hai phía. Không tự kết luận mất dữ liệu. |
| Pulse không có throttle | Xác nhận đủ history/current, cap 128 và thời gian/identity đúng. Ghi chưa exercise; không tự kéo dài/tăng tải ngoài gói duyệt. |
| Shutdown hết 8 phút | Giữ FAIL, invocation journal, attempt errors và pending snapshot. Không nâng timeout rồi coi nguyên nhân đã sửa. |
| ALB drain mất vài phút | Bình thường nếu cấu hình deregistration 300s. App flush/ASG lifecycle là đồng hồ riêng, giảm ALB delay không sửa flush timeout. |
| Destroy bị protection/state lock | Đối chiếu ownership và bản cleanup đã review; chờ đúng process giữ lock. Không force-unlock/force-delete để bỏ qua phiên khác. |
| ELB SLR báo Resources in use sau xóa ALB | Lưu failed task/reason, xác nhận native ALB absent, retry bounded đúng role do lượt tạo. Role không tính phí; không xóa role baseline. |
| Audit timeout/UNVERIFIED | Giữ receipt lỗi, kiểm phiên/quyền/network rồi chạy lại audit read-only. Không sửa verifier để bỏ qua resource. |

## 12. Nhật ký bàn giao

Sau mỗi bước ghi một dòng trong `$RUN/run-log.md`:

```text
UTC | bước | source/config/command đã che secret | PASS/FAIL/UNVERIFIED |
artifact | bước tiếp theo hoặc cleanup
```

Bàn giao tối thiểu: source/release/profile hashes; topology/caps; kết quả mỗi
trial và exact UTC window; request IDs bất thường; shutdown invocation/exit và
progress; pulse/metric; manifest/audit cuối; IAM rollback; chi phí và tài nguyên
được giữ. Một trial retry thành công không xóa kết quả thất bại trước đó.
