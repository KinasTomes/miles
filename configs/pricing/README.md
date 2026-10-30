# Pricing contract v1 — W02-HUNG-02

Nguồn thiết kế: [KT v4 §13.1–13.4](../../docs/dedup-storage-gateway-architecture.md), [API v2 §6.3 cost/report](../../docs/dedup-storage-gateway-api.md). Đây là **schema và quote mẫu**, chưa là cost engine, hóa đơn hay phép đo tiết kiệm.

- [pricing.schema.json](pricing.schema.json): JSON Schema Draft2020-12, version1, unknown fields bị reject.
- [pricing.ap-southeast-1.example.json](pricing.ap-southeast-1.example.json): public gross On-Demand Singapore, `as_of=2026-10-19`. Không có account ID/ARN/hostname/credit nội bộ. File thực tế có thể đặt tên `pricing.json` ở cấu hình riêng, cùng schema.
- [Go parser/validator](../../internal/pricing/catalog.go) và [business rules](../../internal/pricing/validate.go): kiểm schema/date/source refs, ID unique, storage classes, tier bounds, gross normalization và mapping Region/location/usagetype/service/unit cho quote API. Dùng `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3` cho Draft 2020-12 với format assertions; bỏ môi trường Python riêng cho pricing. Schema được embed trực tiếp từ file gốc qua [schema.go](schema.go), không giữ bản sao hoặc cần source tree lúc chạy binary.

## Hợp đồng fields

| Field | Ý nghĩa / cách đọc |
|---|---|
| schema_version |1; consumer không hiểu version phải reject, không đoán layout |
| region / as_of / currency / basis |Region áp dụng, ngày snapshot, USD, gross model trước tax/credit/Free Tier; AWS quotes theo public On-Demand, VM ngoài AWS có thể là operator estimate có evidence riêng; không gộp giá khác Region vào cùng catalog |
| sources |Dictionary source ID → official URL, ngày lấy nguồn và loại `aws_price_api`/`official_page`/`operator_estimate`; URL reference không tự chứng minh có giá |
| rates[].id/category/service |ID ổn định, nhóm storage/request/retrieval/restore/transition/transfer/infrastructure, tên service; source field dùng AWS Price List service code khi quote qua API |
| unit |**Giữ nguyên unit của AWS priceDimensions** cho quote API, không tự đổi giá mỗi request thành mỗi1000request. Nếu đổi đơn vị phải quy đổi rõ bằng Decimal/rational và cập nhật provenance |
| status |`verified`: đơn giá input đã được xác minh theo source/assumptions +tiers (operator_estimate vẫn là ước tính, không phải bill đo thật); `unpriced`: chưa chọn/verify rate, bắt buộc tiers=[]; không thay bằng0 |
| tiers[].begin/end/usd |Các decimal không âm lưu **string**, không float; begin inclusive/end exclusive, end=null nghĩa không upper bound; liên tục từ0 tới∞. Đây là tariff quantity range, không phải số byte object hoặc tổng tiền. Consumer phải split usage theo tier và billing aggregation của AWS |
| source_id / dimensions |Nguồn quote và qualifiers: SKU,regionCode,location,usagetype,operation,volumeType khi API có. Price chỉ dùng trong qualifier đó; không suy một SKU là giá của mọi operation cùng service |
| gross_adjustment |Chỉ hỗ trợ `ddb.storage` verified: `method=exclude_free_tier`, `raw_tiers` giữ tariff AWS gốc, `allowance` ghi quantity/unit/scope=`account_region`/source_id. Đây là provenance, **không khấu trừ** khỏi gross tiers; validator yêu cầu một gross tier từ0 dùng đơn giá paid của tariff gốc |
| note |Điều kiện/thiếu thông tin. `unpriced` có thể có unit gợi ý; phải thay bằng unit quote thật trước tính tiền |
| storage_rules |Bốn class bắt buộc: STANDARD,STANDARD_IA,GLACIER_IR,GLACIER; minimum duration/min size và metadata billing. Source ref riêng; không suy duration từ đơn giá |

