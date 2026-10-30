# Admin identity, credential, token, grant và policy validation

Task **W04-CHIEN-01**, ngày 30/10/2026, từ `origin/main` `893b9ff`. Hard dependencies
W02-CHIEN-02, W03-HUNG-03, W04-HAI-01 và W04-PHONG-02 đã Done trên backlog.
Acceptance: route Admin danh tính/quyền/policy P1 pass auth/CAS fixture; owner không
tự nâng quyền hoặc đổi domain/tiering bucket. Hợp đồng: [API §2.2.3, §6, §7](../dedup-storage-gateway-api.md).

| Checklist | Implementation / proof |
|---|---|
| Users CRUD/usage, bucket metadata/grants, global/cache policy routes; schema/pagination/CAS/bucket_id | `internal/api/admin` server trên Badger thật: `TestErrorOrder`, `TestRevisions`, `TestStrictJSON`, `TestPagination`, `TestBucketPolicyRefusedWritesNothing`, `TestRevokeAtMaxRevision`; mọi dòng Admin của `authorization.json` qua server (`TestAdminAuthorizationFixture`) |
| Credentials, bearer admin-token, offline bootstrap có điều kiện; secret chỉ trả một lần | `TestUsersAndSecrets` (secret/token chỉ ở response tạo, không ở list/audit, secret niêm phong AES-GCM bind access key); `RunIdentity` trên Badger và DynamoDB Local (bootstrap một lần, lần hai `ErrConflict` không ghi gì); `TestBootstrapBadgerOnceOffline`, `TestBootstrapKeepsTokenFileWhenOutcomeUnknown` |
| dsgctl user/usage/grant/policy/bucket-policy/credential/token; owner tunnel + scope | `TestIdentityCommandsWireSchema`, `TestIdentityInvalidArgumentsNeverReachTransport`, `TestCreateWritesSecretOnlyToNewPrivateFile`, `TestPolicySetSplicesRevisionWithoutReencoding`, adminclient PUT/PATCH + mã lỗi API |
| Admin không data grant, write-only, revoke ≤ 5 s, unknown privilege field | `TestAcceptance`: `s3.Router` dùng chung store Badger với Admin server, đồng hồ ảo |

## Kiểm thử tự động

- `TestAcceptance`: credential system admin không có grant bị `AccessDenied` cả GET/PUT;
  grant chỉ write cho PUT, GET `AccessDenied`; thu hồi credential, DELETE grant và
  thu hồi bearer vẫn admit ở 4,999 s, bị từ chối từ 5 s; body grant có `system_admin`
  hoặc `owner` trả 400 và không ghi gì.
- Thứ tự §6.5.1 từng bước, gồm bước 3 tra chủ token ở `DELETE /admin-tokens`; 401 một
  message kèm `WWW-Authenticate: Bearer`; 404 không envelope cho route chưa phục vụ, 405.
- CAS: `If-Match`/`expected_revision`, no-op giữ revision, `revision_conflict` trước no-op,
  `revision_exhausted` khi đổi ở 2^53−1 (no-op ở max vẫn 200), `bucket_id_mismatch`;
  tombstone grant tạo lại bằng `expected_revision` 0.
- JSON strict: member trùng (kể cả escape), null, member lạ, `owner`/`system_admin` ở
  grant, `tiering/compression/dedup_domain` ở bucket policy, số thập phân/mũ/âm/> 2^53−1,
  body > 16 KiB, dữ liệu thừa; không có lần nào ghi metadata.
- Cursor: duyệt hết trang; từ chối khi đổi principal, path, filter, bucket, hết hạn, sửa.
- Transaction (shared contract `RunIdentity`, cả Badger và DynamoDB Local): token user
  chỉ grant trên bucket mình sở hữu và thu hồi token của mình; grant cho owner,
  `debug_allowed` thiếu `system_admin`, đổi cờ credential hoặc `issued_at` bị từ chối;
  `PutPolicy`/`PutBucketPolicy` với `AdminTokenID`; list theo thứ tự byte, filter user,
  bỏ tombstone.
- Runtime: `admin_address` chỉ local/benchmark và loopback; listener thật trả 401 không
  bearer và 201 với token bootstrap; credential tạo qua Admin ký được S3 local.

Kết quả trên source hiện tại: `gofmt`, `go vet ./...` sạch; `go test -count=1 ./...` pass
trên Linux (Go 1.27.2, WSL); `go test -race` pass cho `internal/api/...`,
`internal/storage/...`, `internal/metadata/...`, `internal/app`, `internal/adminclient`,
`internal/config`, `cmd/miles`, `cmd/dsgctl`; contract DynamoDB (`internal/metadata/dynamodb`,
`badger`, `storage/contracttest`, `readpath`) pass với DynamoDB Local 3.3.2 loopback và
`MILES_DDB_LOCAL_REQUIRED=1`.

## Smoke tiến trình thật (local mode)

Binary Linux, Moto S3 loopback làm pack backend, `mode=local` với `s3_address`,
`admin_address`. Quan sát:

1. `dsgctl admin-token bootstrap --local-data-path` khi gateway dừng: token file 0600,
   stdout chỉ `token_id`/`expires_at`; chạy lần hai bị từ chối, không tạo file; chạy khi
   gateway đang giữ Badger lock bị từ chối.
2. Không bearer → 401 + `WWW-Authenticate: Bearer`. System token: tạo user, credential
   (secret chỉ vào `--out`, stdout `[REDACTED]`), token user cho owner `dev`; list
   không có secret; `policy get` trả revision 1 do bootstrap tạo.
3. Credential tạo qua Admin ký SigV4 được (lỗi là `AccessDenied`, không phải
   `InvalidAccessKeyId`); credential system admin không grant bị `AccessDenied` GET/PUT.
4. Token owner: `user ls`, `credentials create` → 403; grant cho chính mình → 400;
   `system_admin` trong body grant và `tiering` trong bucket policy → 400, list grant
   rỗng; grant/đổi/DELETE grant hoạt động, bucket policy revision 1 → 2, CAS cũ → 409.
5. Thu hồi credential: request sau đó `InvalidAccessKeyId`; thu hồi lần hai 204. Owner tự
   thu hồi token: còn dùng được trong cửa sổ snapshot rồi 401 từ khoảng 5 s.
6. Disable user, PATCH revision cũ → 409; restart gateway: system token và credential
   niêm phong trước restart vẫn xác thực; cursor cấp sau restart dùng được, dùng cho
   resource khác → 400 `invalid_cursor`.
7. Audit log JSON mỗi mutation (request_id, principal, route, resource, status), không
   chứa secret/token/hash.

Local runtime chỉ admit ghi object của user `dev` (single-owner), nên PUT của grantee
write-only được chứng minh ở `TestAcceptance` qua Router, không qua object service local.

## Giới hạn và bàn giao

- Runtime Cloud/Edge (DynamoDB thật, Sealer KMS, cursor key dùng chung từ SSM/Secrets
  Manager, listener Admin private) thuộc W05-HUNG-01; mode `runtime` từ chối
  `admin_address`.
- Bootstrap chưa ghi outbox epoch 0 (W04-PHONG-04); `--recover` báo lỗi tới khi có
  recovery gate R2 §15.
- `/objects/layout`, `/buckets/{b}/stats` thuộc W04-HAI-03 (fixture liệt kê pending);
  jobs/nodes/packs/cache/cost/faults chưa có route.
- DynamoDB list users/credentials/tokens quét cả bảng mỗi trang (không GSI).
