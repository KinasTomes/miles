# Evidence W02-HUNG-02 và chuyển pricing sang Go

Ngày kiểm tra bản Go: **2026-10-20 (UTC+7)**. Nhánh `refactor/pricing-validator-go`, từ `origin/main` tại `58681c4` sau khi [PR #8](https://github.com/KinasTomes/miles/pull/8) merge. Bản ghi này là evidence local; CI/review hiện tại xem trên PR tương ứng, không tự đánh task Done.

## Pricing Go — kết quả local

| Kiểm tra | Kết quả quan sát | Giới hạn |
|---|---|---|
| `go test ./internal/pricing ./cmd/pricingcheck` |Pass:10 test functions của parser/validator và1 test function CLI với các subcases |Chuyển đầy đủ8nhóm kiểm tra Python trước đây; thêm invalid/trailing JSON, schema required/nested fields/format, tier gap/overlap/open end, exact decimal và CLI exit codes |
| `make check` |Pass: fmt, go vet, go test ./..., build miles/dsgctl |Generator tests/golden fixture vẫn pass; không gọi AWS hoặc chạy gateway/benchmark |
| `go run ./cmd/pricingcheck --require-rate network.nat_hour --require-rate ddb.storage` |Pass:50rate IDs,30verified/20unpriced |Required rate thiếu hoặc unpriced phải fail; không tính tổng chi phí |
| Standalone binary |`go build -o /tmp/miles-pricing-go.*/pricingcheck ./cmd/pricingcheck`; chạy từ thư mục `/tmp` với `--file` absolute và hai required rates trên:pass |Schema embed từ file gốc; binary không cần checkout/schema file/Python lúc chạy; CLI mặc định vẫn tìm catalog tương đối với working directory |
| `go mod verify` |Pass |Pin `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3` và checksums; không là kiểm chứng giá AWS |
| Diff schema và catalog so với main |Không đổi `pricing.schema.json` hoặc `pricing.ap-southeast-1.example.json` |Không refresh hoặc xác minh lại giá AWS trong lần chuyển ngôn ngữ |

`pricing.Parse` kiểm Draft2020-12 với format assertions trước business rules và trả typed catalog hoặc lỗi. Giá/range vẫn là decimal string; so sánh bằng `math/big.Rat`, không qua float. Các test kiểm cả chênh lệch nhỏ hơn độ chính xác float64 tại quantity lớn. Schema có một bản gốc trong configs/pricing, embed bằng Go; không chép schema vào package khác.

Python validator/tests/requirements riêng của pricing đã được thay bằng Go. CI giữ tên required job `benchmark-contracts`, chạy focused Go tests và command pricingcheck; `check` chạy Go tests toàn module. Python phục vụ Terraform/client compatibility ở nơi khác giữ nguyên.

## Evidence W2 ban đầu (2026-10-19, PR #8)

- Smoke dataset: Go1.27.2, profile=smoke, seed=miles-w02-v1,9files,589914bytes; manifest/checksum đã sinh thật. Không commit payload binaries, không sinh backup30 hoặc đo hiệu năng trên máy cá nhân.
- Public quote input: S3 Singapore178products qua đủ pages; DynamoDB26products; NAT/VPC/ALB quotes cùng ngày.30quote đã đối chiếu response AWS:29giữ tiers gốc;1DynamoDB storage giữ raw tariff và chuẩn hóa gross. Đây là evidence ngày19/10, không là lần lấy giá mới ngày20/10.
- Baseline spec xác định direct-S3 là chính, chunk-per-object là phụ; Cloud/Edge có topology và accounting riêng. Chưa có completed benchmark/harness/simulator hoặc số đo tiết kiệm.

## Invariants và giới hạn còn giữ

- DynamoDB storage gross dùng paid rate từ0; Free Tier25GB-Mo giữ riêng với scope account/Region, không áp per trial/stack hoặc khẳng định eligibility của tài khoản. Giá0 theo quote thật như Bulk restore vẫn hợp lệ.
- Validator reject Region chưa có mapping, quote sai regionCode/location/usagetype/service/unit, source/date không hợp lệ, duplicate IDs/classes, tier discontinuity và gross normalization không nhất quán. Mapping hiện chỉ hỗ trợ Singapore.
-20rate `unpriced` vẫn là đầu vào thiếu cho costing; không thay bằng0 hoặc công bố full total khi workload cần chúng.
- Bốn storage classes/rules, dataset generator/seed/hash, baseline và quyết định R1–R4 giữ nguyên. Không triển khai AWS, đổi backlog/report/apps, chạy benchmark hoặc tự công nhận target đã đạt.
