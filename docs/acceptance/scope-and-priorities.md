# Phạm vi, ưu tiên và truy vết milestone

**Trạng thái: bản dự thảo để lead review, chưa phê duyệt.** Đầu ra của W01-HUNG-02 (A01), phụ thuộc W01-HUNG-01. Không tự đóng task, không xác nhận feature đã triển khai, CI đã chạy hay nghiệm thu đã đạt. Các tuần là mốc dự kiến, không phải kết quả.

Nguồn hợp đồng: [kiến trúc v4](../dedup-storage-gateway-architecture.md) (KT) và [API v2](../dedup-storage-gateway-api.md) (API). Tài liệu này chỉ suy ra kế hoạch nghiệm thu; khi có khác biệt phải review nguồn, không tự thay hợp đồng. Backlog local được đọc để ánh xạ người/tuần, không phải dependency của tài liệu được commit.

[Use case](use-case-matrix.md) · [Definition of Done](definition-of-done.md)

## 1. Thứ tự nguồn và cách đọc

API §4.1 quyết định nhãn operation P0/P1/P2; KT §15.1 quyết định chức năng hệ thống bắt buộc/tùy chọn/không làm. **Bắt buộc hệ thống không tự biến thành API P0.** Mốc triển khai lấy KT §15.2/API §12 và backlog phân công; một prototype sớm không hoàn thành feature. Thay scope/default phải review và cập nhật nguồn đồng bộ, không chỉ sửa tài liệu suy ra này.

W1 chỉ bàn giao hợp đồng/use case/scope/DoD. W3 nghiệm thu MVP dev; W4–6 hoàn thiện bắt buộc; W7 xác minh đầy đủ; W8 bàn giao. Tất cả mốc dưới đây đang là kế hoạch.

## 2. Phạm vi operation và hệ thống

| Mức | Nội dung bắt buộc / giới hạn | Mốc, owner | Use case / nguồn |
|---|---|---|---|
| P0 — 8 operation | CreateBucket, HeadBucket, ListBuckets, PutObject, GetObject (+ single Range), HeadObject, DeleteObject, ListObjectsV2 | W3 dev một node, Chiến; Hải/Phong backend/read path | UC-04–06/09/11; API §4.1–4.2 |
| Auth/checksum của P0 | Owner authorization, default immutable bucket domain; SigV4 header, toàn bộ aws-chunked signing modes API §2.2; Content-MD5, payload SHA và checksum matrix; XML lỗi/HEAD không body | W1–2 fixture/parser, W3 tích hợp; Chiến/Hải | UC-05/08/12; API §2.2–2.7, §3 |
| P1 multipart | CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts; receipt, frozen input/snapshot, takeover, checksum/binding/quota | W4; W5 fault/sweeper, W7 acceptance; Chiến/Hải/Phong | UC-13–15; API §4.3 |
| P1 S3 khác | DeleteObjects, CopyObject, RestoreObject, DeleteBucket, GetBucketLocation, Presigned GET/PUT, **GetObject + If-Match** (412 nếu sai ETag) | Copy nền/Delete/bucket W4; presigned/If-Match compatibility W5; Cold/cross-domain W6; W7 full; Chiến | UC-10–11/16–19/21; API §4.1, §4.4–4.7 |
| P1 Admin + dsgctl | Mọi method của API §6.2: users/usage, buckets/grants, global/bucket policy, stats/layout, packs/restore/promote, jobs (gồm manual `scrub` on-demand có verify và coverage thực), cache, cost, credentials, admin-tokens, nodes/drain, faults; CLI và offline bootstrap/rebuild/sim ở §7 | W4 identity/policy/stats; W5 packs/jobs/cache/nodes/rebuild; W6 lifecycle/cost/fault/sim; từng owner ở bảng milestone | UC-20–30; API §6–7 |
| Bắt buộc hệ thống | FastCDC/SHA-256, domain-aware Bloom/LRU, zstd/raw, pack 32–64 MiB/sidecar/prefix, paged manifest, BadgerDB dev + DynamoDB, hot LRU/read-ahead, global policy, R1–R4, GC/Warm compaction, hai Glacier, Cloud+Edge, Terraform, health/metrics/CloudWatch, simulator/cost/benchmark | W2 freeze format, W3 Warm, W4–6 tích hợp, W7 acceptance; cả nhóm | UC-02–03/12/20–30; KT §3–13, §15.1; API §8 |
| P2 API | ListObjects v1; ListMultipartUploads; **UploadPartCopy**; conditionals còn lại (If-Match ngoài GetObject, If-None-Match, If-Modified-Since, If-Unmodified-Since, x-amz-copy-source-if-*); GetObjectAttributes, GET/HEAD partNumber, response overrides; đổi algorithm/type checksum khi Copy; checksum algorithm ngoài matrix bắt buộc | Không có deadline bắt buộc; chỉ mở sau phê duyệt có ghi nhận, không lấy thời gian sửa integrity/recovery/multipart | API §2.7, §4.1, §4.5, §4.7 |
| Tùy chọn hệ thống | Consistent hashing/gRPC cache, replica cache Edge, mã hóa phía client theo KT §15.1, full automated scrubber, Deep Archive, so sánh chi tiết Storage Gateway | Sau phần bắt buộc ổn định; không là dependency P0/P1; cần review phạm vi trước nhận | KT §11.1, §12, §15.1; API §9 |
| Ngoài phạm vi | S3 Versioning, ACL, bucket policy, Lifecycle API, Tagging, Object Lock, SelectObjectContent, event notification, replication đa vùng, client-side dedup; Cold compaction, write-back | Không triển khai; API không hỗ trợ trả NotImplemented theo contract, không silently ignore; nội bộ DSG grants/global policy vẫn bắt buộc | KT §1.3, §5.1, §15.1; API §3, §4.1 |