Byte quantities phải ghi rõ bytes→AWS billable GB conversion theo service, không coi MB/MiB là một. Storage GB-Mo là byte-time với quy ước tháng/proration của dịch vụ; không dùng730giờ như mặc định chung cho mọi phép tính storage. Request group chung phải có mapping operation thực ở consumer, không đoán khi schema chỉ quote một operation. Tiers áp trên phạm vi billing của service/account/Region, không reset free allowance/tier cho từng object/trial.

Giá sample có **50rate IDs:30verified/20unpriced**. Đã lấy S3 Singapore hết pagination (178products), DynamoDB26products và tái dùng public NAT/VPC/ALB quotes cùng ngày từW02-HUNG-01. 29quote giữ nguyên tiers AWS; riêng `ddb.storage` chuẩn hóa gross như dưới đây, không khẳng định gross tiers giống nguyên bản API. Sample chỉ chọn Standard table class, các class/operations thể hiện trong dimensions; không dùng DynamoDB IA/Vector/replicated pricing thay Standard WRU/RRU.

Giá EC2 chưa chọn instance, local VM, egress route và một số API/monitoring/key rates còn unpriced có chủ đích. **W2 hoàn thành schema không nghĩa sample sẵn sàng tính total.** W4–6 chọn workload/runtime, điền quote còn thiếu và require đầy đủ rate IDs trước công bố chi phí. Giữ `basis` gross; nếu sau này tính hóa đơn net thì thêm lớp credits/free allowance có evidence riêng, không sửa gross rate thành0.

## DynamoDB gross và Free Tier

