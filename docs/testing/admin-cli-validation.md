# Admin CLI và regression API theo vai trò

Ngày 04/11/2026, base `origin/main` `facac0c`. Go 1.27.2 linux/amd64 (WSL2),
Python 3.12.3, boto3 1.43.122, Moto `moto_server` loopback. Không dùng AWS thật.

Acceptance: user/owner/admin chạy đúng command theo scope token; schema API và tài liệu
khớp hành vi. Hợp đồng: [API §6.4–6.5, §7](../dedup-storage-gateway-api.md).

## Checklist và bằng chứng

| Yêu cầu | Hiện thực và kiểm thử |
|---|---|
| Pagination, error handling | Mọi list (`user/grant/credentials/admin-token/job/node/pack ls`, `pack show`) dùng chung `--limit 1..1000 --cursor`; chỉ gửi tham số được đặt. Lỗi Admin in status, `error.code` (chỉ `[a-z_]`) và `request_id` 32 hex để tra audit log; không in `message`/body. 501 có envelope (vd. `job_type_unavailable`) là lỗi gateway; router 404/405 và 501 trần vẫn là unsupported. Test: `TestWriteMethodsErrorCodesAndNoContent`, `TestResource404IsNotUnsupportedEndpoint`. |
| Token file 0600, endpoint SSH, secret | Token chỉ đọc từ file regular 0600 không symlink; không có flag/env chứa raw token. `DSGCTL_ENDPOINT`/`DSGCTL_TOKEN_FILE` cho tunnel cố định, flag thắng, resolve sau parse nên help không in giá trị env; endpoint env vẫn qua kiểm tra loopback/HTTPS. Secret của create chỉ vào file `--out` mới 0600. Test: `TestEnvironmentSelectsTunnelEndpointAndTokenFile`, `TestCreateWritesSecretOnlyToNewPrivateFile`. |
| Nối stats/layout/cache/job/node/packs, từ chối scope không đủ | `stats [--bucket]`, `layout <bucket> <key>` (query encode một lần), `pack ls/show/restore/promote`, `cache stats/purge --scope cluster\|node --node id`, `job`, `node`. Scope cache sai và id/path không an toàn bị từ chối trước transport. `TestCLIRolesAgainstAdminServer` chạy CLI thật với Admin server thật trên Badger (stats collector, lifecycle catalog, jobs cluster): system admin qua, owner/stranger 403 cho route system; owner qua cho bucket của mình, stranger 403; owner không tự nâng quyền (`user set`, `admin-token create --system-admin`) và không để lại file secret. |
| Unknown privilege fields | `TestUnknownPrivilegeFieldsWriteNothing`: field ngoài schema (kể cả lồng trong policy) ở PATCH user, POST admin-tokens/credentials/jobs/cache purge/drain, PUT policy trả 400 với cả system admin; đếm token, credential, revision user/policy, job không đổi. Grant/bucket policy/user create đã có ở `TestStrictJSON`. |
| `expected_bucket_id` khi bucket tái tạo | `TestRecreatedBucketRejectsStaleBucketID`: DeleteBucket rồi CreateBucket cùng tên qua producer thật; bucket_id cũ ở grant set/rm và bucket policy set trả 409 `bucket_id_mismatch`, không ghi; grant cũ không xuất hiện ở incarnation mới. Grant chưa có vẫn 404 trước 409 theo thứ tự lỗi §6.5.1. |
| Concurrent grant/policy CAS | `TestConcurrentCASHasOneWinner`: 16 writer song song cùng expected revision cho grant, global policy, bucket policy; đúng một 200, còn lại 409 `revision_conflict`, revision tăng đúng 1. |

## Lệnh

```sh
gofmt -l cmd internal
go vet ./...
go test -race -count=1 ./cmd/dsgctl ./internal/adminclient ./internal/api/admin
```

## Smoke tiến trình thật (local mode)

`miles` local (Badger, pack S3 Moto loopback, `admin_address`) và binary `dsgctl` build
từ cây làm việc. Token đầu tiên cấp bằng `admin-token bootstrap --local-data-path` khi
gateway dừng (file 0600). Bucket `demo` và object tạo qua SigV4 bằng user `dev`; system
admin cấp token owner (`dev`) và `stranger` qua `admin-token create --out` (file 0600,
stdout `[REDACTED]`). Endpoint/token lấy từ `DSGCTL_ENDPOINT`/`DSGCTL_TOKEN_FILE`.

| Lệnh | system admin | owner | stranger |
|---|---|---|---|
| `stats` | 200 | 403 | 403 |
| `stats --bucket`, `layout`, `pack ls`, `cache stats`, `job ls`, `node ls` | 503 `unavailable` (local chưa gắn provider) | 403 | 403 |
| `grant ls demo`, `bucket show demo` | 200 | 200 | 403 |

Owner `grant set` với bucket_id cũ: 409 `bucket_id_mismatch`; với bucket_id hiện tại:
200 revision 1; stranger có grant read vẫn 403 ở `grant ls`. Owner
`admin-token create --system-admin`: 403, không tạo file. `cache purge` thiếu scope và
`cache stats --scope node` thiếu `--node` bị từ chối không gửi request; cursor giả: 400
`invalid_cursor`. Log gateway không chứa secret của token; `--token <raw>` bị từ chối.

## Giới hạn

- Smoke local trả 503 cho route mà local mode chưa gắn provider; bằng chứng scope và
  wire của các route này là test CLI với Admin server thật, không phải Cloud/Edge.
- Kiểm tra quyền file 0600 dùng mode bit POSIX; `dsgctl` không build trên Windows.
- Chưa chạy trên Cloud/Edge thật hay DynamoDB; CAS concurrent ở đây chạy trên Badger.
- `cost report`, `fault`, `rebuild-index`, `sim run` vẫn unsupported.
