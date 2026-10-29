# Edge skeleton

Dùng cùng release/engine như Cloud, profile `edge`, local cache trên `/var/lib/miles`;
không tạo backend riêng, không dùng credentials Cloud/EC2 cho host ngoài AWS.
Production composition chưa nối xong; **chỉ stage cấu hình** cho W5. Không start
Nginx/Miles để phục vụ client cho tới khi runtime ready và các port được wire đúng.

Host cần Linux systemd, Python 3.12+, Nginx có TLS, kiến trúc phù hợp release và
`aws_signing_helper` được pin/verify theo [hướng dẫn AWS](https://docs.aws.amazon.com/rolesanywhere/latest/userguide/credential-helper.html).
Tạo user/group hệ thống `miles`, home/data `/var/lib/miles` mode0700 thuộc miles.
`/opt/miles`, `/etc/miles` và unit files do root quản lý, Miles chỉ được đọc.

Sau khi kiểm archive SHA-256 từ kênh tin cậy và giải nén an toàn:

```bash
sudo python3 bundle/deploy/install.py --bundle "$PWD/bundle" --profile edge --address 127.0.0.1
sudo systemctl daemon-reload
```

Không bật `miles-lifecycle.service` trên Edge; đó là controller ASG/IMDS riêng cho Cloud.
Unit `miles.service` dùng cùng binary/config contract; `mode=runtime`, drain 1800s.
Không cho key của S3 client vào AWS credential provider (hai danh tính khác nhau).

## Credentials và TLS

`/etc/miles/aws-config` dùng profile `miles-runtime`, `credential_process` của IAM
Roles Anywhere. Thay public ARN placeholders bằng trust anchor/profile/role đã
review, scope role tới backend/tenant tương ứng. Cài helper tại đường dẫn đã ghi;
certificate và private key provider ở `/etc/miles/rolesanywhere/`, directory0750
root:miles, key0640 root:miles. Không đặt secret trong environment, startup JSON,
Terraform hoặc Git. SDK refresh qua provider; test thực tế cả phiên hết hạn/renewal,
revocation và clock skew trước nghiệm thu. `AWS_EC2_METADATA_DISABLED=true` để không
fallback IMDS khi helper hỏng. Không copy instance profile/static AWS access key.

Copy `nginx.conf` vào include bên trong `http {}` của Nginx; bỏ default server khác
đang mở cổng ngoài dự kiến. Dùng server_name/DNS/certificate thật và key TLS riêng
root-only `/etc/miles/tls/{fullchain.pem,private-key.pem}`. Kiểm `nginx -t` trước
reload. Chỉ mở 443 cho client được phép; S3 bind127.0.0.1:8080, Admin9000 và
health/metrics9100 chỉ loopback/management network được kiểm soát, không proxy ra
Internet. Chưa cấu hình hệ thống client identity/Admin bằng task này.

Giữ `proxy_pass` không URI/rewrite, `$http_host` (kể cả port), `merge_slashes off`,
underscores headers, không buffer request/response và không retry write sang
upstream khác. Timeout proxy150s; không thêm body-size limit phá PUT lớn. Source:
[Nginx proxy_pass](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_pass).

## Start/stop trong W5

Sau runtime integration: start Miles trước, kiểm private `/healthz`200 và `/readyz`200,
config/credentials/backend/clock; rồi mới start Nginx. Test signed PUT/GET/HEAD/Range,
LIST, Unicode/`%`/`+`/double slash và presigned URL qua DNS/TLS thật. Fixture local
chỉ chứng minh transport + native SigV4 router, chưa chứng minh storage/provider thật.

Drain Edge: ngừng nhận client mới ở front door nhưng giữ kết nối đang chạy, gửi
SIGTERM bằng `systemctl stop miles`, chờ exit0 **và** `shutdown complete` của đúng
invocation, rồi mới dừng proxy/host. Tránh `nginx -s stop`/SIGKILL cắt request đang
chạy; với Nginx dùng graceful quit và theo dõi workers. Nếu timeout/flush thất bại,
giữ trạng thái incomplete, không báo node drained. Jobs/Nodes/drain API và chứng
minh R2 flush thật thuộc tích hợp W5, không do proxy/installer giả lập.