**Matrix checksum bắt buộc (API §2.7):** PUT hỗ trợ CRC64NVME, CRC32/CRC32C, SHA1/SHA256. Multipart FULL_OBJECT: CRC64NVME/CRC32/CRC32C; COMPOSITE: CRC32/CRC32C/SHA1/SHA256; SHA không FULL_OBJECT và CRC64 không COMPOSITE. Không chọn algorithm thì CRC64NVME FULL_OBJECT. Part binary digest + raw length được verify/lưu; checksum metadata không thay SHA-256 chunk. GET toàn object/HEAD checksum-mode trả descriptor đã lưu; Range không gắn full checksum như checksum range.

**Compatibility không vượt scope:** CopyObject đơn ≤5 GiB. High-level cp/mv khi size ≥ threshold có thể gọi GetObjectTagging/UploadPartCopy chưa hỗ trợ. Dùng profile threshold > size hoặc s3api copy-object trong cap; >5 GiB download + multipart reupload, tính bandwidth và restore nếu cần. Move không atomic. GET file lớn dùng Range + If-Match P1, không được đánh đồng với UploadPartCopy P2. API §4.1/4.5/11 là nguồn chi tiết.

## 3. Bất biến không được hạ cấp

Các ID INV dùng làm khóa tham chiếu; kiểm chứng cụ thể ở DoD L2, không tạo bộ trạng thái nghiệm thu thứ hai.

| ID | Yêu cầu giữ nguyên | Kiểm chứng chính / nguồn |
|---|---|---|
| INV-01 — identity/quyền | Domain bucket bất biến, từ owner khi tạo; default global. A ghi B dùng domain B/quota B; mọi refs cùng domain bucket, key Chunks domain_id#hash. Authorize trước lookup/cache/restore; admin không data grant ngầm | UC-04/06–08/12/17/28; KT §1.4–1.5/10.1; API §2.2.1 |
| INV-02 — durability/integrity | Write-through: pack/sidecar/pages bền vững trước head. Không phát chunk chưa SHA-256 verify. Lỗi trước commit giữ object cũ; GSI/eventual/TTL/refcount không làm trọng tài an toàn | UC-05/23–27; KT §1.2/5.1/6/8/9; API §2.6–2.7 |
| INV-03 — R1 | Shared gate + atomic writer registration, fencing lease, read view trước resolve và location pins; reader mất lease dừng, stale worker không publish/xóa. Durable Deleting/Transitioning không readmit khi lệnh S3 chưa rõ; grace không gia hạn quyền | UC-24/29; KT §8.1–8.3/19; API §2.6/8 |
| INV-04 — R2 commit | Head/revision/quota/reservation/outbox atomic; Complete thêm Completed receipt/state. Stable op_id, unknown outcome tra consistent receipt/outbox, không retry gây double quota/refcount. Immutable pages dưới item limits; transaction ≤100 actions/4 MB | UC-14/25/27; KT §5.2/9.0/10.2; API §2.6/4.3 |
| INV-05 — R2 recovery | Async export không là RPO; close recovery epoch chặn mọi authoritative producer, budget120s rồi abort đóng nếu chưa quiesce. Exact immutable membership + chain contiguous; checkpoint từ replay, không Scan nhiều bảng như snapshot. Chỉ complete cuts 24h +10phút GC buffer và anchor/roots/journal cần thiết; PITR1day không thay bảo vệ pack | UC-25–26; KT §9.1; API §6.4/13.3 |
| INV-06 — recovery security | Stop/gate old service/GC; reconcile destructive actions; history_id/service_incarnation mới, catalog CAS/base checkpoint khớp trước ready. Credentials/token phục hồi disabled, audit và cấp lại; abort upload dở, tính lại used, reset staged/reserved | UC-26; KT §9.1; API §6.4 |
| INV-07 — R3 | PUT/UploadPart revive archive không đọc được và **mọi Restored** từ body client sang Warm, không restore nguồn; IR reuse được. Admission protected_until ≥hard deadline+15phút, commit còn ≥15phút. Complete revalidate/pin; GET/Copy Restored cần hết deadline+120s | UC-20–22; KT §5.1/19.1; API §4.2–4.5/13.2 |
| INV-08 — R4 | Cap object100GiB, single PUT/part/copy5GiB; quota owner logical delta/overwrite credit, reservation64MiB và staging cap16 uploads/logical_limit+100GiB; commit delta theo current head. Admin bearer riêng qua bastion/tunnel, không public9000; ETag PUT MD5/Complete MD5-of-parts/Copy opaque giữ nguồn | UC-05/13–17/27–28; KT §9.2/11/19; API §2.5–2.7/4.5/6.4 |
| INV-09 — policy/tier | Compression/chunking/tiering global, bucket chỉ cache. Cả GLACIER_IR/GLACIER bắt buộc; IR đọc trực tiếp, GLACIER restore. Cold immutable/min retention; controls/sidecar/journal/checkpoint/catalog Standard; không bỏ protection vì age đủ | UC-12/20–22/28; KT §1.6/4.1/7/19.1; API §6.3 |
| INV-10 — profile/privacy | Cùng binary/core contract cho A/B; Badger chỉ dev1node, DDB shared/multi-node. Physical stats admin-only; mọi x-dsg cần signed debug + system_admin + debug_allowed và data authorization. Quota/billing logical; timing/pack shared side-channel vẫn có | UC-02–03/06–08/28/30; KT §1.5/11.1/13.2; API §5/6/8 |

