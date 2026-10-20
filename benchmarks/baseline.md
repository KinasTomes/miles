# Baseline và workload specification — W02-HUNG-02

**Trạng thái: kế hoạch để review, chưa chạy benchmark.** Owner Hưng; phối hợp Chiến(client/API), Hải(engine/metadata/integrity), Phong(cache/Clock/lifecycle/recovery). Nguồn: [KT v4 §13.1–13.4/15.2/16](../docs/dedup-storage-gateway-architecture.md), [API v2 §2.7/4.5/6.3/11–12](../docs/dedup-storage-gateway-api.md), [DoD](../docs/acceptance/definition-of-done.md). Không thay R1–R4 hoặc mở UploadPartCopy P2.

## 1. Đối chứng và fairness

| Run | Bên client | Backend/đường chính | Được kết luận |
|---|---|---|---|
| direct-s3 (chính) |Cùng executable, files, keys, metadata và workload schedule với DSG |Client→S3 thật, object độc lập, SSE-KMS và Region cùng giả định |Chi phí/throughput so với ghi thẳng S3; không áp gateway compute lên baseline |
| dsg |Client→S3-facing gateway; client luôn gửi đủ filebytes |Gateway→S3 packs/controls, DDB metadata, đầy đủ infrastructure và R1/R2 khi mốc yêu cầu |Hiệu quả tổng sau nén/dedup/packing/tiering/cache và overhead |
| chunk-per-object (phụ) |Dùng chính chunk boundaries/hash/codec của DSG input, mỗi blob thành object thay vì pack |Giữ domain/hash/codec và phạm vi dedup tương đương; tính index/manifest/request thực nếu có |Chỉ giải thích packing/request reduction; không thay direct-s3 để tuyên bố tiết kiệm tổng |

Mọi so sánh phải cùng input manifest/hash, seed, class policy, Region, encryption giả định, concurrency, client config, duration và accounting window. Bật/tắt checksum hoặc nén client chỉ một bên là trial không hợp lệ. Pin executable/dependency versions thật trước run theo fixture [transport.json](../tests/fixtures/s3/transport.json); ghi lại actual phiên bản, không copy tên version từ tài liệu như bằng chứng đã cài.

Sử dụng fresh backend/trial isolation hoặc phương pháp reset được kiểm chứng để không dedup/cache với run trước. Bucket/key mới riêng lẻ **không đủ** vì dedup toàn domain. Nếu dùng domain riêng cho trial, vẫn phải đo/phân bổ physical overhead và shared pack đúng phương pháp, không suy bucket unique totals cộng được thành system total. Không xóa Cold/recovery roots trước retention chỉ để reset. Lưu snapshot đầu/cuối, background backlog và retention còn lại.

## 2. Topology matrix (chưa triển khai bởi task này)

| Scenario | direct S3 | DSG | Đường byte/cost cần ghi |
|---|---|---|---|
| A / Cloud |Client EC2 cùng Region→S3 |Cùng client→ALB→gateway cluster→S3/DDB qua endpoint |Client↔gateway, gateway↔S3, ALB/LCU, crossAZ và control-plane thực; cache không chứng minh giảm Internet egress |
| B / Edge |Client test ngoài AWS→S3 |Cùng client local→gateway local1–2nodes→S3/DDB HTTPS Internet |AWS→Edge download, Edge→AWS upload, local gateway→client, metadata/control; VM/disk/network overhead có giả định định lượng |
| Cloud+Internet client (đối chứng riêng) |Internet client→S3 |Cùng client→Cloud gateway→S3 |Gateway vẫn gửi đủ logical bytes ra AWS; không dùng hit ratio giảm gateway→Internet bytes |

EC2 chạy cấu hình Edge có thể test chức năng nhưng không là benchmark on-prem B thật. Việc cung cấp host B và chạy A/B thuộc W5–7, **không là dependency đóng task W2 này**. Không dùng máy cá nhân làm runtime/dataset lớn.

## 3. Workload chuẩn bị

