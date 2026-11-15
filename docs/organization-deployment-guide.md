# Hướng dẫn tự dựng Miles cho một tổ chức (Cloud / Local / Hybrid)

Tài liệu này là điểm bắt đầu dành cho một tổ chức muốn tự dựng hệ thống **Miles** theo đúng nhu cầu hạ tầng của mình: **Cloud** (100% trên AWS), **Local** (1 node nội bộ không dùng AWS), hoặc **Hybrid** (Edge tại hạ tầng tổ chức/VM nội bộ nối về backend AWS, hoặc kết hợp Cloud cluster + Edge dùng chung backend).

Mọi mô hình đều dùng chung lõi binary `miles` và CLI quản trị `dsgctl`; khác nhau ở **backend metadata/pack**, **mạng**, **TLS**, và **cách cung cấp AWS credentials**.

---

## 1. Chọn mô hình triển khai phù hợp

| Mô hình | Thành phần vận hành | Metadata & Lưu trữ vật lý | Phù hợp khi | Giới hạn / Lưu ý chính |
|---|---|---|---|---|
| **Local** (`mode: "local"`) | 1 process `miles` trên máy Linux nội bộ | BadgerDB trên đĩa local + Moto S3 tạm thời qua loopback | Đánh giá tính năng, kiểm thử API S3/Admin bằng dữ liệu thử mà không cần AWS account | 1 node, bind loopback; S3 ghi bằng user `dev`; pack mất khi Moto khởi động lại hoặc bị xóa |
| **Cloud** (`profile: "cloud"`) | ALB HTTPS + ASG 2 EC2 Gateway (mỗi node có RAM/disk cache riêng) | S3 PackStore + 20 bảng DynamoDB + KMS + Secrets Manager trong VPC | Tổ chức muốn đặt toàn bộ gateway và dữ liệu trên AWS, có HA đa AZ | Cần AWS account, quyền IAM/VPC/EC2/ALB/S3/DDB/KMS/Secrets Manager và `chrony` |
| **Hybrid (AWS-managed test)** (`deployment.profile: "both"`) | Cụm Cloud (ALB + 2 EC2) + 1 EC2 Edge trên AWS dùng chung backend | Chung 1 S3 PackStore + 20 bảng DynamoDB trên AWS | Kiểm thử cross-profile (Cloud ↔ Edge) hoặc pilot nhanh toàn bộ trên AWS | Edge EC2 vẫn nằm trên AWS, chưa phản ánh WAN thực tế từ on-prem |
| **Hybrid (On-Prem Edge + AWS Backend)** | Edge Gateway (`miles` + Nginx TLS + `chrony`) trên server/VM nội bộ của tổ chức, có thể chạy cùng hoặc không chạy Cloud cluster | Chung 1 S3 PackStore + 20 bảng DynamoDB + KMS + Secrets Manager trên AWS; Edge xác thực bằng IAM Roles Anywhere | Tổ chức đặt gateway gần người dùng/dữ liệu tại văn phòng hoặc datacenter riêng, lưu pack dài hạn trên AWS | Cần chuẩn bị host Linux systemd, Nginx TLS, `aws_signing_helper`, NTP IPv4 tin cậy và kết nối HTTPS tới AWS |

> **Nguyên tắc chọn nhanh:**
> - Chưa có AWS account / muốn thử nghiệm nhanh: bắt đầu bằng **Local (Mục 3)**.
> - Muốn dựng nhanh bằng 1 file JSON cho tổ chức trên AWS (dùng CA nội bộ cho thử nghiệm/pilot): dùng `deploy-installation.py` trong **Mục 4.1** hoặc **Mục 5.1**.
> - Muốn vận hành dài hạn với domain thật, ACM certificate công khai, ALB nội bộ (`alb_internal = true`), hoặc Edge đặt ngoài AWS: dùng **Terraform trực tiếp** kết hợp **Mục 4.2** / **Mục 5.2**.

---

## 2. Chuẩn bị chung (áp dụng cho mọi mô hình)

### 2.1 Công cụ cần có trên máy điều phối / build

- **Chung:** Git, GNU Make, Go **1.27.2** (theo `go.mod`), Python **3.12+**.
- **Nếu chạy Local:** Docker Engine + Docker Compose, AWS CLI v2 (để thao tác client/fixture).
- **Nếu chạy Cloud hoặc Hybrid trên AWS:** AWS CLI v2 (`>= 2.32.0`), Session Manager plugin, Terraform **1.15.2**, OpenSSL, OpenSSH (`ssh`, `ssh-keygen`), và `chronyc` đang đồng bộ với một nguồn NTP IPv4 tin cậy.

Clone source ở commit sạch và kiểm tra build nền:

```bash
umask 077
export MILES_WORK="$HOME/miles-org-setup"
export MILES_REPO="$MILES_WORK/repo"
export MILES_PRIVATE="$MILES_WORK/private"
mkdir -p "$MILES_PRIVATE"
chmod 700 "$MILES_PRIVATE"

git clone https://github.com/KinasTomes/miles.git "$MILES_REPO"
cd "$MILES_REPO"
make check
make build
./bin/miles -version
./bin/dsgctl -help
```

