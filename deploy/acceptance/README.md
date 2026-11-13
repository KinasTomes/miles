# Focused AWS acceptance runner

Bộ chạy cho một installation dev disposable: hai Cloud, một Edge, một client,
private networking/TLS, shared DynamoDB/S3. Chạy GET và shutdown/native Outbox
throttling, thu evidence rồi cleanup. Toàn bộ provisioning/runtime/test dùng
code trong repo; không cần script hay secret của lượt cũ.

Hướng dẫn từng lệnh: [operator guide](../../docs/testing/focused-aws-operator-guide.md).

Chạy theo template bằng một process, có checkpoint/receipt và báo cáo gọn:
[process guide](../../docs/testing/aws-acceptance-process.md). Bộ điều phối gọi
lại các lệnh dưới đây, không thay runtime hoặc chính sách audit.

Entry point:

```bash
python3 deploy/acceptance/run.py --run-dir /absolute/private/new-run --help
```

`configure` đọc account, role ARN, state bucket và chọn/pin AMI từ AWS; không tạo
resource. `init` tạo PKI/SSH key mới, freeze source và build release; `assets` tạo
RPM bundle có chữ ký từ image AL2023 pin digest. Các bước live tách rõ plan,
review và mutate. Mutations cần `--confirm-run` đúng run ID; không tự apply sau
khi plan. Profile IAM chỉ được gọi mutation IAM; workload dùng deploy role.

## Prerequisites

- Linux Bash, Python 3.12+, Go theo `go.mod`, Terraform theo lockfile trong repo,
  AWS CLI có `aws login --remote`, Session Manager plugin, OpenSSH, OpenSSL,
  Docker chạy được ARM64 image, `uvx` để chạy IAM Policy Autopilot.
- Account đã có bootstrap operator/deploy role, runtime boundary và state bucket
  theo `infra/terraform/cloud/bootstrap/`. Bộ chạy không tạo IAM user/MFA hay
  bootstrap backend. `configure` kiểm các prerequisite này bằng API.
- Chỉ một installation `miles-dev` trong account/Region. Runner từ chối state,
  bucket data hoặc runtime roles đã tồn tại; không adopt dữ liệu người khác.
- Deployment mới này chưa được chạy lại trên AWS. Offline contracts không thay
  cho native IAM/plan review và nghiệm thu trên account của người vận hành.

## Nguồn tài sản và quyền

- `policies/*.json.tmpl` parameterize các scope đã kiểm ở lượt focused trước;
  mỗi lượt render account/Region/AMI/secret/auditor/expiry mới. Review native
  Access Analyzer, simulator và baseline Autopilot theo plan trước activation.
  Không attach thẳng output của policy generator hoặc cấp AdministratorAccess.
- IAM versions/defaults trước activation được giữ lại. Restore không ghi đè
  concurrent policy changes. Cleanup grant riêng chỉ bind IDs trong manifest.
- Assets image: `public.ecr.aws/amazonlinux/amazonlinux` pin digest trong
  `prepare.py`; gói RPM kiểm chữ ký ở build và host, archive kiểm SHA.
- Roles Anywhere helper pin bản 1.8.5 ARM64 đã dùng trong lượt trước, SHA từ
  [AWS credential helper](https://docs.aws.amazon.com/rolesanywhere/latest/userguide/credential-helper.html).
  TLS CA và Roles Anywhere CA riêng, chỉ dùng cho test private hai ngày.
- Config/plan/state/key/token/raw logs ở run directory 0700/file 0600 ngoài Git.
  Runtime secret không nằm trong Terraform/user-data/argv. Bootstrap credentials
  đi stdin tới memfd, không ghi ra disk host. Không có static AWS access key.

## Giới hạn và lỗi

Fresh pricing và envelope là dự toán, không phải hard billing cap. Runner chặn
tải mới khi vào reserve cleanup 90 phút, nhưng không thay người vận hành giám
sát thời gian, request costs, auth sessions hoặc xử lý một lệnh bị gián đoạn.
Không có timer âm thầm tự xóa AWS khi người khác đang dùng.

Nếu apply/staging mất response, dùng `recover-apply`/`recover-host` để đọc proof
trước khi tiếp tục. Bootstrap có intent mà chưa confirmed phải giữ token/status
và reconcile; không reset backend hoặc lặp bootstrap. Partial apply có graph
chưa đủ cho strict inventory hoặc host không liên lạc được sẽ dừng cleanup để
reconcile ownership/quiescence; không force-delete để tạo PASS. Giữ receipt lỗi,
state và account baseline cho việc xử lý đó, không mở lượt khác cùng namespace.

Throttling pulse cố ý gây tải chỉ đọc, bốn worker/120s, cap Outbox128 giữ nguyên.
Không có throttle thì ghi chưa exercise. Shutdown lỗi vẫn ghi FAIL dù peer
hoàn thành sau đó; PUT409 không nằm trong scope. Final audit dùng verifier hiện
có, chỉ chấp nhận đúng KMS/secret mới chờ xóa30ngày và không có residual khác.
