# Theo dõi Miles bằng CloudWatch dashboard

Dashboard là giao diện có sẵn trong **AWS CloudWatch Console**, không phải web
app Miles và không cần EC2/Grafana riêng. Runtime đang có exporter
`Miles/DSG`; Terraform chỉ thêm dashboard đọc các series đó. Cổng Admin và
`/metrics` của gateway vẫn private.

## Bật khi dựng installation

Trong JSON private, chọn profile cần chạy và bật dashboard **trước `prepare`**:

```json
{
  "deployment": {
    "profile": "cloud",
    "max_cost_usd": 5,
    "max_active_minutes": 150
  },
  "enable_dashboard": true
}
```

Đây chỉ là đoạn cấu hình bổ sung, không phải installation JSON đầy đủ hay
quyền tự tạo tài nguyên. Mẫu [Cloud-only](../../infra/terraform/cloud/environments/dev/cloud-demo.example.json)
đã bật cờ này; làm theo [hướng dẫn triển khai](../planning/cloud-demo-deployment.md)
để điền IAM/backend/AMI/network và review saved plans.

- `cloud`: tám biểu đồ cho Cloud; `edge`: tám biểu đồ cho Edge; `both`: hai
  nhóm riêng. Bộ dựng sinh `monitored_profiles` từ `deployment.profile`.
- Terraform trực tiếp cần đặt `monitored_profiles` đúng các profile thực sự chạy.
- Dashboard mặc định tắt để giữ tương thích với các installation cũ; alarms là
  tùy chọn riêng, không tự bật theo dashboard.
- Một installation đã `prepare` giữ config hash. Muốn thêm dashboard vào lượt
  đang tồn tại phải review migration riêng, không sửa hash hay state để vượt gate.

Tên dashboard là `<installation_name>-<region>-runtime`. Dashboard AWS là
resource toàn account; tên chứa Region để tránh trùng khi nhìn từ các Region
khác. Runtime metrics vẫn được đọc từ đúng Region của installation.

## Quyền AWS

Với bootstrap `new`, renderer thêm quyền dashboard khi `enable_dashboard=true`;
review/apply IAM plan như luồng cũ. Với `existing`, renderer chỉ tạo đề xuất
private `rendered/iam/deploy-dashboard.json` để chủ account duyệt/cấp, không tự
sửa IAM. Không thay toàn bộ policy role bằng file bổ sung này.

Quyền Put/Get/Delete dashboard chỉ áp dụng đúng ARN
`arn:aws:cloudwatch::<account_id>:dashboard/<installation_name>-<region>-runtime`.
Không cấp quyền sửa dashboard của installation khác, không thêm quyền public
sharing, không đưa Admin token Miles vào Console.

Quyền đọc metrics trong đề xuất giới hạn theo Region nhưng cho phép đọc các
classic metrics của account trong Region đó; bộ lọc installation của biểu đồ
không phải một cơ chế IAM cô lập metrics. Người mở Console phải có quyền xem
dashboard và đọc metrics phù hợp. Runtime tiếp tục dùng quyền
`cloudwatch:PutMetricData` với namespace `Miles/DSG` trong role/boundary hiện có;
không cần quyền quản lý dashboard trên gateway.

Không tự nhận một dashboard đã tồn tại cùng tên ngoài state. Trước lần apply
đầu, đối chiếu ownership/name với chủ account vì PutDashboard có thể thay toàn
bộ dashboard cùng tên. Mọi thay đổi nội dung thực hiện trong source Terraform,
không sửa bằng tay trên Console rồi bỏ qua drift.

## Mở giao diện và dùng khi demo

Sau `apply active` và `deploy verify`, xem URL từ private runtime inventory:

```bash
python3 - <<'PY'
import json, os
from pathlib import Path
p = Path(os.environ['MILES_PRIVATE']) / 'runtime-inventory.json'
dashboard = json.loads(p.read_text()).get('dashboard')
if not dashboard:
    raise SystemExit('Dashboard disabled or installation paused; inspect the applied plan.')
print(dashboard['url'])
PY
```

