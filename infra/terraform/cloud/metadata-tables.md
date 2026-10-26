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
Go key helpers có test đối chiếu đủ 20 bảng với inventory Terraform. Baseline
W4 đề xuất không GSI/TTL; record phụ dùng namespace SystemConfig có record_kind,
cần review cùng metadata/R2 trước enablement, không thêm thuộc tính vào page v1.
Review mapping này cùng adapter trước apply, không đổi encoding Badger/pack/pages
hoặc tự thêm GSI/TTL khi các task W2 chưa hoàn tất. GSI chỉ tìm candidates, không là
bằng chứng authorization/commit; TTL không xóa lease/outbox/reservations đang sống.

Mỗi bảng dùng on-demand, giới hạn dev 10 read/10 write request units mỗi giây,
SSE-KMS và PITR 1 ngày theo kiến trúc. Hết throughput sẽ throttle; đây không phải
cấu hình benchmark. PITR 1 ngày không rẻ hơn PITR 35 ngày trên cùng lượng dữ liệu;
[AWS tính theo dung lượng](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Point-in-time-recovery.html).
Deletion protection và Terraform prevent_destroy đều bật. Không seed Users,
Credentials, SystemConfig, token hoặc recovery catalog bằng Terraform.