Dataset theo [manifest/generator](datasets/README.md). Mỗi trial ghi manifestSHA256 và fileSHA256, giữ thứ tự file trong manifest trừ khi khai báo lịch khác. Thực hiện riêng từng nhóm để so entropy; nếu gộp nhóm thì phải giữ breakdown và ghi thứ tự interleave rõ. Smoke chỉ kiểm harness/protocol; backup30 là điểm khởi đầu cần pilot scale W6.

| ID | Trình tự dự kiến | Evidence / giới hạn |
|---|---|---|
| WL-01 warm-write |Upload từng file dưới key riêng chứa group/version; giữ đủ phiên bản, không overwrite cùng key làm mất logical snapshot |Tất cả payload giữ nguyên; bytehash trước upload và GET verify; committed live logical khác cumulative attempted bytes |
| WL-02 read-cache |Đọc toàn bộ dataset1lần với cache sạch rồi thêm3lần đọc lặp; Range subset ghi riêng, không trộn cold/warm samples |Ghi method purge/reset được verify, warmup/repeat counters, success/failed bytes; direct S3 chạy cùng logical reads |
| WL-03 overwrite |Run riêng: upload base key rồi overwrite cùng logical size/nội dung phiên bản mới; cuối cùng GET verify |Commit logical usage theo head hiện tại; ghi bytes lịch sử riêng, không cộng mọi version vào live bytes; không cần fault test này ởW2 |
| WL-04 domains |Hai owner upload cùng files ở bucket của mình, một trial cùng default domain và một trial miền tách, workloads/grants giữ tương đương |Không duplicate dataset files local; mapping actor/bucket/key ở private run config. So identity/physical/network, không hứa hết timing side-channel |
| WL-05 multipart |Upload/verify dataset qua client high-level với threshold8MiB, part size8MiB, concurrency cố định; test threshold−1/=/+1 bằng fixture riêng |File gzip có kích thước thực trong manifest; log operation dispatch/checksum mode/part count. UploadPartCopy/GetObjectTagging không thuộc required baseline |
| WL-06 Cold/restore/revival |Sau Warm verification, dùng policy toàn hệ thống chọn IR rồi Flexible trong trial riêng; đọc/restore/copy/reupload các hash trùng có chọn lọc |Cold transition/restore thực được ghi thời gian thật; Clock/simulator kiểm tuổi/retention dài riêng, không giả đã chờ90ngày |

**Single-stream PUT throughput** là trial WL-01 riêng, client concurrency1, dùng PutObject trực tiếp hoặc threshold64MiB để backup30 không chuyển multipart; cả direct-S3/DSG dùng cùng route transport/threshold, filecap≤5GiB. Không suy throughput single PUT từ high-level multipart concurrency. WL-05 cố ý khác config, không trộn sample hai nhóm.

Cache warmup và provisioning nằm ngoài thời gian stream throughput nhưng phải tính trong chi phí toàn trial. Đề xuất3lượt lặp độc lập mỗi cấu hình sau pilot, giữ từng raw sample; W6 review sample count/duration/concurrency trước W7. Đây là tham số kế hoạch, không là số lượt đã thực hiện. Backend/control export phải được quan sát tới boundary cuối; nếu còn outbox/journal/checkpoint pending, ghi pending bytes/requests và cost window, không dừng đồng hồ lúc PUT response rồi bỏ phí async.

## 4. Storage class và vòng đời

- Baseline chính Warm dùng S3 Standard; DSG pack Warm Standard, sidecar/journal/checkpoint/catalog luôn Standard. Backend SSE-KMS/cipher metadata/config chung theo thiết kế; baseline cũng tính KMS phát sinh.
- IR/Flexible được đo riêng bằng override policy toàn hệ thống; direct S3 chạy lịch class/restore theo giả định công bố tương ứng, không áp policy pack vật lý như thể S3 hiểu logical dedup. Nếu comparator policy khác phải ghi đó là biến thử nghiệm.
- Retention/minimum size/archive metadata theo [pricing contract](../configs/pricing/README.md); tính cả dead/retired/orphan, restored copy tạm và phí xóa/chuyển sớm. Không cộng lifecycle transition vào self-COPY đã tính phí request.
- Same-domain CopyObject có thể copy Cold metadata mà không payload; cross-domain destination-first chỉ tải phần thiếu, GET/Copy Restored dùng deadline+margin, PUT/UploadPart revive mọi Restored theo R3. Các khác biệt có chủ đích với direct S3 phải ghi riêng, không ghép thành một “same operation” latency khi semantics khác.
- Simulator dùng Clock/policy/pricing chung để so4policies/≥3traces ởW6, chưa hiện thực trong task này. Kết quả mô phỏng tách measured AWS; không gọi forecast là observed bill.