### 2.2 Quy tắc bảo mật và tách biệt 3 miền danh tính

Mọi triển khai Miles tách cứng 3 loại credential — **không bao giờ dùng lẫn**:

1. **AWS IAM Backend Credential:** Chỉ dùng nội bộ để `miles` gọi S3 PackStore, DynamoDB, KMS, Secrets Manager (Cloud dùng EC2 IMDSv2; Edge dùng IAM Roles Anywhere `credential_process`; Local dùng dummy `mileslocal`).
2. **Miles S3 SigV4 Key:** Cấp cho client/ứng dụng của tổ chức qua `dsgctl credentials create`. Dùng để gọi S3 API (`:8080` / `:443`).
3. **Miles Admin Bearer Token:** Bootstrap lần đầu khi gateway dừng (`dsgctl admin-token bootstrap`), sau đó cấp qua `dsgctl admin-token create`. Lưu trong file regular mode `0600` và chỉ gọi qua loopback/SSH tunnel (`:9000`).

> Không đặt private directory, `.env`, key, token, chứng chỉ CA, hoặc Terraform state vào trong thư mục checkout Git.

---

## 3. Phương án Local (1 node nội bộ, không cần AWS)

### 3.1 Thử nhanh tự động (`make test-local`)

Nếu chỉ muốn xác minh nhanh binary và các luồng S3 cơ bản trên máy hiện tại:

```bash
cd "$MILES_REPO"
make test-local
```

Lệnh này tự bật container tạm, chạy smoke test rồi tự xóa toàn bộ container/dữ liệu tạm.

### 3.2 Dựng sandbox Local để thử tương tác

Chỉ dùng dữ liệu có thể bỏ. Badger giữ metadata trên đĩa, còn dịch vụ Moto trong Compose này không có lưu trữ bền vững. Khởi động lại hoặc tạo lại Moto có thể làm mất các pack trong khi Badger vẫn giữ metadata. Chế độ Local dùng đồng hồ giả lập và không kiểm chứng R1/R2 phân tán. Xem [local-runtime.md](development/local-runtime.md).

1. **Khởi động S3 fixture (Moto) bằng Docker Compose và lấy port loopback:**

   ```bash
   cd "$MILES_REPO"
   docker compose -f deploy/local/compose.yaml -p miles-org-local up -d s3
   MOTO_ADDR="$(docker compose -f deploy/local/compose.yaml -p miles-org-local port s3 5000)"
   echo "Moto S3 endpoint: http://${MOTO_ADDR}"
   ```

2. **Tạo physical pack bucket trên Moto (tách biệt khỏi tên bucket của client):**

   ```bash
   env -u AWS_SESSION_TOKEN -u AWS_SECURITY_TOKEN \
     AWS_ACCESS_KEY_ID=mileslocal AWS_SECRET_ACCESS_KEY=mileslocal AWS_EC2_METADATA_DISABLED=true \
     aws --endpoint-url "http://${MOTO_ADDR}" --region ap-southeast-1 s3 mb s3://miles-local-packs
   ```

3. **Tạo thư mục dữ liệu và file cấu hình riêng ngoài Git (từ [local.example.json](../configs/local.example.json)):**

   ```bash
   mkdir -p "$MILES_PRIVATE/local-data"
   chmod 700 "$MILES_PRIVATE/local-data"
   cat > "$MILES_PRIVATE/local.json" <<EOF
   {
     "profile": "edge",
     "mode": "local",
     "health_address": "127.0.0.1:9100",
     "s3_address": "127.0.0.1:8080",
     "admin_address": "127.0.0.1:9000",
     "region": "ap-southeast-1",
     "local_data_path": "$MILES_PRIVATE/local-data",
     "pack_endpoint": "http://${MOTO_ADDR}",
     "pack_bucket": "miles-local-packs",
     "shutdown_seconds": 1800
   }
   EOF
   chmod 600 "$MILES_PRIVATE/local.json"
   ```

   *Lưu ý theo `internal/config/startup.go`: `mode: "local"` bắt buộc bind địa chỉ loopback (`127.0.0.1`), `pack_endpoint` phải là HTTP loopback có port tường minh, và không dùng đồng thời 2 process `miles` trên cùng `local_data_path`.*

4. **Bootstrap Admin token lần đầu (chạy trước khi start `miles`):**

   ```bash
   ./bin/dsgctl admin-token bootstrap \
     --user ops --ttl 24h \
     --token-file "$MILES_PRIVATE/local-admin-token" \
     --local-data-path "$MILES_PRIVATE/local-data"
   ```

5. **Khởi động gateway và kiểm tra:**

   ```bash
   ./bin/miles -config "$MILES_PRIVATE/local.json"
   ```

   Ở terminal khác:

   ```bash
   curl -fsS http://127.0.0.1:9100/healthz
   curl -fsS http://127.0.0.1:9100/readyz
   ```

   Miles ghi credential S3 client vào file riêng `$MILES_PRIVATE/local-data/credentials.json`. Dùng `access_key_id` và `secret_access_key` từ file để cấu hình profile AWS CLI riêng cho `--endpoint-url http://127.0.0.1:8080` (addressing style `path`). Dùng `DSGCTL_ENDPOINT=http://127.0.0.1:9000` và `DSGCTL_TOKEN_FILE="$MILES_PRIVATE/local-admin-token"` cho `dsgctl`.