`ddb.storage.tiers` dùng `$0.285/GB-Mo` từ quantity0 cho Singapore. `gross_adjustment.raw_tiers` giữ báo giá API:0–25GB-Mo giá0, trên25 giá0.285; allowance25GB-Mo là Free Tier của Standard table storage, nguồn [AWS DynamoDB pricing](https://aws.amazon.com/dynamodb/pricing/). Gross rate được lấy từ paid tier, không tự nhận trial nhỏ được miễn phí.

Allowance chỉ là metadata theo **account/Region**, không theo table/bucket/stack/trial; chưa xác minh tài khoản đủ điều kiện hay còn bao nhiêu allowance. Consumer gross chỉ dùng `tiers`. Consumer net sau này cần evidence eligibility, usage tổng hợp toàn account/Region và phần allowance còn lại; không áp lại25GB cho từng trial. Validator reject raw Free Tier tiers được chép trở lại `tiers`, thiếu normalization, paid rate sai hoặc allowance không khớp raw tariff. Giá0 có thật như Bulk restore vẫn hợp lệ, không reject mọi quote0.

## Validation và cách cập nhật quote

```bash
go run ./cmd/pricingcheck
go test ./internal/pricing ./cmd/pricingcheck
# Kiểm một workload đã có giá cần thiết:
go run ./cmd/pricingcheck --require-rate network.nat_hour
# Phải fail cho tới khi EC2 instance quote được chọn/verify:
go run ./cmd/pricingcheck --require-rate ec2.compute
```

Hiện validator có mapping Region đã kiểm chứng cho **ap-southeast-1** và30API rate IDs verified trong sample. Region/verified API ID mới bị reject tới khi bổ sung mapping có evidence; không tự suy prefix/location. Mỗi API quote phải khớp `regionCode`, `location`, usage type chính xác theo ID và Region, service/unit kỳ vọng trong `apiRates` ở validator. Thêm test khi mở rộng mapping. Kiểm tra này không gọi AWS trong CI và không thay việc đối chiếu SKU/operation/volumeType với AWS khi refresh.

Cost/simulator consumers dùng chung `pricing.Parse(data, requiredRates...)` từ `internal/pricing`: trả `*Catalog` với structs `Rate`, `Tier`, `Source`, `StorageRule` hoặc lỗi; không trả catalog dùng dở khi validation fail. Decimal được giữ dạng string và so sánh bằng `math/big.Rat`, không qua `float64`. `--file` chọn catalog khác; `--require-rate` lặp được. CLI exit0 khi valid/help,1 khi lỗi file/contract/required rate,2 khi sai cú pháp. Binary embed schema nên dùng được ngoài repo với catalog path rõ ràng.

Cost/simulator consumers dùng cùng contract này. Mỗi workload liệt kê required IDs và từ chối tính **complete total** khi thiếu giá/đơn vị/route, không silently skip; partial subtotal chỉ được báo với danh sách unpriced đi kèm. Parse decimal bằng exact decimal/rational.

Prototype W04-HUNG-03 dùng `internal/cost` và `cmd/costsim` để tính subtotal của usage được cung cấp: cộng theo rate trước khi tách tiers, kiểm unit, giữ provenance và danh sách unpriced; không trừ lại Free Tier. Kết quả exact là string decimal/rational; `complete_total_usd=null` vì chưa chứng minh đủ coverage. [Trace contract, lệnh chạy và giới hạn](../../benchmarks/cost-simulator.md).

Reproduce quotes (read-only; đọc ngày thực trước refresh):

```bash
aws pricing get-products --service-code AmazonS3 --region us-east-1 \
  --filters 'Type=TERM_MATCH,Field=location,Value=Asia Pacific (Singapore)' \
  --max-results 100 --no-cli-pager
```

Nếu response có NextToken, tiếp tục `--next-token` cho tới hết; không chọn giá từ trang đầu rồi kết luận không có SKU. Với mỗi rate, match servicecode+usagetype+operation+volumeType trong dimensions, đọc **mọi OnDemand priceDimension** và giữ unit/begin/end/pricePerUnit. Riêng `ddb.storage`: lưu các tier gốc vào `gross_adjustment.raw_tiers`, kiểm allowance với nguồn official, dùng paid rate từ quantity0 ở gross `tiers`; không bỏ provenance hoặc chép Free Tier trực tiếp vào gross. Kiểm `regionCode` với product attributes, không tự gắn Region chỉ theo tên file. DDB dùng AmazonDynamoDB; NAT AmazonEC2; endpoints AmazonVPC; ALB AWSELB với operation=LoadBalancing:Application. Không copy raw response credentials/private config vào Git. Cập nhật `as_of` và source date đúng lần lấy, review diff các tier/status.

## Accounting bắt buộc ở consumer tương lai

- Dead/retired/orphan Cold pack, minimum billed storage, archive metadata và restored temporary Standard copy đều có chi phí. Control objects luôn Standard, không áp archive rules lên sidecar/journal/checkpoint/catalog.
- Self-COPY đổi class tính theo operation thật; không vừa tính COPY vừa cộng lifecycle transition giả. Early-deletion charge dựa minimum duration còn lại, không double count thời gian storage đã trả.
- Revival thêm storage/PUT/network thực và Cold cũ còn giữ; không cộng một khoản “revival fee” nữa. Restore theo unique pack, gồm request+retrieved GB+temporary storage; expiry extension/retry theo request thực.
- DDB cost theo billed units từng item/read consistency/transaction/rounding; không cộng một “transaction fee” bằng cách nhân lần nữa units đã có factor. PITR1day không suy là đơn giá giảm so với PITR chuẩn.
- Infrastructure gồm compute/disk/ALB/IPv4/NAT/endpoints, KMS/SSM/secrets/CloudWatch/PITR/backups theo workload. Snapshot/EBS IOPS/throughput, alarms, DNS hoặc dịch vụ khác nếu dùng thì bổ sung rate IDs, không bỏ qua vì sample chưa có.
- Transfer là từng route có hướng thực. Cloud cache không giảm logical Internet bytes gateway→client; Edge hit có thể giảm AWS→Edge download khi topology thật. Chi phí VM ngoài AWS cần evidence operator, không lấy URL EC2 trong placeholder làm chứng minh giá VM.

Tham khảo [S3 pricing](https://aws.amazon.com/s3/pricing/) và [GetProducts API](https://docs.aws.amazon.com/aws-cost-management/latest/APIReference/API_pricing_GetProducts.html); từng quote giữ provenance trong JSON.
