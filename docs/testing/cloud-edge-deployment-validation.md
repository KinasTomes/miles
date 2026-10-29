# Cloud / Edge deployment validation

Task **W04-HUNG-01** — ngày 29/10/2026. Scope: cấu hình Cloud ASG/ALB và Edge skeleton
để thử thật W5, cùng binary/business logic; không phải nghiệm thu deployment AWS/
non-AWS hay R1/R2. Dependencies W03-HUNG-01 và W03-HUNG-03 đã merge (PR #30/#38).

## Checklist và bằng chứng

| Checklist | Implementation | Bằng chứng local |
|---|---|---|
| LT/ASG hai AZ, HTTPS ACM, SG8080/9000/9100 | Opt-in fleet, hai private node/subnet; reuse ALB/SG/data stack | Terraform mock active/pause/resume, AMI/root-disk/IMDS/TLS/ports contracts |
| Host/path, 120/150/1800s, heartbeat | ALB giữ Host; app idle150; termination controller stop + heartbeat + ALB proof | Native Nginx TLS/SigV4 fixture; Python deadline/failure/restart cases |
| Cùng binary, Edge TLS/provider, không commit secret | Allowlisted versioned release, manifest SHA, systemd, Nginx, Roles Anywhere provider config | Installer/tamper/architecture/immutable config tests; native release smoke in CI |

Các kiểm tra đã chạy tại worktree task:

- `make check`: format/vet, toàn bộ Go tests, 18 Python tests, document-name check,
  build Miles/dsgctl/warmbench — pass. Proxy test opt-in được chạy riêng bên dưới,
  không coi skip mặc định của `go test ./...` là bằng chứng Nginx.
- Terraform **1.15.2**, AWS provider **6.21.0**: init backend-disabled/readonly lock,
  validate, fmt và **31 mock cases** — pass. Có giữ datastore qua pause/resume,
  từ chối release không pin/compute dùng chung/single-AZ NAT, gateway không có
  S3Delete, KMS CreateGrant chỉ custom SLR và AWS resource.
- `python3 -B -m unittest discover -s infra/terraform/cloud/scripts/tests -v`:
  **12/12** offline guard tests pass.
- `MILES_EDGE_PROXY_TEST=1 go test -v -count=1 ./deploy/edge`: pass, real Nginx/TLS,
  native Miles SigV4 router, special paths/query/underscored metadata/binary PUT;
  tampered Host bị 403. TLS certificate test được trust bằng CA riêng, không tắt
  verification. Nginx official **1.30.6**, image pin ở `proxy_test.go`.
- IAM Autopilot **0.3.0** chạy trên Terraform show-JSON offline synthetic fleet
  plan, chỉ sinh private baseline; không upload/apply IAM. AMI lookup thay fixture,
  không gọi AWS. Baseline cần review quyền wildcard/provider lifecycle trước W5;
  xem [operator handoff](../../infra/terraform/cloud/bootstrap/operator/README.md).

Chạy lại native release + slow TLS proof từ worktree sạch:

```bash
python3 -B scripts/deployment_smoke.py --out "$PRIVATE_DIR/new-deployment-smoke"
```

Job CI `deployment-contracts` chạy cùng lệnh: build một native release, stage cả
hai profile, so SHA binary, kiểm health200/ready503/metrics/SIGTERM và response65s
qua proxy bằng test Go có race detector. Không cần AWS credentials. Native process
fixture đổi health sang loopback/port động và shutdown2s, **không** sửa deployment
config1800s hay bật production readiness.

Native smoke đã pass trên **Linux arm64**, Go **1.27.2**, Python **3.12.3**, source
[`22374bb`](https://github.com/KinasTomes/miles/commit/22374bb6dc591c4eef0a0a56ee2322fda2bafb71).
Hai profile cùng Miles SHA-256
`04b3b598066604a847e8e498a0c7a016b3b7f3a8572f59ec4e914c53e6c95518`;
archive18.211.859bytes, hash/version ghi trong
[summary](evidence/cloud-edge-deployment/summary.json) và
[manifest](evidence/cloud-edge-deployment/manifest.json).
[TLS/race log](evidence/cloud-edge-deployment/tls-proxy.log) ghi response65s pass;
[checksums](evidence/cloud-edge-deployment/sha256sums.txt) được CI kiểm tra. Các
commit sau source này chỉ bổ sung evidence/tài liệu và bước kiểm checksum CI.

## Handoff trước live W5

| Phần cần nối/kiểm thật | Task nhận |
|---|---|
| Compose runtime Cloud/Edge: S3/Admin listeners, shared metadata, credential providers, R1/R2/workers | W05-HUNG-01, phối hợp các owner bên dưới |
| Admin identity/auth APIs trên9000 | W04-CHIEN-01 |
| Shared DDB R1 admission, exporter/epoch seal thật | W04-PHONG-03 / W04-PHONG-04 |
| Jobs/Nodes/metrics và drain state vận hành | W04-HUNG-02, W05-HUNG-02 |
| Signed/presigned compatibility qua ALB thật và host Edge ngoài AWS | W05-CHIEN-01 / W05-HUNG-01 |

Current production startup vẫn trả ready503 và chưa mở S3/Admin. Không chấp nhận
`mode=dev/local`, fake backend/event hay đổi readiness thành200 để nghiệm thu fleet.
DDB adapter/producer đã merge không tự đồng nghĩa composition đã tồn tại.

Chưa chạy: AWS apply, ACM/DNS thật, native EC2/systemd boot, real lifecycle APIs,
active request drain/R2 flush qua replacement/pause, IAM Access Analyzer/simulator
cho delta fleet, Roles Anywhere token renewal/revocation, non-AWS Edge host.
Các bước này cần runtime integration, host/certificate/identity và budget được duyệt
cho W5. Timeout hook là best effort, không chứng minh data durability khi máy bị
terminate đột ngột. Không thay đổi hay tạo resource AWS trong W04-HUNG-01.

Runbooks: [release chung](../../deploy/README.md), [Cloud](../../deploy/cloud/README.md),
[Edge](../../deploy/edge/README.md). Evidence local đầy đủ giữ ngoài Git tại
`/home/ubuntu/.local/state/miles/cloud-edge-deployment`; không commit binaries,
certificate test, planJSON hay private environment config.
