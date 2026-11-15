# Edge deployment

Dùng cùng release/engine như Cloud, profile `edge`, local cache trên `/var/lib/miles`;
không tạo backend riêng, không dùng credentials Cloud/EC2 cho host ngoài AWS.
Runtime đã nối shared DynamoDB/R1/S3 theo
[cấu hình đầy đủ](../../docs/development/shared-runtime.md). Installer mặc định
stage skeleton; điền `shared-edge.example.json` và provision
backend/secret/history trước khi start. Chỉ phục vụ client sau khi provider,
chrony và backend thật đã kiểm tra thành công. Nếu Edge cùng chạy trên AWS,
dùng [installation guide](../../docs/testing/aws-personal-account-guide.md);
host VM/on-prem làm theo các bước thủ công bên dưới.

Thứ tự host mới: chuẩn bị user/directories và dependency → stage release →
điền startup/provider/TLS → bootstrap backend một lần bằng operator khi mọi
gateway dừng → start Miles → kiểm readiness → start proxy. Backend đã provision
thì không bootstrap lại. Khi nâng cấp, drain/dừng bản cũ trước khi đổi release.

Host cần Linux systemd, Python 3.12+, Nginx có TLS, kiến trúc phù hợp release và
`aws_signing_helper` được pin/verify theo [hướng dẫn AWS](https://docs.aws.amazon.com/rolesanywhere/latest/userguide/credential-helper.html).
Tạo user/group hệ thống `miles`, home/data `/var/lib/miles` mode0700 thuộc miles.
`/opt/miles`, `/etc/miles` và unit files do root quản lý, Miles chỉ được đọc.

Sau khi kiểm archive SHA-256 từ kênh tin cậy và giải nén an toàn, đặt `MILES_REGION`
theo Region trong installation config trên shell của host Edge.

```bash
sudo python3 bundle/deploy/install.py --bundle "$PWD/bundle" --profile edge --address 127.0.0.1 \
  --region "${MILES_REGION:?Set MILES_REGION from the installation config}"
sudo systemctl daemon-reload
```

Không bật `miles-lifecycle.service` trên Edge; đó là controller ASG/IMDS riêng cho Cloud.
Unit `miles.service` dùng cùng binary/config contract; `mode=runtime`, drain 1800s.
Không cho key của S3 client vào AWS credential provider (hai danh tính khác nhau).

Sau stage, operator hoàn thiện `/etc/miles/startup.json` từ shared Edge example:
`runtime_node_id` riêng cho node, table prefix/bucket/KMS/secret đúng installation,
`aws_profile` khớp provider và `utc_reference` đúng nguồn chrony đã kiểm. Giữ S3,
Admin và health ở loopback khi dùng proxy trên cùng host. Installer không tự điền
những backend references này và không ghi đè config operator đã có.

## Credentials và TLS

`/etc/miles/aws-config` dùng profile khớp `aws_profile` trong startup JSON (mặc định
`miles-runtime`), `credential_process` của IAM
Roles Anywhere. Thay public ARN placeholders bằng trust anchor/profile/role đã
review, scope role tới backend/tenant tương ứng. Certificate CN phải khớp
`edge_certificate_common_name` của installation. Cài helper tại đường dẫn đã ghi;
certificate và private key provider ở `/etc/miles/rolesanywhere/`, directory0750
root:miles, key0640 root:miles. Không đặt secret trong environment, startup JSON,
Terraform hoặc Git. SDK refresh qua provider; test thực tế cả phiên hết hạn/renewal,
revocation và clock skew trước nghiệm thu. `AWS_EC2_METADATA_DISABLED=true` để không
fallback IMDS khi helper hỏng. Không copy instance profile/static AWS access key.

Edge private không có NAT cần endpoint Roles Anywhere cùng đường TCP 443 từ SG
Edge tới SG endpoint. Module Edge hiện có đường PrivateLink và ingress tương ứng
khi bật Edge; kiểm Terraform outputs và network thực tế trước startup theo
[installation guide](../../docs/testing/aws-personal-account-guide.md).
VM/on-prem cần HTTPS tới các dịch vụ AWS của đúng Region và NTP đã cấu hình.

Copy `nginx.conf` vào include bên trong `http {}` của Nginx; bỏ default server khác
đang mở cổng ngoài dự kiến. Dùng server_name/DNS/certificate thật và key TLS riêng
root-only `/etc/miles/tls/{fullchain.pem,private-key.pem}`. Kiểm `nginx -t` trước
reload. Chỉ mở 443 cho client được phép; S3 bind127.0.0.1:8080, Admin9000 và
health/metrics9100 chỉ loopback/management network được kiểm soát, không proxy ra
Internet. Bootstrap system token theo shared-runtime guide rồi cấp user/Miles
S3 credential và owner token qua [Admin API](../../docs/api-usage-guide.md);
AWS Roles Anywhere credential chỉ phục vụ backend.

Với host dùng `umask 077`, kiểm cả quyền traverse thư mục cha, không chỉ file key.
Ví dụ trước startup, root đặt `/etc/miles/rolesanywhere` mode `0750`, owner
`root:miles`, và key provider mode `0640 root:miles`. Directory `0700 root` sẽ
chặn user `miles` đọc key dù file permission đúng. Không nới key thành world-readable.

Giữ `proxy_pass` không URI/rewrite, `$http_host` (kể cả port), `merge_slashes off`,
underscores headers, không buffer request/response và không retry write sang
upstream khác. Timeout proxy150s; không thêm body-size limit phá PUT lớn. Source:
[Nginx proxy_pass](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_pass).

## Start/stop và nghiệm thu

Sau khi hoàn tất config/provision: start Miles trước, kiểm private `/healthz`200 và `/readyz`200,
config/credentials/backend/clock; rồi mới start Nginx. Test signed PUT/GET/HEAD/Range,
LIST, Unicode/`%`/`+`/double slash và presigned URL qua DNS/TLS thật. Fixture local
chỉ chứng minh transport + native SigV4 router, chưa chứng minh storage/provider thật.

Drain Edge: ngừng nhận client mới ở front door nhưng giữ kết nối đang chạy, gửi
SIGTERM bằng `systemctl stop miles`, chờ exit0 **và** `shutdown complete` của đúng
invocation, rồi mới dừng proxy/host. Tránh `nginx -s stop`/SIGKILL cắt request đang
chạy; với Nginx dùng graceful quit và theo dõi workers. Nếu timeout/flush thất bại,
giữ trạng thái incomplete, không báo node drained. Jobs/Nodes/drain và R2 flush
phải được kiểm từ runtime/invocation thật, không suy ra từ proxy hoặc installer.
