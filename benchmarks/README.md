# W02-HUNG-02 — Pricing, dataset và baseline

Bản triển khai chuẩn bị hợp đồng/đầu vào để lead review, chưa đóng task và chưa có benchmark DSG/AWS. Phụ thuộc W01-HUNG-02/W01-HUNG-03; không yêu cầu thêm host Edge hoặc triển khai Cloud để nghiệm thu phần W2.

| Checklist W02-HUNG-02 | Đầu ra |
|---|---|
| Pricing có Region/ngày/nguồn và các nhóm storage/request/retrieval/restore/transfer/infra |[Pricing contract/schema/example](../configs/pricing/README.md), [validator](pricing.py) và negative tests |
| Ba nhóm dataset, generator/seed/checksum |[Dataset contract](datasets/README.md), [smoke manifest](datasets/smoke-manifest.json), `go run ./cmd/datasetgen` |
| Direct-S3 baseline cùng workload/topology/class; chunk-per-object chỉ phụ |[Baseline specification](baseline.md) gồm workload/transport/cache/Cold/domain và evidence requirements |

Pricing schema dùng chung cho cost/report/simulator ở task sau; chưa viết Go cost engine/simulator hoặc tự bổ sung command dsgctl. Generator là dev tool, không thay gateway runtime. `cmd/datasetgen` được kiểm trong `go test ./...`; Makefile hiện có đã fmt/vet/test thư mục cmd/internal nên không cần sửa Makefile.

## Kiểm tra nhanh

```bash
make check
python3 -m venv /tmp/miles-pricing-venv
/tmp/miles-pricing-venv/bin/pip install -r benchmarks/requirements.txt
/tmp/miles-pricing-venv/bin/python -B benchmarks/pricing.py
/tmp/miles-pricing-venv/bin/python -B -m unittest discover -s benchmarks/tests -v
```

CI thêm job `benchmark-contracts` cho JSONSchema và negative tests, dùng sample public không cần AWS credential. Job Go `check` kiểm generator cùng các package hiện có. Trạng thái run và required checks được xác minh trực tiếp trên PR; kiểm tra local không được tính là CI pass. Không thay quy tắc review hoặc quyền admin bypass.

Xem [validation và giới hạn](validation.md). Các source kiến trúc/API, Terraform W02-HUNG-01, backlog/report/apps giữ nguyên; không commit generated dataset binaries, private configuration hoặc quote chứa định danh account.
