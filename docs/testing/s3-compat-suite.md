# S3 compatibility suite trước freeze

Producer task: W02-CHIEN-03. Consumers: W03-CHIEN-02 (PUT/GET/HEAD/Range),
W03/W04 handler và adapter/integration tests, W04-CHIEN-01 (Admin/dsgctl).
Contract và source proof: [dedup-storage-gateway-api.md](../dedup-storage-gateway-api.md)
mục 2.2.2, 2.4, 2.7, 4.1, 4.2 và 11; quyết định chi tiết chưa ghi trong API
nằm ở [ADR-0001](../adr/0001-s3-compat-freeze-decisions.md).

## Fixture layout

`tests/fixtures/s3/compat.json` là ma trận tương thích pre-freeze, mỗi nhóm
ghi rõ section API v2 mà nó pin ở `sections`:

| Nhóm | Pin gì | Chạy thế nào hiện tại |
|---|---|---|
| `keys` | Path decode đúng một lần: `+` literal, `//`, `..`, trailing `/`, NFC/NFD là hai key, byte không UTF-8 giữ nguyên; query decode một lần; key trong XML error escape đúng, byte hỏng thành U+FFFD | Qua `Resolve` và `WriteError` thật |
| `empty` | Empty PUT (aws-chunked của CLI 2.36.41 và plain Content-Length 0) decode 0 byte, lưu CRC64NVME `AAAAAAAAAAA=`, ETag MD5 rỗng; GET object rỗng 200 length 0; mọi Range trên object rỗng 416 | PUT qua `Router`/`Payload` thật (case `from` tham chiếu `transport.json`); GET/Range qua tham chiếu `parseRange` |
| `range` | `bytes=a-b`, `bytes=a-`, `bytes=-n`; end clamp về size-1; suffix `-0`, start ≥ size, non-digit, khoảng trắng, multi-range, tràn số, unit khác bytes | Qua tham chiếu `parseRange` từ API v2 4.1 |
| `head` | Response HEAD có header ETag/Last-Modified/Content-Length/Accept-Ranges, không body kể cả lỗi; checksum mode ENABLED/absent theo 2.7; `?partNumber` và `If-Match` trả 501 | Row có `request` qua `Resolve` thật; phần còn lại qua tham chiếu `headChecksumHeaders` và `WriteError` |
| `xml` | Body XML đúng một document: DOCTYPE/two roots/text ngoài root/declaration giữa document/root sai/truncated/empty → `MalformedXML`; quá limit → `EntityTooLarge` | Qua `Payload.DecodeXML` thật |

Mọi row mong đợi lỗi đều pin kèm HTTP status; `checkStatusPin` đối chiếu
status đó với bảng lỗi API v2 2.4 (`errorCodes`, cũng là bảng mà `errors.json`
pin), nên fixture – code – document không thể lệch nhau trước freeze.

## Bàn giao cho adapter/integration tests

- **W03-CHIEN-02 (PUT/GET/HEAD/Range):** thay `parseRange` và
  `headChecksumHeaders` trong `internal/api/s3/compat_test.go` bằng chính
  handler; các row `range`/`head`/`empty` (GET) phải pass nguyên vẹn —
  cùng cách `authorization.json` và `quota.json` đã thay hàm tham chiếu ở
  `contract_test.go` sau W02-CHIEN-02. Handler Range phải trả đúng
  206/200/416 và `Content-Range` như row pin.
- **Handler XML (DeleteObjects, CreateBucket):** thêm target struct theo op
  và dùng lại nhóm `xml`; parser `DecodeXML` đã là của chung.
- **Integration tests (W03+):** request thật của client đã pin nằm ở
  `transport.json` (kèm `canonical_request` để kiểm lại chữ ký độc lập);
  `compat.json` chỉ chứa contract response/error, không trùng lặp.

Sinh lại fixture khi đổi version client (cần AWS CLI 2.36.41, boto3 pin ở
`scripts/s3capture/requirements.txt`, curl):

```sh
uv run --no-project --python 3.13 --with-requirements scripts/s3capture/requirements.txt -- go run ./scripts/s3capture
```

`compat.json` viết tay theo API v2; khi API đổi, đổi fixture trong cùng PR
và ghi lý do vào ADR.

## Fuzz

`internal/api/s3/fuzz_test.go` có bốn target trên bề mặt parser trước handler:
`FuzzResolve`, `FuzzSigV4` (parse + canonical request), `FuzzAWSChunked`
(framing, chữ ký chunk/trailer, trailer corruption đủ ba mode STREAMING-) và
`FuzzDecodeXML`. Seed là request/body thật của client đã pin trong
`transport.json`, `routes.json`, `compat.json`, `multipart.json`; corpus
chạy trong mọi `go test`. Chạy fuzz bounded:

```sh
make fuzz                # mỗi target 30s
FUZZTIME=120s make fuzz  # hoặc lâu hơn
```

Quy ước ghi lỗi thật: một crasher tìm được qua fuzz được minimize, thêm vào
`compat.json`/`transport.json` làm row regression kèm lỗi thực tế của parser
trong `note`, rồi mới sửa code. Không có crasher thì ghi số executions vào
bằng chứng PR như dưới.

## Kết quả 25/10/2026 (W02-CHIEN-03)

Local Windows, Go 1.27.2, seed corpus pass trong `make check`. Fuzz từng
target: `FuzzResolve` 1.115.398 execs/30 s, `FuzzSigV4` 1.527.259 execs/30 s,
`FuzzDecodeXML` 1.212.276 execs/30 s — không lỗi. `FuzzAWSChunked` tìm thấy
một crasher thật sau ~2 s:

- **Input:** body `"0"`, mode `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`,
  `x-amz-decoded-content-length` 75, `x-amz-trailer: 0`.
- **Lỗi thật:** `newPayload` lấy thuật toán checksum từ `x-amz-trailer` rồi
  gọi `checksumHashes[alg]()`; thuật toán lạ cho map entry nil → panic
  nil-pointer (`payload.go`). Qua `Router` thì `Resolve` chặn trailer lạ
  trước đó, nên lỗi chỉ bộc lộ khi gọi payload layer không qua router —
  đúng cấu trúc mọi handler/integration test sau này dựng `Payload` riêng.
- **Sửa:** `newPayload` và `checksumDigest` kiểm thuật toán trong bảng trước
  khi dùng; thuật toán lạ thành lỗi sticky `InvalidRequest` "checksum
  algorithm X is not supported" đúng bảng 2.4, hai case unit đi kèm ở
  `TestChecksumHeaderValue`. Crasher minimize giữ làm seed regression ở
  `internal/api/s3/testdata/fuzz/FuzzAWSChunked/`. Sau sửa, target chạy
  115.004 execs/60 s sạch.

Giới hạn: fuzz local chưa chạy dài, chưa chạy trong CI (mỗi PR chỉ chạy seed
corpus kèm crasher regression), và các row tham chiếu chỉ chứng minh hợp đồng
nội tại của fixture – bằng chứng hành vi thật của handler thuộc task W03.