Defaults lease60s/heartbeat15s/grace5s, deadline30phút, node skew≤2s, auth cache≤5s và các limit khác lấy KT §19.1/API §13.3. Điều chỉnh sau pilot W5–6 phải đồng bộ sources và fixture; không giảm invariants. Không thay R1 bằng epoch reclamation, không rút R2 thành PITR/sidecar-only.

## 4. Ownership và dependency

| Tên viết tắt | Thành viên | Trách nhiệm chính |
|---|---|---|
| Hưng | Trịnh Quang Hưng — 23021587, lead | Cloud/Edge, jobs/nodes/metrics, cost/simulator, benchmark/tích hợp |
| Chiến | Nguyễn Minh Chiến — 23021491 | S3/Admin API, auth, multipart/copy, client compatibility |
| Hải | Tô Ngọc Hải — 23021543 | Chunk/dedup/nén/pack, metadata/transaction, GC/compaction, integrity |
| Phong | Đinh Tuấn Phong — 24020266 | Cache/read path, Clock/policy, R1, R2 recovery, tiering/restore |

Chủ trì được ghi trước; người phối hợp chịu phần module của mình. Phân công theo backlog thành viên; KT §18/API §12 chỉ ghi số module, không phải số thứ tự tên thành viên.

## 5. Truy vết milestone W1–W8

