# Outbox exporter và recovery catalog prototype

Task: [W03-PHONG-03](https://miles.extase.dev/tasks/W03-PHONG-03).
Baseline: `origin/main` tại `87d0769` (26/10/2026).

## Readiness và phạm vi

Khi đọc board, task Todo/Ready; hard prerequisites W01-PHONG-03,
W02-PHONG-03 và W03-HAI-02 đều Done. Main có Badger sync writes,
object/config transaction outbox và coordination fault harness. Đủ điều kiện
triển khai prototype W03. Không có bằng chứng live AWS hoặc complete recovery.

`internal/recovery.Exporter` chạy bounded work qua shared
`storage.OutboxExportStore`, được implement bởi Badger và metadata fake.
Không tạo metadata database hoặc authoritative event ledger thứ hai.
`exports` chỉ là tiến độ riêng của immutable `events`; không xóa event, payload,
outcome, manifest hoặc root. Mark compare toàn bộ event và descriptor identity;
retry cùng giá trị idempotent, khác giá trị conflict.

Prototype không claim job lease/fencing hoặc distributed takeover. Mark là
monotonic progress trên exact immutable bytes, không cấp authority, không dùng
để cho phép cleanup. Production worker claim/fenced publication, close/seal và
retention conjunction thuộc W04 và các integration gates của R2.

## Đường export và retry

1. Đọc committed outbox theo ID. PayloadSource đọc đúng retained immutable input
   qua PayloadPointer; không dựng từ object head/policy hiện tại.
2. Kiểm SHA-256 của nguyên input, schema, history/event/epoch, page number,
   effects/revisions và bounds trước khi ghi artifacts.
3. Ghi `journal/{history}/{epoch}/{event}/pages/{number}.json` và đọc lại exact
   bytes. Descriptor được ghi cuối, bind source envelope, mutation/logical ID,
   page order, lengths và SHA-256. Full input digest nằm trong source envelope.
4. Đọc lại descriptor rồi mới compare/mark tiến độ trong DB. Không có thao tác
   nào gán epoch Sealed hoặc công bố supported cut.

`RunPage` liệt kê base events theo ID, filter history/epoch, limit1..256 base
records được đọc trước filter; trang rỗng vẫn có thể có Next. Cursor strict-after.
Scan epoch Open chỉ để tìm work. Nếu một event lỗi, cursor
trả về event thành công ngay trước nó; retry không bỏ event lỗi. Caller giữ cursor
và schedule. Không có background worker tự khởi chạy trong `cmd/miles`.

`NewS3Objects` nhận AWS SDK client/bucket/prefix/KMS key của caller. PutObject
dùng `If-None-Match: *`, `STANDARD`, optional SSE-KMS; verify toàn bộ body,
không dùng ETag làm checksum. Lost ACK/conditional collision được reconcile
bằng readback; khác bytes fail closed, không overwrite. Readback lỗi giữ unknown
và không mark. S3 API stub kiểm tra request thật của adapter; chưa chạy AWS.
IAM phải cấm overwrite/delete/tiering journal; prototype không provision IAM.

## Schema thử nghiệm

`Input`, `EventPage`, `Descriptor`, `Catalog` và `Inventory` là schema prototype,
không thay golden/schema W02 hoặc `MILESPAG` kinds1–4. Metadata ≤256KiB,
fanout≤256, depth≤8; integer0..2^53−1, revision positive. Để giữ scope W03,
toàn retained input cũng ≤256KiB; chưa hỗ trợ hierarchical manifests hoặc
binary FORMAT attachments. Logical/mutation ID dùng cùng OpID vì shared
producer hiện chỉ hỗ trợ final object/config transactions, chưa child mutations.

Codec dùng đúng ASCII/nonnegative-integer subset của W02 fixture. Từ chối
Unicode ngoài subset, null, duplicate/unknown fields, noncanonical/trailing bytes
và unsupported versions ở semantic validator. Không nhận full RFC8785/JCS
conformance hoặc format freeze. Effect key/value là projection fixture, chưa
phải đầy đủ authoritative entity schema. Descriptor giữ cả shared source
envelope để không mất revision/entity/object/payload bindings.

Catalog bind history, installation incarnation và genesis reference. Exact
member fixture nằm ở `tests/fixtures/recovery/export-catalog.json`.
`Inventory.ValidateExact` yêu cầu Closed/history/epoch và sorted unique IDs
khớp toàn bộ independently obtained expected membership. Empty epoch phải có
explicit empty array. Count/digest, partial listing hoặc Open không đủ. Hàm
chỉ kiểm fixture; không sinh closure certificate hoặc xác minh distributed
quiescence. Inventory không phải seal/marker/checkpoint/recovery target.

## Đối chiếu producer

| Producer | Source hiện tại | Evidence / phần còn thiếu |
|---|---|---|
| Objects | CommitObject/DeleteObject atomically ghi outbox và outcome | Test exporter dùng committed object event, overwrite sau đó không thay retained input; effects là fixture, chưa full manifest/receipt/attachment projection |
| SystemConfig | PutPolicy atomically ghi immutable history/current/outbox | Export test qua PutPolicy thật trên fake và Badger; payload mẫu được retain trước publication, chưa runtime payload writer |
| Quyền: Users/Buckets/Grants/Credentials/AdminTokens | Shared EventID/Outbox validator chưa hỗ trợ các projections này | Không synthesize/backfill event từ current state; integration blocker cho recovery đầy đủ |
| Chunks/location | EventID đã nhận Chunks/location_set; Outbox validator và PutChunkIfAbsent/location CAS chưa có atomic event payload | Negative test khóa gap hiện tại; không bỏ validation để giả hỗ trợ |

Existing object/config callers vẫn có thể tạo outbox trỏ tới payload fixture hoặc
digest placeholder. Exporter sẽ từ chối missing/mismatched input. Wiring retained
payload writer/projections vào mọi producer là prerequisite cho runtime exporter,
không được suy ra từ việc metadata outbox đã tồn tại. Không backfill thiếu event
sau authoritative mutation và không claim RPO60s.

## Kiểm thử

- Crash trước/sau từng page/descriptor write, trước progress mark và lost ACK
  sau mark; fake và Badger reopen đều retry giữ exact IDs, không mất DB event.
- Source event mismatch/descriptor mismatch conflict; missing payload, digest
  mismatch, corrupt readback và immutable key collision không mark exported.
- Base enumeration pagination, history isolation, object overwrite và config
  producer; schema byte fixture, duplicate/unknown/version/trailing/size errors;
  exact/empty membership, wrong member và Open epoch reject.
- AWS SDK request stub: STANDARD/If-None-Match/KMS, lost ACK, retry collision,
  read failure, AccessDenied và Cold journal reject.

Lệnh kiểm tra: `go test ./...`, `go vet ./...`, `go build ./cmd/...`,
`python -B -m unittest discover -s scripts/tests -v`,
`python -B scripts/check_doc_names.py`, `gofmt -l cmd internal configs/pricing`.
Go1.27.2/windows-amd64. Badger tests cần quyền rename trong temp directory;
sandbox Windows chặn MANIFEST rename cả baseline tests và Python temp files,
nên chạy cùng suite với quyền file bình thường. Không sửa Badger để né lỗi môi trường.

Kết quả thực chạy 26/10/2026: toàn bộ Go tests pass, vet/build pass,6 Python
tests pass, doc-name và gofmt checks pass. Sau thay đổi cuối về bounded scan,
rerun recovery/Badger/storage tests và vet. Không có live AWS test hoặc race
detector evidence trong lượt này.

Checklist/code evidence được chuẩn bị cho review độc lập; chưa tự gán Done,
Approved, Frozen, production verified hoặc complete recovery cut trên board.