## 5. Byte và request accounting

Đo riêng: client_to_gateway, gateway_to_client, gateway_to_s3_upload, s3_to_gateway_download, metadata/control-plane và inter-node. Direct-S3 ghi client↔S3. Phải phân biệt logical body sau aws-chunked decode với wire bytes, compression, read-ahead, retries và failed uploads. Không chuyển cache-hit ratio thành tiền khi chưa có route/cost rule.

S3 request counters tách PUT/COPY/MP/GET/HEAD/LIST/restore/delete/transition thực, gồm controls và retries; DDB đo billed read/write units/transactions/PITR, không lấy số lời gọi metadata thay billed units. Gateway physical upload includes sidecar/header/footer/loser blobs; journal/checkpoint async là nhóm riêng phải cộng tổng đúng1lần. Restore theo unique whole pack, denominator logical requested range; mẫu số0→null.

| Chỉ số | Định nghĩa / nguồn raw |
|---|---|
| Throughput |Successful logical bytes / elapsed seconds, tách upload/download, concurrency và định nghĩa elapsed |
| P95 |Phân phối latency operation end-to-end trên samples thật; ghi N, không chỉ trung bình từng P95 |
| Integrity |SHA256 toàn file input/output; mọi lỗi/chunk corrupt/stream abort phải ghi, không loại sample lỗi để báo100% |
| dedup_ratio |Live logical / unique raw mỗi domain/hash sống1lần |
| compression_ratio |Unique raw / unique stored canonical |
| total_reduction |Live logical / physical backend thực giữ, gồm overhead/dead và breakdown billed restored bytes |
| cache_hit_ratio |Định nghĩa granularity request/segment/byte trước run, ghi numerator/denominator; không trộn bytehit và requesthit |
| PUT reduction |1−PUT_DSG/PUT_chunk-per-object cùng scope/interval; không thay mẫu số bằng direct-S3 |
| Cost/net saving |Tích phân byte-time+requests/retrieval/restore/transfer/infra; net=directS3−DSG, không tính khi required price còn unpriced; credits/Free Tier tách lớp riêng, allowance theo account/Region không reset theo trial |

Targets dedup≥5×, cache≥80%, single-streamPUT≥50% directS3, PUT reduction≥90% chunk-per-object là **provisional, chưa đo/chưa đạt**; cần chốt W5–6 trước W7. Toàn vẹn100% trên verified cases vẫn là yêu cầu correctness, không hạ xuống để đạt performance. Revival có thể làm physical reduction kém hơn dù dedup identity ratio không đổi.

## 6. Run record bắt buộc (chưa có completed run)

Run ID/version/commit, source contracts, generator/Go/seed/manifest hash, dataset sizes, actual client/SDK/checksum/threshold/timeout, Region/topology/instance/CPU/disk/network/IAM provider (không secret), system policy revision/codec/chunking, DDB/Badger mode, quota/domain setup, operation order/concurrency/warmup/repetitions, timestamps/sample count, raw metrics/status/hash/logs, pricing snapshot hash/units, async outstanding work, cost assumptions, actual results, failed/excluded cases kèm lý do, limitations và cleanup/retention residual.

Không commit account IDs, resource ARNs, credential, hostname/private endpoint hoặc raw bill của nhóm vào repo public. Sample dùng placeholder; evidence thật redacted trước chia sẻ. W3 chỉ preliminary Badger/Warm; W5–6 pilot DynamoDB/R1/R2/Cloud/Edge đầy đủ; W7 benchmark chính thức. Schema/dataset/spec W2 không tự đóng những task đó.
