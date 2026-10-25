# ADR-0001: Quyết định tương thích S3 trước freeze API

Task: W02-CHIEN-03 · Ngày: 25/10/2026 · Trạng thái: đề xuất để review trước
freeze (cuối W02); bản 2 sửa theo review PR #29 (key UTF-8, HEAD Range theo
S3, range unit không phân biệt hoa/thường). Ma trận tương thích:
[s3-compat-suite.md](../testing/s3-compat-suite.md), fixture
`tests/fixtures/s3/compat.json`.

## Bối cảnh

API v2 §4.1/§4.2 định nghĩa GetObject/HeadObject ở mức "một khoảng
bytes=a-b/a-/-n, invalid trả 416", "HEAD không body kể cả lỗi". Bộ fixture
freeze cần chốt từng trường hợp biên mà document chưa ghi, để handler W03
(W03-CHIEN-02) và integration tests không phải đoán. Các quyết định dưới đây
được pin trong `compat.json` và kiểm bằng hàm tham chiếu `parseRange`,
`headResponse` (`internal/api/s3/compat_test.go`).

## Quyết định

1. **Range** (§4.1):
   - Range unit **không phân biệt hoa/thường** như RFC 9110 §14.1
     (`Bytes=0-1` là byte range hợp lệ, 206); unit thực sự khác `bytes`
     (ví dụ `items=0-1`) bị **bỏ qua**, trả 200 cả object theo RFC 9110
     §14.2 (origin server MUST ignore unrecognized units).
   - Đúng **một** byte range trong ba dạng `bytes=a-b`, `bytes=a-`,
     `bytes=-n`; multi-range trả 416 như API đã ghi.
   - `bytes=-0`, start ≥ size, start > end, token không phải chữ số (kể cả
     `+1`), khoảng trắng bên trong, số tràn int64: **416 InvalidRange**.
   - End quá size được **kẹp về size-1** (206, không 416) — hành vi S3 thật.
   - Suffix dài hơn object trả cả object; suffix/dạng nào trên **object rỗng
     đều 416** (§4.1 đã ghi, fixture pin từng dạng).
   - `Content-Range: bytes <start>-<end>/<size>`; không range hoặc bị bỏ qua
     → 200 không `Content-Range`.
2. **HEAD** (§4.2): HEAD nhận `Range` như HeadObject của S3 — khoảng thỏa mãn
   trả **200** (không 206) với `Content-Length` của khoảng được chọn, khoảng
   không giao object trả **416**, và response luôn không body kể cả lỗi;
   `If-Match` ở HEAD thuộc P2 nên `Resolve` trả 501 như hiện nay; checksum
   headers theo §2.7 (ENABLED → checksum + type, absent → không bắt buộc).
3. **Empty object** (§4.1, §2.7): PUT rỗng (kể cả aws-chunked 0 byte của CLI)
   lưu checksum CRC64NVME của chuỗi rỗng (`AAAAAAAAAAA=`), ETag là MD5 rỗng;
   GET trả 200 length 0.
4. **Key** (§3, §2.5): key **phải là UTF-8 hợp lệ** sau khi decode path đúng
   một lần — S3 định nghĩa key là UTF-8 và metadata (canonical JSON, Badger)
   không giữ được byte lỗi, nên `Resolve` trả **400 InvalidArgument** trước
   mọi ghi durable (byte lỗi trong XML error vẫn hiển thị thành U+FFFD theo
   §2.4). Unicode giữ nguyên, **không chuẩn hóa NFC/NFD**; không mở rộng
   metadata/canonical JSON để lưu key nhị phân.
5. **XML** (§2.2.2): body phải đúng một document; DOCTYPE/directive bị từ chối
   ở mọi vị trí (chặn entity expansion); namespace/attribute của root không
   đổi tên local — một document có namespace prefix vẫn hợp lệ nếu local name
   đúng; khai báo XML chỉ được đứng đầu; quá limit → `EntityTooLarge`.

## Đánh đổi

- Bỏ qua unit lạ làm gateway khác với một số validator nghiêm ngặt, nhưng
  khớp RFC và S3; fixture pin rõ để review một lần trước freeze.
- Từ chối key không UTF-8 thu hẹp bề mặt so với bản 1 (giữ byte thô); chọn
  theo S3 và vì metadata không thể lưu key nhị phân — một request như vậy
  không bao giờ đến giai đoạn ghi durable.
- HEAD trả 200 với Content-Length của khoảng (không 206) theo đúng
  HeadObject của S3; client cần bytes phải dùng GET.
- Chấp nhận namespace prefix làm bề mặt parser rộng hơn một chút; đổi lại
  tương thích với client gửi namespace mặc định (CLI/boto3 đều gửi xmlns).
- Kẹp end thay vì 416 theo dõi S3 thật; nếu review muốn nghiêm ngặt hơn thì
  chỉ đổi một row fixture và hàm tham chiếu.

## Tác động

- W03-CHIEN-02 triển khai handler Range/HEAD theo các row `range`/`head`/
  `empty` của `compat.json`; test handler thay hàm tham chiếu.
- API v2 §4.1/§4.2 có ghi chú "Hiện thực (W02-CHIEN-03)" trỏ về ADR này.
- Đổi từ bản 1 (PR #29 review): bỏ "Range trên HEAD bị bỏ qua" và bỏ "key
  không bắt buộc UTF-8".