Copy URL đầy đủ từ terminal SSH sang trình duyệt đã đăng nhập đúng account/role.
Cũng có thể vào CloudWatch → Dashboards → chọn đúng tên. Dùng dữ liệu synthetic
upload/download theo Cloud guide rồi refresh biểu đồ. Dashboard mở mặc định
một giờ gần nhất, mỗi điểm có period 60 giây; exporter gửi một batch mỗi phút.
Chờ vài chu kỳ và kiểm ingestion trước khi dùng giao diện làm bằng chứng demo.

Quyền tối thiểu của PR không bao gồm `ListDashboards`. Nếu menu Dashboards báo
AccessDenied, mở URL trực tiếp ở trên. `GetDashboard` vẫn cần được cấp trên
đúng tên dashboard; AccessDenied không chứng minh dashboard đã tồn tại hay đã xóa.

| Biểu đồ | Cách đọc |
|---|---|
| Runtime health | Minimum readiness được báo cáo, trạng thái draining và maximum active requests trên một node |
| Warm cache | Số cache hits/misses trong phút và tỷ lệ `hits/(hits+misses)` khi có traffic được báo cáo; đếm cache lookup, không phải tỷ lệ request HTTP |
| Body traffic | Bytes/phút client↔gateway và gateway↔S3; không bao gồm mọi protocol/header overhead |
| Warm reads | Requests, aborted và integrity failures/phút; không thay cho toàn bộ P0/P1 coverage |
| Recovery/R1/telemetry | Maximum cut age, thời gian gate đóng và observation age, theo giây |
| DynamoDB | Calls, transactions, throttles và retries/phút từ runtime |
| Revival/orphans | Stored bytes được báo cáo trong phút; không phải current retained bytes hoặc phí AWS |
| Operational failures | Job dispatch, dependency probe và runtime observation failures/phút |

Các node cùng profile hiện xuất vào cùng series dimension `profile` và
`installation` (table prefix có dấu `-` cuối). Vì vậy:

- Counter deltas dùng `Sum`; gauges dùng `Minimum`/`Maximum` của những node đã
  báo cáo. Không cộng gauge thành tổng cluster hoặc hứa có dữ liệu từng node.
- Readiness bằng 1 chỉ cho biết các sample nhận được báo ready; node không báo
  không tự trở thành sample 0. Kiểm đủ node bằng `dsgctl node ls`, SSM và
  ALB targets, không dựa một đường biểu đồ để kết luận cả cụm khỏe.
- Khoảng trống có thể là chưa phát sinh event, dừng node hoặc export lỗi. Không
  fill bằng 0, không coi missing data là healthy. Cache ratio không có điểm nếu
  không có lookup được báo cáo; counter deltas/ACK có thể thiếu hoặc lặp nên
  không phải ledger chính xác cho nghiệm thu hay hóa đơn.
- Gauge recovery cũ có thể bị runtime bỏ khỏi export khi observation stale;
  kiểm observation age và thời điểm sample, không dùng sample cũ làm trạng thái hiện tại.

Dung lượng logical/physical, dedup ratio, cache capacity và danh sách job/pack
chi tiết vẫn dùng `dsgctl stats`, `cache stats`, `job ls`, `pack ls` qua Admin
access đã xác thực. Dashboard không tự quét S3/DynamoDB để tính số liệu mới.

## Xác minh ingestion và biểu đồ bằng một smoke nhỏ

Chạy sau phần upload/download của Cloud guide, trong cùng shell đã có
`MILES_PRIVATE`, `MILES_REGION`, `MILES_AWS_PROFILE` và `MILES_DEMO_URL`.
CLI dùng deploy profile đã được cấp quyền, không cần root để đọc metrics.
Các lệnh dưới có một PUT và ba GET vào bucket synthetic của chính lượt demo;
phần CloudWatch chỉ đọc. Không chạy lại benchmark hoặc phát metric giả bằng
`put-metric-data` để làm biểu đồ hiện lên.

