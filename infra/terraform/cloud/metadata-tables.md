# Schema vật lý DynamoDB dev

Nguồn logical keys: kiến trúc §10.1 và `internal/storage/model.go`.
[Module data](modules/data/main.tf) là inventory vật lý cho 20 bảng `miles-dev-*`.
Hash keys đều là String. Sort keys `page_no` và `part_number` là Number;
`user_id`, `key`, `generation`, `lease_id` là String.

Hai tên thuộc tính làm rõ composite key trong bảng kiến trúc:

| Bảng | Thuộc tính PK vật lý | Giá trị |
|---|---|---|
| Chunks | `chunk_id` | `domain_id#hash`, theo ERD và encoding domain/hash đã chốt |
| RecoveryEpochs | `history_epoch_key` | `history_id#epoch_id`, đầy đủ history và epoch |

`Packs.generation` là String, khớp `PackRef.Generation`, không ép ID hex thành
Number. Không dùng riêng `domain_id` làm partition key của Chunks.

Các tên thuộc tính này là đầu vào bàn giao cho adapter DynamoDB W4. Chưa có adapter
DynamoDB production trong checkout; thay schema sau khi có dữ liệu sẽ cần migration.
W03-HAI-03 bổ sung [mapping adapter/record phụ, index và transaction](../../../docs/contracts/dynamodb-adapter.md).
Baseline W04 đã được lead duyệt tại
[PR #35](https://github.com/KinasTomes/miles/pull/35#issuecomment-6436239260):
giữ đúng **20 base tables, không GSI/LSI/TTL**. Go regression tests đối chiếu
count/PK/SK và kiểm tất cả file `.tf` của module data để chặn index/TTL cả dạng
dynamic, resource bảng phát sinh hoặc đổi nguồn `for_each = local.tables`.
Inventory hiện khớp nên PR này không sửa Terraform resources hoặc apply AWS.

Record phụ dùng namespace SystemConfig và `record_kind` phân biệt với global-policy;
không export operation/manifest-owner như policy changes, không thêm thuộc tính
vào page v1. Strongly consistent base reads và conditional transactions giữ
correctness; bounded Scan chỉ discovery candidates, không chứng minh authorization
hoặc epoch completeness. Cleanup explicit theo roots/retention, không dựa vào TTL.
Consumer sign-off và implementation/fault proofs vẫn là gate riêng trước enablement.
Mọi thay đổi table/index/TTL tương lai cần reviewed migration và cập nhật guard;
baseline này không cấm tối ưu sau khi đã review. Không đổi encoding Badger/pack/pages.

Mỗi bảng dùng on-demand, giới hạn dev 10 read/10 write request units mỗi giây,
SSE-KMS và PITR 1 ngày theo kiến trúc. Hết throughput sẽ throttle; đây không phải
cấu hình benchmark. PITR 1 ngày không rẻ hơn PITR 35 ngày trên cùng lượng dữ liệu;
[AWS tính theo dung lượng](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Point-in-time-recovery.html).
Deletion protection và Terraform prevent_destroy đều bật. Không seed Users,
Credentials, SystemConfig, token hoặc recovery catalog bằng Terraform.
