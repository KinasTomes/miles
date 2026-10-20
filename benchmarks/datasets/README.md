# Dataset v1 — W02-HUNG-02

Dữ liệu synthetic phục vụ [baseline specification](../baseline.md); không dùng dữ liệu cá nhân/production và không chứng minh dedup/throughput đã đạt target.

| Nhóm | Cách sinh | Điều cần đánh giá sau này |
|---|---|---|
| low-entropy |Log text lặp từ seed; mỗi phiên bản thay một cửa sổ đầu dài floor(raw_bytes/100), các byte còn lại giữ nguyên |Ảnh hưởng nén/dedup của backup thay đổi nhỏ; đây là synthetic, không đại diện mọi DB dump/VM image |
| compressed-identical |AES-CTR pseudorandom deterministic → gzip BestSpeed một lần; copy **nguyên gzip bytes** cho mọi phiên bản |Byte giống nhau dù đã nén; không hứa gzip làm random nhỏ hơn, không tái nén riêng mỗi upload |
| random-distinct |AES-CTR từ SHA256(seed+NUL+label chứa version), zero IV, mỗi version label khác |Đối chứng entropy cao/byte khác nhau; không khẳng định không có bất kỳ chunk trùng trước khi đo |

AES/gzip chỉ để tạo dữ liệu thử tái hiện, không là mã hóa client hay feature gateway. Header gzip không timestamp/name và OS=255. Byte stream gzip phụ thuộc implementation Go; manifest ghi Go version và generator version. Pin Go1.27.2 của repo để tái tạo golden fixture; đổi generator/codec/toolchain cần review hash mới, không tự ghi đè fixture.

## Profiles và giới hạn

| Profile | Phiên bản mỗi nhóm | Raw input mỗi phiên bản | Mục đích |
|---|---:|---:|---|
| smoke |3 |65536bytes |9file nhỏ để kiểm generator/manifest, **không đủ nghiệm thu performance hoặc FastCDC avg1MiB** |
| backup30 |30 |8388608bytes |Bộ backup tổng hợp cho harness tuần sau; gzip có overhead nên kích thước file .gz lấy từmanifest, không mặc định8MiB |

Không sinh backup30 hoặc chạy workload benchmark trên máy làm việc cá nhân. Chỉ code/unit tests và smoke nhỏ chạy local; bộ lớn sinh trên EC2 test riêng theo kế hoạch. W6 có thể phải bổ sung scale/dataset thực phù hợp trước chốt benchmark, không biến kích thước smoke thành ngưỡng nghiệm thu.

## Lệnh tái tạo

Chạy từ repo root; parent output phải tồn tại, thư mục output **chưa được tồn tại**:

```bash
go run ./cmd/datasetgen --profile smoke --seed miles-w02-v1 --out /tmp/miles-dataset-smoke
# Chỉ trên host test riêng:
go run ./cmd/datasetgen --profile backup30 --seed miles-w02-v1 --out /tmp/miles-dataset-backup30
```

Generator từ chối output đã có, profile sai hoặc seed rỗng, không overwrite thư mục/file cá nhân, không gọi AWS. File và manifest nằm dưới output riêng; không commit data .bin/.gz. Trên failure chỉ dọn thư mục vừa do generator tạo, không dọn đường dẫn có sẵn.

[smoke-manifest.json](smoke-manifest.json) là manifest **đã sinh thật** từ command smoke cùng seed/toolchain:9files,589914bytes. Không kèm data files vào Git. Đây là checksum/byte count input, không phải số đo DSG. Unit test tái tạo rồi so fixture, kiểm cả filehash/size, gzip round-trip, nhóm copies byte-identical, random versions khác, seed tác động dữ liệu và bảo vệ output hiện có.

## Hợp đồng manifest.json

`schema_version=1`, `generator`, `go_version`, `profile`, `seed`, `versions`, `raw_bytes_per_version`, `total_bytes`, và `files[]`. Mỗi file có `path` relative với manifest directory, `group`, `version` (0-based), `bytes` actual object payload và `sha256` lowercase trên **toàn bộ file client sẽ gửi**, kể cả gzip header/trailer.

Không có absolute path/hostname/account/timestamp khiến nội dung thay đổi giữa lần chạy. `total_bytes` là tổng filebytes một bản dataset, không là live DSG storage, wire bytes, billable size hoặc restored bytes. Harness phải kiểm hash trước upload và sau download; không dùng ETag multipart/copy như whole-file hash. Upload đúng filebytes, không decode gzip ở client; Content-Encoding không dùng để tự giải nén trong phép hash.

Một file có thể được upload dưới key/version/owner khác nhau; manifest input không nhân đôi bytes lưu local. Workload mapping nằm trong baseline, không coi `versions` là số bucket hoặc số bản backup lịch sử đã thực sự chạy.