6. **Dọn dẹp khi ngừng dùng:**

   Dừng `miles` bằng `Ctrl+C`/`SIGTERM` và đợi process thoát. Lệnh dưới đây **xóa toàn bộ pack thử trong Moto**, nhưng giữ thư mục Badger. Lượt thử sạch tiếp theo cần thư mục `local_data_path` và file Admin token mới, cùng bucket vật lý được tạo lại.

   ```bash
   docker compose -f deploy/local/compose.yaml -p miles-org-local down --volumes
   ```

---

## 4. Phương án Cloud (100% trên AWS)

Tổ chức có hai đường dựng Cloud tùy theo việc triển khai **thử nghiệm/pilot nhanh (có thời hạn)** hay **vận hành dài hạn với domain/ACM thật**.

### 4.1 Dựng nhanh bằng bộ điều phối một-file (`deploy-installation.py`)

Phù hợp để một tổ chức dựng trọn gói VPC, S3, 20 bảng DynamoDB, KMS, Secrets Manager, ALB HTTPS, ASG 2 EC2 Gateway và CloudWatch dashboard.

#### Bước 1: Chọn chế độ IAM & State Backend (`bootstrap_mode`)

- **`"new"`**: Dùng khi tổ chức tạo mới từ đầu trên AWS account (tạo IAM operator user, MFA deploy role, runtime permissions boundary và S3 Terraform state bucket riêng). Cần phiên admin/root để chạy 2 stack `operator` và `state`.
- **`"existing"`**: Dùng khi đội Security/Platform của tổ chức cấp sẵn IAM deploy role (trust operator + MFA), runtime permissions boundary và S3 state bucket (bật versioning, SSE-S3, public access block, bucket policy deny HTTP, explicit deny `s3:DeleteObject`/`s3:DeleteObjectVersion` trên state key). Chi tiết yêu cầu tại [aws-personal-account-guide.md](testing/aws-personal-account-guide.md#11-dùng-iam-và-backend-đã-cấp-sẵn-trên-máy-member).

#### Bước 2: Điền file cấu hình installation riêng cho tổ chức

Copy mẫu từ [cloud-demo.example.json](../infra/terraform/cloud/environments/dev/cloud-demo.example.json) ra `$MILES_PRIVATE/installation.json`:

```bash
cp infra/terraform/cloud/environments/dev/cloud-demo.example.json \
  "$MILES_PRIVATE/installation.json"
chmod 600 "$MILES_PRIVATE/installation.json"
```

Các trường bắt buộc thay theo thông tin thật của tổ chức:

| Nhóm | Trường | Giá trị cần điền |
|---|---|---|
| Định danh tổ chức | `account_id`, `region`, `availability_zones` | AWS Account ID 12 số, commercial Region (VD `ap-southeast-1`), 2 AZ khác nhau |
| Namespace | `installation_name`, `environment`, `owner` | Tên installation riêng, 3–24 ký tự, bắt đầu bằng chữ thường và kết thúc bằng chữ thường hoặc số. Điền tag môi trường và chủ sở hữu theo quy ước của tổ chức |
| IAM & Bootstrap | `bootstrap_mode`, `runtime_iam_path`, `runtime_permissions_boundary_arn`, `operator.*` | Chọn `new` hoặc `existing`; điền đúng ARN user, MFA, deploy role, `state_bucket`/`state_key` (nếu `existing`) |
| Compute | `ami_id`, `release_architecture`, `instance_type`, `fleet_instance_type` | AMI Amazon Linux 2023 đã pin đúng Region/arch (`arm64` hoặc `amd64`), instance type được account cho phép |
| Mạng & Truy cập | `client_cidrs`, `egress_mode` | `client_cidrs`: IP public/NAT của tổ chức dạng `/32` hoặc CIDR; giữ `egress_mode: "interface"` |
| Profile & Giám sát | `deployment.profile`, `enable_dashboard` | Đặt `"profile": "cloud"`, `"enable_dashboard": true` nếu muốn dựng CloudWatch dashboard |
| Đồng hồ & Giới hạn | `operator.utc_reference`, `deployment.max_cost_usd`, `deployment.max_active_minutes` | IPv4 nguồn `chrony` đang sync trên máy operator (kiểm bằng `chronyc -n -c -h 127.0.0.1 tracking`); đặt ngân sách và thời lượng phiên |

#### Bước 3: Chạy quy trình dựng Cloud

```bash
deploy() {
  python3 -B "$MILES_REPO/infra/terraform/cloud/scripts/deploy-installation.py" \
    --config "$MILES_PRIVATE/installation.json" \
    --private "$MILES_PRIVATE" "$@"
}

deploy prepare
```

- **Nếu `bootstrap_mode: "new"`:**
  1. Đăng nhập AWS bằng `operator.admin_profile` (`aws login --profile <admin_profile> --region <region> --remote`).
  2. `deploy plan operator` → review `$MILES_PRIVATE/operator-plan-details.json` và `rendered/iam/*` → `deploy apply operator --plan-sha256 <SHA>`.
  3. Đổi password Console lần đầu và enroll MFA đúng tên `operator.mfa_arn`, sau đó Switch role sang `operator.deploy_role_arn` và đăng nhập `operator.login_profile` (`aws login --profile <login_profile> --region <region> --remote`).
  4. `deploy plan state` → review → `deploy apply state --plan-sha256 <SHA>`.
- **Nếu `bootstrap_mode: "existing"`:**
  1. Đăng nhập `operator.login_profile` bằng deploy role được cấp.
  2. Nếu bật `enable_dashboard: true`, nhờ chủ account review và gắn thêm inline policy từ `$MILES_PRIVATE/rendered/iam/deploy-dashboard.json` vào deploy role.
  3. Chạy `deploy verify-bootstrap`.

Sau khi hoàn tất bootstrap (chung cho cả `new` và `existing`):

```bash
deploy plan foundation
# Review $MILES_PRIVATE/foundation-plan-details.json
deploy apply foundation --plan-sha256 REPLACE_WITH_FOUNDATION_SHA256

deploy provision

deploy plan active
# Review $MILES_PRIVATE/active-plan-details.json
deploy apply active --plan-sha256 REPLACE_WITH_ACTIVE_SHA256

deploy verify
```

Chi tiết từng bước kiểm tra, lấy URL CloudWatch dashboard và curl/AWS CLI demo xem tại [cloud-demo-deployment.md](planning/cloud-demo-deployment.md).

### 4.2 Điều chỉnh để vận hành Cloud dài hạn cho tổ chức (Terraform trực tiếp)

Dùng cùng Terraform stack với Mục 4.1 khi cần domain/TLS của tổ chức hoặc Edge đặt ngoài AWS. Mục này dùng chung cho Cloud và Hybrid on-prem. Thực hiện theo thứ tự **IAM/state → foundation → provision → bật gateway**.

#### 4.2.1 Chuẩn bị IAM và backend Terraform

1. Tạo JSON riêng theo Mục 4.1. Chọn `installation_name` riêng trong account, chọn Region đích và điền IAM path, role, boundary, bucket/key state và thư mục private của installation đó. Nếu dùng chung bucket state, cấp key và quyền lock riêng cho từng installation. Giữ nguyên các định danh khi cập nhật một installation đã có dữ liệu.
2. Chạy `deploy prepare` từ checkout sạch đã commit. Với `new`, làm các bước đăng nhập, `plan/apply operator` và `plan/apply state` của Mục 4.1. Với `existing`, chủ account cấp IAM/backend trước và operator chạy `verify-bootstrap` trên key dành riêng cho installation mới. Xem [hướng dẫn đăng nhập bằng link và code](testing/aws-personal-account-guide.md#2-đăng-nhập-quản-trị-iam-bằng-link-và-code).
3. Từ đây quản lý workload bằng Terraform trực tiếp, dùng `rendered/backend.hcl` và một bản copy riêng của `rendered/installation.tfvars.json`. Dùng cùng backend, workspace `default` và bộ inputs đã đối chiếu ở cả foundation lẫn active. Tránh chạy xen các stage workload của `deploy-installation.py` vì bộ dựng có state điều phối riêng.

Terraform backend và provider cần profile phụ khớp `operator.terraform_profile` trong AWS config trên máy điều phối. Profile phụ gọi CLI để đọc phiên `operator.login_profile` hiện hành. Ví dụ, thay cả tên profile và Region theo JSON:

```ini
[profile miles-org-terraform]
region = ap-southeast-1
credential_process = aws configure export-credentials --profile miles-org-deploy --format process --region ap-southeast-1
```

Dùng một shell riêng cho installation, bỏ các AWS credential và Terraform override kế thừa. Đặt `AWS_PROFILE` về profile Terraform, `TF_WORKSPACE=default` và `TF_DATA_DIR="$MILES_PRIVATE/terraform-data"`. Trước mỗi lần apply, chạy [check-deploy-identity.py](../infra/terraform/cloud/scripts/check-deploy-identity.py) với `--role-arn` là ARN trong `operator.deploy_role_arn`. Giữ biến Terraform `deploy_role_arn = null` vì CLI đã dùng deploy role.

#### 4.2.2 Apply foundation khi gateway đang tắt

Lưu bản copy inputs thành `$MILES_PRIVATE/foundation.tfvars.json`. Giữ `paused = true` và các cờ `enable_compute`, `enable_asg`, `enable_alb`, `enable_edge`, `enable_acceptance_nodes` đều `false`. Bỏ các release pin và `certificate_arn` chưa có. Nếu sẽ cài Edge on-prem, điền public CA và certificate CN như Mục 5.2 **trước khi lập plan**.

```bash
miles_tf() {
  terraform -chdir="$MILES_REPO/infra/terraform/cloud/environments/dev" "$@"
}

miles_tf init -input=false -reconfigure -lockfile=readonly \
  -backend-config="$MILES_PRIVATE/rendered/backend.hcl"
miles_tf plan -input=false -var-file="$MILES_PRIVATE/foundation.tfvars.json" \
  -out="$MILES_PRIVATE/foundation.tfplan"
miles_tf show "$MILES_PRIVATE/foundation.tfplan"
```

Sau khi đối chiếu danh tính deploy, scope IAM và chi phí, apply đúng saved plan bằng `miles_tf apply "$MILES_PRIVATE/foundation.tfplan"`. Kiểm plan tạo backend dữ liệu, KMS, secret metadata và network nền, chưa có serving VM hay ALB. Lấy bucket, KMS key và secret ARN từ `miles_tf output -json runtime_inventory`.

#### 4.2.3 Provision rồi mới bật gateway

1. Tạo secret runtime và bootstrap metadata một lần lúc chưa có gateway chạy, theo Mục 6.1. Với Hybrid chỉ có Edge on-prem, giữ AWS foundation ở trạng thái này rồi sang bước cài host tại Mục 5.2.
2. Nếu có Cloud, điền `MILES_ARCH` (`amd64` hoặc `arm64`) khớp AMI và fleet, cùng `MILES_REGION`, `MILES_DATA_BUCKET` và `MILES_KMS_KEY_ARN` theo foundation. Từ repo root, build từ commit sạch và upload bằng deploy role:

   ```bash
   release="$MILES_PRIVATE/miles-linux-${MILES_ARCH:?Điền kiến trúc}.tar.gz"
   python3 -B deploy/build-release.py --arch "$MILES_ARCH" --out "$release" &&
     release_sha="$(sha256sum "$release" | cut -d ' ' -f 1)" &&
     aws --region "${MILES_REGION:?Điền Region}" s3api put-object \
       --bucket "${MILES_DATA_BUCKET:?Điền bucket data}" \
       --key "releases/$release_sha/${release##*/}" --body "$release" \
       --if-none-match '*' --server-side-encryption aws:kms \
       --ssekms-key-id "${MILES_KMS_KEY_ARN:?Điền KMS key ARN}"
   ```

   Dùng `release_sha` và `VersionId` trả về để pin release. Bucket phải bật versioning và `VersionId` không được là `null`. Nếu upload báo object đã tồn tại, kiểm tra nội dung/version của bản đã upload trước khi dùng pin đó.
3. Copy inputs foundation thành `active.tfvars.json`. Dùng [mẫu Cloud fleet](../infra/terraform/cloud/environments/dev/cloud-fleet.tfvars.example) để điền AMI, loại máy, kiến trúc, `release_sha256` và `release_version_id`. Điền `certificate_arn` của certificate ACM `ISSUED` cho domain của tổ chức, cùng account và Region. Bật `enable_asg` và `enable_alb`, đặt `paused = false`, giữ `enable_compute = false` và `enable_edge = false`. Giữ cấu hình Roles Anywhere từ foundation nếu dùng Edge on-prem.
4. Đặt `alb_internal = true` nếu client đi qua mạng riêng, hoặc chọn ALB public với `client_cidrs` là các IP egress được phép. Cả hai cần allowlist IPv4. Chọn `egress_mode = "interface"` hoặc NAT mỗi AZ (`egress_mode = "nat"` và `nat_per_az = true`). Nếu dùng tunnel Admin qua SSH, điền `owner_ssh_public_key` trước khi bật fleet. Mặc định ALB drain là 1800 giây. Chọn thời gian phù hợp với request đang chạy.
5. Dùng lệnh plan/show/apply như foundation, thay var-file và saved plan bằng file active riêng. Sau apply, trỏ DNS về `runtime_inventory.alb_dns`, đợi cả hai target healthy và kiểm PUT/GET qua HTTPS. Xem [Cloud fleet runbook](../deploy/cloud/README.md) cho rollout, lifecycle drain và pause/resume.

---

## 5. Phương án Hybrid (Edge + Cloud dùng chung backend AWS)

Trong kiến trúc Miles, Cloud cluster và Edge gateway cùng ghi/đọc trên **một bộ S3 PackStore + 20 bảng DynamoDB + KMS + Secrets Manager**, mỗi node giữ RAM/disk cache riêng tại `/var/lib/miles`. Dữ liệu upload từ Edge có thể đọc lại qua Cloud và ngược lại.

Tổ chức có thể chọn một trong hai cách dựng Hybrid sau:

### 5.1 Biến thể A: Hybrid toàn bộ trên AWS (`deployment.profile: "both"`)

Phù hợp để pilot/kiểm thử luồng Cloud ↔ Edge trước khi đưa máy chủ Edge về hạ tầng on-prem:

- Dùng chính quy trình ở **Mục 4.1**, nhưng trong `$MILES_PRIVATE/installation.json` đặt:
  ```json
  "deployment": {
    "profile": "both",
    "max_cost_usd": 10,
    "max_active_minutes": 180
  }
  ```
- Bộ dựng sẽ tạo cả **Cloud ASG (2 EC2 sau ALB)** lẫn **1 Edge EC2** (chạy Nginx + `aws_signing_helper` + IAM Roles Anywhere), dùng chung backend S3/DynamoDB, và `deploy verify` sẽ tự kiểm tra cả luồng ghi từ Cloud đọc từ Edge và ngược lại.

### 5.2 Biến thể B: Hybrid thực tế (Backend/Cloud trên AWS + Edge tại On-Prem / VM nội bộ)

Đây là mô hình tổ chức đặt một hoặc nhiều máy chủ Edge Linux tại datacenter/văn phòng nội bộ, kết nối HTTPS về AWS backend (hoặc chạy song song với cụm Cloud ALB/ASG trên AWS).

#### Bước 1: Dựng phần AWS Backend (+ Roles Anywhere Trust Anchor & Profile)

1. Theo Mục 4.2 để chuẩn bị IAM/state và foundation. Trước foundation plan, truyền public PEM của **Identity CA dành riêng cho Roles Anywhere** vào `edge_ca_pem` và đặt `edge_certificate_common_name` (ví dụ `org-edge-node`). Giữ Identity CA riêng với CA TLS của Nginx. Terraform tạo trust anchor, profile và Edge role khi có `edge_ca_pem`, kể cả với `enable_edge = false`.
2. Khi foundation đã apply, dùng `miles_tf output -json runtime_inventory` và `miles_tf output -json edge_provider` để lấy:
   - `runtime_inventory.data_bucket_name`
   - `runtime_inventory.kms_key_arn`
   - `runtime_inventory.secret_metadata_arn`
   - `edge_provider.trust_anchor_arn`, `edge_provider.profile_arn`, `edge_provider.role_arn`, `edge_provider.certificate_common_name`
3. Provision secret và metadata một lần theo Mục 6.1. Nếu cần Cloud song song, dùng active plan ở Mục 4.2.3. Nếu chỉ chạy Edge on-prem, giữ `paused = true` và các cờ serving tắt. Cả hai dùng chung S3, DynamoDB, KMS và runtime secret của foundation.

#### Bước 2: Cài đặt & cấu hình máy chủ Edge nội bộ (Linux)

Thực hiện trên từng máy chủ Edge của tổ chức (chi tiết đầy đủ tại [deploy/edge/README.md](../deploy/edge/README.md)):

1. **Chuẩn bị hệ điều hành & đồng hồ:**
   - Linux có `systemd`, Python **3.12+**, Nginx hỗ trợ TLS, và `chrony` đồng bộ với một nguồn NTP IPv4 nội bộ/tin cậy (tắt clock stepping sau đồng bộ ban đầu).
   - Tạo user/group hệ thống `miles` và thư mục `/var/lib/miles` (`chmod 0700`, owner `miles:miles`).
   - Cài đặt binary `aws_signing_helper` từ AWS IAM Roles Anywhere vào `/usr/local/bin/aws_signing_helper` và xác minh SHA-256 trước khi dùng.

2. **Build & stage release Miles trên host Edge:**

   Trên máy build, điền `MILES_ARCH` là kiến trúc của máy Edge (`amd64` hoặc `arm64`):

   ```bash
   cd "$MILES_REPO"
   release="$MILES_PRIVATE/miles-linux-${MILES_ARCH:?Điền kiến trúc}.tar.gz"
   python3 -B deploy/build-release.py --arch "$MILES_ARCH" --out "$release" &&
     sha256sum "$release"
   ```

   Chuyển archive sang máy Edge, đối chiếu SHA-256 và giải nén vào `bundle/`. Điền `MILES_REGION` theo installation trên shell của host Edge, rồi stage:

   ```bash
   sudo python3 bundle/deploy/install.py \
     --bundle "$PWD/bundle" \
     --profile edge \
     --address 127.0.0.1 \
     --region "${MILES_REGION:?Điền Region của installation trên host Edge}"
   sudo systemctl daemon-reload
   ```

   *Lưu ý: không bật `miles-lifecycle.service` trên Edge (service đó chỉ dành cho ASG EC2 trên Cloud).*

3. **Cấu hình IAM Roles Anywhere & phân quyền thư mục:**
   - Cấp chứng chỉ X.509 cho host Edge từ Identity CA ở Bước 1 với `CN` khớp `edge_certificate_common_name`.
   - Đặt `certificate.pem` và `private-key.pem` tại `/etc/miles/rolesanywhere/` với quyền `0750 root:miles` cho thư mục và `0640 root:miles` cho private key.
   - Chỉnh `/etc/miles/aws-config` (từ mẫu [aws-config.example](../deploy/edge/aws-config.example)) điền đúng `region`, `trust-anchor-arn`, `profile-arn` và `role-arn` từ Terraform output `edge_provider`.
   - Đảm bảo `/etc/miles/runtime.env` giữ `AWS_EC2_METADATA_DISABLED=true` để helper hỏng không fallback nhầm sang credential khác.

4. **Hoàn thiện `/etc/miles/startup.json` (từ mẫu [shared-edge.example.json](../configs/shared-edge.example.json)):**
   - `runtime_node_id`: nhãn duy nhất cho máy Edge (VD: `"edge-hn-01"`).
   - `region`, `ddb_table_prefix` (`"<installation_name>-"`, VD `"miles-org-"`), `pack_bucket`, `pack_kms_key_id`, `runtime_secret_arn`: điền khớp Terraform output.
   - `aws_config_file`: `"/etc/miles/aws-config"`, `aws_profile`: `"miles-runtime"`.
   - `utc_reference`: IPv4 nguồn `chrony` trên máy Edge (kiểm bằng `chronyc -n -c -h 127.0.0.1 tracking`).
   - Giữ `s3_address: "127.0.0.1:8080"`, `admin_address: "127.0.0.1:9000"`, `health_address: "127.0.0.1:9100"`.

5. **Cấu hình Nginx TLS làm front door (`:443` → `127.0.0.1:8080`):**
   - Đặt chứng chỉ TLS thật của tổ chức tại `/etc/miles/tls/fullchain.pem` và `/etc/miles/tls/private-key.pem` (chỉ root đọc).
   - Include [nginx.conf](../deploy/edge/nginx.conf) vào block `http {}` của Nginx (giữ nguyên `Host $http_host`, `merge_slashes off`, `underscores_in_headers on`, tắt buffering và tắt retry write). Kiểm tra `sudo nginx -t`.

6. **Khởi động và kiểm tra Edge:**

   ```bash
   sudo systemctl enable --now miles &&
     curl -fsS --retry 20 --retry-connrefused --retry-delay 2 --max-time 3 http://127.0.0.1:9100/readyz &&
     sudo nginx -t &&
     sudo systemctl enable --now nginx
   ```

   Nếu readiness vẫn lỗi, kiểm `journalctl -u miles`, provider, kết nối backend và chrony trước khi mở Nginx. Khi Nginx đã chạy và vừa đổi cấu hình, dùng `sudo nginx -t && sudo systemctl reload nginx` sau khi kiểm readiness.

   Chỉ mở Nginx `:443` cho dải mạng client nội bộ được phép; **không** expose `:8080`, `:9000`, `:9100` ra ngoài loopback.

---

## 6. Khởi tạo quản trị & onboard người dùng trong tổ chức

Sau khi hạ tầng backend đã sẵn sàng (áp dụng cho cả **Cloud** và **Hybrid**; nếu dùng `deploy-installation.py provision`, bước bootstrap token đầu tiên đã được thực hiện tự động và token nằm ở `$MILES_PRIVATE/system-admin-token`):

### 6.1 Provision secret và Admin token lần đầu (Terraform trực tiếp)

Sau foundation, khi mọi gateway đang dừng và cả 20 bảng còn rỗng:

1. Tạo `$MILES_PRIVATE/runtime-keys.json` mode `0600` có `key_id` (ví dụ `initial`), `seal_key` và `cursor_key`. Dùng hai giá trị độc lập, mỗi giá trị là 32 byte ngẫu nhiên mã hóa base64 (`openssl rand -base64 32`). Chỉ tạo giá trị ban đầu. Giữ nguyên keys nếu phải kiểm tra một lần provision chưa rõ kết quả. Xem [contract secret](development/shared-runtime.md#aws-provider-and-utc-source). Điền `MILES_REGION` và `MILES_RUNTIME_SECRET_ARN` từ installation và `runtime_inventory.secret_metadata_arn`, rồi dùng deploy role:

   ```bash
   aws --region "${MILES_REGION:?Điền Region}" secretsmanager put-secret-value \
     --secret-id "${MILES_RUNTIME_SECRET_ARN:?Điền runtime secret ARN}" \
     --secret-string "file://$MILES_PRIVATE/runtime-keys.json"
   ```

2. Copy [shared Edge config](../configs/shared-edge.example.json) thành `$MILES_PRIVATE/operator-startup.json`. Đặt `runtime_node_id` là `operator`, `local_data_path` là đường dẫn tuyệt đối trong private directory, `utc_reference` là nguồn chrony IPv4 trên máy điều phối, và điền Region, table prefix, bucket, KMS key và secret ARN từ foundation. Đặt `aws_config_file` tới AWS config chứa profile `credential_process` ở Mục 4.2.1 và `aws_profile` bằng tên profile đó. Vai trò deploy cần đọc secret, ghi metadata DynamoDB và ghi genesis/catalog vào bucket S3 bằng KMS của installation.
3. Từ repo root, chạy bootstrap **một lần**:

   ```bash
   ./bin/dsgctl admin-token bootstrap \
     --runtime-config "$MILES_PRIVATE/operator-startup.json" \
     --user operator \
     --token-file "$MILES_PRIVATE/system-admin-token"
   ```

Nếu bootstrap thất bại mà chưa rõ đã ghi gì, giữ token và artifacts để kiểm tra backend trước khi tiếp tục. Cấu hình operator chỉ dùng trên máy điều phối. Gateway Edge dùng profile Roles Anywhere ở Mục 5.2.

### 6.2 Kết nối `dsgctl` an toàn vào Admin API

- **Trên Cloud:** dùng SSH config đã pin host key của node qua mạng riêng hoặc SSH qua SSM, với tài khoản `miles-owner` đã cấu hình bằng `owner_ssh_public_key`. Nếu dùng SSM, role cần quyền `AWS-StartSSHSession` cho node được chọn. Giữ tunnel mở trong terminal riêng để forward `127.0.0.1:19000` trên máy điều phối tới **`127.0.0.1:9000` trên gateway**. Ví dụ dùng [admin_tunnel.py](../scripts/admin_tunnel.py) với system token ở Mục 6.1 (hoặc từ `deploy provision`):
  ```bash
  python3 -B scripts/admin_tunnel.py --ssh-config "$MILES_PRIVATE/ssh-config" \
    --target cloud-a --local-port 19000 \
    --token-file "$MILES_PRIVATE/system-admin-token" --user-id operator
  ```
  Thay `cloud-a` bằng alias trong SSH config. Khi tunnel báo sẵn sàng, dùng terminal khác:
  ```bash
  export DSGCTL_ENDPOINT=http://127.0.0.1:19000
  export DSGCTL_TOKEN_FILE="$MILES_PRIVATE/system-admin-token"
  ```
- **Trên Edge:** thao tác trên host qua loopback, hoặc mở SSH tunnel từ cổng local `9000` tới `127.0.0.1:9000` trên host Edge. Giữ token trên máy chạy `dsgctl`:
  ```bash
  export DSGCTL_ENDPOINT=http://127.0.0.1:9000
  export DSGCTL_TOKEN_FILE="$MILES_PRIVATE/system-admin-token"
  ```

### 6.3 Tạo user, cấp S3 credential và phân quyền bucket cho từng team

```bash
# 1. Tạo user cho từng team/ứng dụng (đặt default_domain để cô lập hoặc dùng chung phạm vi dedup, và logical_limit theo byte)
./bin/dsgctl user create team-backup --default-domain team-backup --logical-limit 1099511627776
./bin/dsgctl user create team-reader --default-domain team-backup

# 2. Cấp credential S3 SigV4 cho từng user
./bin/dsgctl credentials create --user team-backup --out "$MILES_PRIVATE/team-backup-s3.secret"
./bin/dsgctl credentials create --user team-reader --out "$MILES_PRIVATE/team-reader-s3.secret"

# 3. Cấp owner bearer token (không có quyền system_admin) để chủ bucket tự quản lý grant
./bin/dsgctl admin-token create --user team-backup --ttl 24h --out "$MILES_PRIVATE/team-backup-admin-token"
```

Lấy `access_key_id` từ JSON in ra stdout khi tạo credential. File `--out` mới có mode `0600` và chỉ chứa `secret_access_key` dạng text. Dùng đủ cặp để cấu hình một profile AWS CLI dành riêng cho endpoint S3 của Miles theo [hướng dẫn client](api-usage-guide.md). Sau khi `team-backup` tạo bucket `org-backups`, dùng owner token để cấp quyền đọc cho `team-reader`:

```bash
export DSGCTL_TOKEN_FILE="$MILES_PRIVATE/team-backup-admin-token"
./bin/dsgctl bucket show org-backups
bucket_id=REPLACE_WITH_BUCKET_ID_FROM_OUTPUT
./bin/dsgctl grant set org-backups team-reader --read=true --write=false \
  --expected-revision 0 --expected-bucket-id "$bucket_id"
```

---

## 7. Checklist nghiệm thu và vận hành sau khi dựng

Trước khi bàn giao hệ thống cho các thành viên trong tổ chức sử dụng:

1. **Xác minh sẵn sàng & tính toàn vẹn dữ liệu:**
   - Mọi node trả `200` ở `/healthz` và `/readyz`; nếu có ALB, toàn bộ target healthy.
   - Chạy thử `create-bucket` → `put-object` → `head-object` → `get-object` → so sánh `sha256sum` / `cmp`. Với Cloud/Hybrid, kiểm qua HTTPS của endpoint thực và dùng `--cacert` hoặc `AWS_CA_BUNDLE` nếu có CA riêng. Với Local, dùng HTTP loopback.
   - Nếu có cả Cloud và Edge, PUT từ Edge rồi GET và kiểm SHA-256 từ Cloud, sau đó thử chiều ngược lại.
2. **Xác minh ranh giới bảo mật:**
   - Cổng `:8080` (direct S3 backend), `:9000` (Admin API), `:9100` (`/metrics` & health) không mở ra Internet.
   - Đã tạo user riêng cho client/owner, không dùng `system-admin-token` cho tác vụ hằng ngày.
3. **Giám sát và bảo trì:**
   - Kiểm tra CloudWatch dashboard (`enable_dashboard=true`) theo [cloudwatch-dashboard.md](runbooks/cloudwatch-dashboard.md).
   - Nắm quy trình tiering, restore/promote, node drain và điều kiện dừng khẩn cấp tại [lifecycle-recovery-operations.md](runbooks/lifecycle-recovery-operations.md).
   - Với bộ dựng thử nghiệm, chạy `deploy plan paused`, review saved plan, rồi `deploy apply paused --plan-sha256 REPLACE_WITH_PAUSE_PLAN_SHA256` để drain và tắt tài nguyên phiên. Storage, KMS key và secret giữ lại vẫn có thể tính phí.
   - Nếu cần xóa dữ liệu thử, hoàn tất apply paused trước. Sau khi chủ account duyệt phạm vi xóa và cấp quyền cleanup, làm theo [runbook teardown](testing/aws-personal-account-guide.md#8-pause-hoặc-full-cleanup) để plan/apply `unprotect`, plan/apply `destroy` với `--delete-data` rồi `deploy closeout`. Với Terraform trực tiếp, dùng saved plan và runbook của chính installation. Nếu có Edge on-prem, drain/dừng host trước khi thu hồi backend dùng chung.