```bash
export MILES_INSTALLATION="$(python3 -c 'import json,os; print(json.load(open(os.environ["MILES_PRIVATE"]+"/installation.json"))["installation_name"])')"
export MILES_CW_CHECK_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
aws cloudwatch get-dashboard \
  --profile "$MILES_AWS_PROFILE" --region "$MILES_REGION" \
  --dashboard-name "$MILES_INSTALLATION-$MILES_REGION-runtime" \
  --output json --no-cli-pager > "$MILES_PRIVATE/dashboard-readback.json"
curl --config "$MILES_PRIVATE/client.curl" --request PUT \
  --data-binary "@$MILES_PRIVATE/demo-input.txt" "$MILES_DEMO_URL/metrics-demo.txt"
for iteration in 1 2 3; do
  curl --config "$MILES_PRIVATE/client.curl" "$MILES_DEMO_URL/metrics-demo.txt" \
    --output "$MILES_PRIVATE/metrics-demo-output.txt" || break
  cmp "$MILES_PRIVATE/demo-input.txt" "$MILES_PRIVATE/metrics-demo-output.txt" || break
done
```

Chỉ tiếp tục khi GET/hash đều thành công. Đợi khoảng 2–3 phút để có vài chu kỳ
export, rồi chạy truy vấn sau. Nếu ingestion chậm, đợi thêm và tạo lại end time;
không coi thời hạn chờ này là SLA. Start time giữ từ **trước traffic**, không lấy
dữ liệu cũ của lượt khác để thay.

```bash
export MILES_CW_CHECK_END="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
cw_metric() {
  local metric_name="$1" statistic="$2"
  shift 2
  aws cloudwatch get-metric-statistics \
    --profile "$MILES_AWS_PROFILE" --region "$MILES_REGION" \
    --namespace Miles/DSG --metric-name "$metric_name" \
    --dimensions Name=profile,Value=cloud \
      "Name=installation,Value=$MILES_INSTALLATION-" "$@" \
    --start-time "$MILES_CW_CHECK_START" --end-time "$MILES_CW_CHECK_END" \
    --period 60 --statistics "$statistic" \
    --query 'sort_by(Datapoints,&Timestamp)' --output json --no-cli-pager
}
cw_metric miles_ready Minimum Name=mode,Value=runtime \
  > "$MILES_PRIVATE/cloudwatch-readiness.json"
cw_metric miles_warm_read_requests_total Sum \
  > "$MILES_PRIVATE/cloudwatch-read-events.json"
cw_metric miles_client_gateway_body_bytes_total Sum \
  > "$MILES_PRIVATE/cloudwatch-upload-bytes.json"
cw_metric miles_runtime_observation_age_seconds Maximum \
  > "$MILES_PRIVATE/cloudwatch-observation-age.json"
cat "$MILES_PRIVATE/cloudwatch-readiness.json"
cat "$MILES_PRIVATE/cloudwatch-read-events.json"
cat "$MILES_PRIVATE/cloudwatch-upload-bytes.json"
cat "$MILES_PRIVATE/cloudwatch-observation-age.json"
```

Mở dashboard, chọn cùng time range bao phủ UTC start/end vừa ghi, refresh và
đối chiếu tên metric/statistic trong Source hoặc Graphed metrics của widget.
Khi đối chiếu bằng Console có thể phóng rộng cửa sổ quanh start/end vì điểm được
gom theo phút; không đòi timestamp đúng từng giây của HTTP request.

| Kiểm tra | PASS | Chưa đạt / chưa xác minh |
|---|---|---|
| Dashboard | GetDashboard thành công, tên/body đúng installation/Region/profile; Console hiện đúng tám panel Cloud | AccessDenied, không tìm thấy hoặc body khác plan |
| Ingestion mới | Có datapoints mới thuộc cửa sổ vừa tạo, readiness được báo cáo bằng 1 và observation age dưới 60 giây | `[]`, sample cũ hoặc observation stale; không phải PASS với số 0 tự điền |
| Traffic | Warm read events và client→gateway body bytes có Sum dương sau thao tác nhỏ | Chỉ có metrics lịch sử hoặc không có event mới |
| Biểu đồ | Cùng time range, period 60, statistic/dimensions khớp truy vấn API; traffic/read points xuất hiện tương ứng | Chỉ có khung widget hoặc dùng sai dimension/statistic |
| Nội dung | Ba GET và `cmp` thành công; hai Cloud targets/node readiness được kiểm riêng bởi `deploy verify` | Biểu đồ đẹp nhưng GET/hash hoặc readiness có lỗi |

