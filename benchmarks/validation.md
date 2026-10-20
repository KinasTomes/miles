# Evidence W02-HUNG-02

Ngày ghi nhận:2026-10-19 (UTC+7). Nhánh:`feat/w02-hung-02-pricing-dataset-baseline`, từ `origin/main` tại `5abf19d` (W02-HUNG-01 đã merge). Đây là bản chờ lead review, không tự đánh task Done, evidence dưới đây là kiểm tra local trước PR; trạng thái CI/review xem trực tiếp trên PR.

| Kiểm tra | Kết quả quan sát | Giới hạn |
|---|---|---|
| `make check` |Pass: fmt,go vet,go test ./...,build miles/dsgctl |Không gọi AWS hoặc chứng minh gateway feature mới chạy |
| Generator tests |3test pass: deterministic/seed/bytes/hash/gzip/change-window; reject overwrite/invalid input; golden smoke manifest |Smoke nhỏ, không sinh backup30 và không đo throughput/dedup/cache |
| Giá example/schema |Pass với jsonschema4.26.0; 50rate IDs,30verified/20unpriced |Partial quote catalog, không đủ full total khi workload dùng unpriced IDs |
| Pricing negative tests |8test pass, bao gồm gross/Free Tier provenance và Region/location/usagetype/service/unit negatives cho30API rates, cùng subcases date/unknown field/negative float or string/status-tier mismatch, duplicate/source/tier integrity, missing required price |Đây là contract validator, không phải phép tính chi phí hay proof giá future vẫn đúng |
| S3 public quote source |Đã đọc178products qua đủ regional pages; DDB26products, network/ALB quotes cùng ngày |30quote đối chiếu cached AWS response cùng ngày:29giữ tiers gốc;1DDB storage giữ raw tiers và chuẩn hóa gross. Không suy20mục còn lại đã verified |
| Smoke command |Go1.27.2,profile=smoke,seed=miles-w02-v1:9files,589914bytes; manifest đã copy vào repo |Checksum input thật; không có performance measurements hoặc AWS storage usage |

Tool JSONSchema được cài riêng ngoài repo, không thay Python môi trường app. Data smoke ở thư mục `/tmp/miles-w02-datasets.*/smoke`, không nằm trong repo/Git; generation không chạy gateway/container/cache hoặc workload tải lớn trên máy cá nhân.

## Self-review

- Giá tiền/quantity range là decimal string và kiểm bằng Decimal khi validate tiers; không dùng float để tính giá. `unpriced` phải tiers rỗng và required-rate check phải fail; giá0 chỉ hợp lệ khi có verified quote thật (ví dụ Bulk restore có rate0 theo source).
- Bốn storage classes bắt buộc có rules minimum duration/size/archive overhead và source. DeepArchive/UploadPartCopy không được nâng thành bắt buộc.
- Gzip-identical copies là byte-identical trên stream đã tạo một lần, random-distinct dùng seed/version domain khác. Low-entropy thay đúng floor(size/100)byte và giữ phần còn lại; không bịa dedup ratio từ tỷ lệ này. Golden manifest gắn generator/Go version.
- Baseline chính directS3, phụ chunk-per-object. Cloud/Edge khác topology/cost và EC2 Edge-config không tự chứng minh on-prem egress saving. Single-PUT throughput tách multipart, cache cold/warm và raw/error samples giữ riêng.
- Chi phí controls/async/retry/deadCold/temporaryrestore/network/infra/retention không được bỏ qua hoặc double-count. Giá/group hiện thiếu phải điền sau chọn workload trước cost/simulator total, không dùng0 để báo tiết kiệm.
- Source fixture/chunking/policy/SDK không thay bằng cấu hình giả. Mọi target performance là provisional; W3 preliminary, W5–6 pilot đầy đủ, W7 official theo thiết kế.
- Chưa có estimator/harness/simulator chạy thật hoặc phiên benchmark; đây là phạm vi những task sau. Kết quả local không thay cho CI run GitHub hoặc review approval; kiểm trạng thái thực trên PR.

## Sửa sau review

- Gross DynamoDB storage dùng paid rate từ0, Free Tier25GB-Mo giữ riêng trong provenance với scope account/Region; không áp per trial/stack và không khẳng định eligibility của tài khoản.
- Validator reject Region chưa có mapping, quote sai regionCode/location/usagetype/service/unit và normalization không nhất quán. Hiện mapping chỉ hỗ trợ Singapore; mở Region mới phải thêm mapping + test trước.
-20rate `unpriced` vẫn là đầu vào còn thiếu cho costing, không thay bằng0. Chưa có total-cost engine, benchmark thật hay AWS apply trong task này.