| Mốc | Đầu ra triển khai / xác minh dự kiến | Owner và task bàn giao | Dependency / gate |
|---|---|---|---|
| W1 | Use case/scope/DoD; contract operation/error/auth/checksum; model/format; R1/R2/policy draft | Hưng W01-HUNG-02/A01; Chiến W01-CHIEN-01..03; Hải W01-HAI-01..03; Phong W01-PHONG-01..03 | W01-HUNG-01; sources v4/v2. Review tài liệu L1, không đòi production AWS |
| W2 | Pin client fixtures; parser/transport nền; freeze pack/manifest/event/completion/checkpoint serialization; Clock/policy/interfaces; Terraform/health/pricing nền | Chiến W02-CHIEN-01..03; Hải W02-HAI-01..03; Phong W02-PHONG-01..03; Hưng W02-HUNG-01..03 | Contract W1; fixtures/golden/property/fuzz phù hợp; freeze có review, không suy endpoint hoàn tất |
| W3 | Warm MVP1node:8P0+owner/domain/checksum/dedup/nén/pack/Badger/S3/cache; harness và số sơ bộ | Chiến W03-CHIEN-01..03; Hải W03-HAI-01..03; Phong W03-PHONG-01..03; Hưng W03-HUNG-01..03 | W2 formats/adapters; round-trip, lỗi upload giữ head cũ; số Badger không đại diện DDB đủ |
| W4 | Multipart/receipt, DynamoDB atomic commit/outbox, Admin auth/grants/quota/policy/stats/layout; Copy/Delete nền; R1/exporter/CloudEdge packaging | Chiến W04-CHIEN-01..04; Hải W04-HAI-01..03; Phong W04-PHONG-01..04; Hưng W04-HUNG-01..03 | W3 MVP + frozen input/event schema; L2 data/auth/MP/transaction checks |
| W5 | Cloud/Edge thật, R1+GC/compaction, seal/checkpoint/rebuild/24h roots; jobs/nodes/drain/cache scope; presigned và GetObject If-Match P1 (handler + Range/412) compatibility; pilot DDB | Hưng W05-HUNG-01..03; Hải W05-HAI-01..03; Phong W05-PHONG-01..03; Chiến W05-CHIEN-01..03 | W4 transaction/R1/format; real DDB/ALB/Edge evidence, R1/R2 gates; If-Match mapping đã gắn W05-CHIEN-01 trong backlog local |
| W6 | IR/Flexible restore/promote/revival/cross-domainCopy; manual scrub on-demand P1, cost/simulator/faults; chốt threshold sau pilot đầy đủ | Phong W06-PHONG-01..03; Hải W06-HAI-01..03; Chiến W06-CHIEN-01..03; Hưng W06-HUNG-01..03 | W5 profile/coordination/recovery; Cold thật + Clock biên; phân biệt mô phỏng và đo |
| W7 | Feature/API freeze, full P0/P1 compatibility, integrity/auth/quota/R1/R2/Cold fault acceptance, benchmark/cost chính thức | Hưng W07-HUNG-01..03 chủ trì đo; Chiến W07-CHIEN-01..03 API; Hải W07-HAI-01..03 data; Phong W07-PHONG-01..03 recovery | W6 đầy đủ và ngưỡng đã review; L3 gates/evidence, blocker không bị đổi thành P2 |
| W8 | Tài liệu/slide/demo, diễn tập cả hai profile, cleanup/teardown và buffer sửa regression | Hưng điều phối; cả nhóm W08-*-01..03 | W7 evidence + unresolved issues; rerun check liên quan sau sửa; không tự tuyên bố release |

Nguồn milestone: KT §15.2/16/18, API §12; phân công cụ thể từ backlog README và bốn file member (local-only). Các ID task trong bảng là tham chiếu văn bản, không link đến tài nguyên local-only khi tài liệu được chia sẻ.

## 6. Đối chiếu và xử lý khác biệt (chờ review PR)

| ID | Quan sát nguồn | Ảnh hưởng / hướng làm rõ đề nghị | Trạng thái |
|---|---|---|---|
| CL-01 | API §4.1/4.2 có GetObject + If-Match P1; backlog từng ghi 19 dòng S3 P0/P1 | Đã đồng bộ "backlog/api-coverage.md", "backlog/README.md" và W05-CHIEN-01: 8 P0 + 12 P1 = 20 S3, 28 Admin, 3 health. Chiến thực hiện handler If-Match/Range/412 và test W5, không thay P2 khác | Đã chỉnh mapping local; chưa có evidence implementation W5 |
| CL-02 | Câu benchmark KT §13.4 dễ bị hiểu là server-side copy mọi file lớn; API §4.1/4.5/11 loại GetObjectTagging/UploadPartCopy khỏi P1 | Đã giới hạn câu KT §13.4: cp/sync upload (multipart)/download (Range+If-Match) file lớn trong P0/P1; copy giữa key chỉ CopyObject đơn ≤5 GiB; dưới threshold là điều kiện riêng của lệnh high-level; không nâng UploadPartCopy P2 | Đã làm rõ nguồn; chờ review PR |
| CL-03 | API §6.2 có job type scrub P1; KT §15.1 cho full automated scrubber là tùy chọn | Bổ sung contract manual scrub on-demand P1 ở KT §11.2/API §6.2: kiểm checksum/SHA thật trên mẫu hoặc tập hữu hạn readable, đếm candidate/checked/corrupt/skipped, không silent pass/no-op, không auto restore/repair. Hưng (jobs W5, manual scrub W6), Hải/Phong phối hợp; periodic/full-store scrub tùy chọn | Đã làm rõ hợp đồng; nghiệm thu scrub W6 vẫn Not verified |

Các sửa đổi hợp đồng/diễn giải trên **đang chờ review PR**; không coi là approval/implementation đã đạt. CL-01 cập nhật backlog local-only, không đưa backlog lên Git. Khi test/hợp đồng bị chặn, ghi Blocked/Not verified cho đúng feature; không tự đánh Pass.