Không yêu cầu số event bằng chính xác ba hoặc byte counter bằng kích thước
file: cả hai node, traffic khác trong cùng phút và telemetry best-effort có thể
đóng góp. Không dùng screenshot để khẳng định cả hai node đều xuất metrics.
Lưu UTC interval, source/release, installation/profile, các JSON đọc lại và ảnh
dashboard đã che thông tin riêng nếu cần bàn giao; không lưu token vào ảnh/log.

## Khi biểu đồ trống

1. Kiểm account/Region, time range, dashboard đúng installation/profile và runtime
   đã chạy đủ thời gian xuất metrics. Không lấy metrics lịch sử của lượt cũ làm
   trạng thái lượt mới.
2. Kiểm quyền Console GetDashboard/GetMetricData và runtime PutMetricData;
   xác nhận boundary, KMS/STS/network endpoints theo cấu hình hiện có.
3. Dùng SSM/SSH đã duyệt kiểm service Miles, private readiness và `/metrics` trên
   cổng health 9100 của từng node. Nếu `/metrics` có dữ liệu mà Console không có,
   kiểm exporter/IAM/đường tới CloudWatch; không public cổng để chữa lỗi.
4. Kiểm đúng dimension: `installation=<table-prefix>` và `profile=cloud|edge`;
   riêng `miles_ready` có thêm `mode=runtime`. Chưa có kết quả thì ghi thiếu
   telemetry, không sửa biểu đồ để giả số 0/PASS.

## Pause và teardown

Pause xóa dashboard cùng serving resources, giữ `enable_dashboard=true` trong
session configuration để resume tạo lại đúng tên. Gate pause/resume chỉ nhận
create/delete đúng dashboard đã chọn; update, rename hoặc graph khác cần review
riêng. Full teardown vẫn xóa dashboard nếu còn trong state; `deploy closeout`
kiểm native GetDashboard trả `ResourceNotFound`. AccessDenied hoặc lỗi API
không được coi là bằng chứng đã xóa.

Bộ audit tài nguyên chung cũng ghi nhận dashboard trong frozen Terraform state,
native GetDashboard và so sánh inventory account-wide. Account baseline mới dùng
schema `miles-account-inventory/v3` có thêm danh sách dashboards; audit role cần
ListDashboards/GetDashboard (template auditor đã cập nhật). Giữ baseline cũ nguyên
vẹn và dùng source đã pin khi audit lượt cũ; không sửa hash, thêm danh sách rỗng
hoặc lấy baseline mới sau triển khai để thay bằng chứng trước triển khai.

Việc xóa dashboard không xóa lịch sử metric hoặc tắt chi phí của storage/key
được giữ. Dashboard và custom metrics có thể tính phí theo
[CloudWatch pricing](https://aws.amazon.com/cloudwatch/pricing/); không coi có
Free Tier/credits là mọi thành phần đều miễn phí. Chưa tạo tài nguyên hoặc
xác minh ingestion trên AWS cho phần hướng dẫn mới này.

Nguồn: [dashboard JSON syntax](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Dashboard-Body-Structure.html),
[PutDashboard](https://docs.aws.amazon.com/AmazonCloudWatch/latest/APIReference/API_PutDashboard.html),
[provider dashboard v6.21.0](https://github.com/hashicorp/terraform-provider-aws/blob/v6.21.0/internal/service/cloudwatch/dashboard.go)
và [CloudWatch authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_amazoncloudwatch.html).
Lệnh đọc lại dựa trên [GetDashboard CLI](https://docs.aws.amazon.com/cli/latest/reference/cloudwatch/get-dashboard.html)
và [GetMetricStatistics CLI](https://docs.aws.amazon.com/cli/latest/reference/cloudwatch/get-metric-statistics.html).
