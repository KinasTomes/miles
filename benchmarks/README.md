# W02-HUNG-02 — Pricing, dataset và baseline

Harness W03-HUNG-02: [runner, artifact và lệnh chạy](warm-harness.md),
[validation](warm-harness-validation.md),
[số sơ bộ AWS thật và cleanup](warm-aws-results.md).

Phần W2 dưới đây chuẩn bị hợp đồng/đầu vào; số đo W3 nằm ở báo cáo riêng phía trên. Phụ thuộc W01-HUNG-02/W01-HUNG-03; không yêu cầu thêm host Edge hoặc triển khai Cloud để nghiệm thu phần W2.

| Checklist W02-HUNG-02 | Đầu ra |
|---|---|
| Pricing có Region/ngày/nguồn và các nhóm storage/request/retrieval/restore/transfer/infra |[Pricing contract/schema/example](../configs/pricing/README.md), [Go parser/validator](../internal/pricing/catalog.go) và negative tests |
| Ba nhóm dataset, generator/seed/checksum |[Dataset contract](datasets/README.md), [smoke manifest](datasets/smoke-manifest.json), `go run ./cmd/datasetgen` |
| Direct-S3 baseline cùng workload/topology/class; chunk-per-object chỉ phụ |[Baseline specification](baseline.md) gồm workload/transport/cache/Cold/domain và evidence requirements |

Pricing schema dùng chung cho cost/report/simulator ở task sau; chưa viết Go cost engine/simulator hoặc tự bổ sung command dsgctl. Generator là dev tool, không thay gateway runtime. `cmd/datasetgen` được kiểm trong `go test ./...`; Makefile fmt thêm package embed schema trong configs/pricing; vet/test ./... kiểm cả pricing package và CLI.

## Kiểm tra nhanh

```bash
make check
go run ./cmd/pricingcheck
go test ./internal/pricing ./cmd/pricingcheck
```

CI giữ required job `benchmark-contracts`, chuyển sang Go cho JSON Schema và negative tests, dùng sample public không cần AWS credential. Job Go `check` kiểm generator cùng các package hiện có. Trạng thái run và required checks được xác minh trực tiếp trên PR; kiểm tra local không được tính là CI pass. Không thay quy tắc review hoặc quyền admin bypass.

Xem [validation và giới hạn](validation.md). Các source kiến trúc/API, Terraform W02-HUNG-01, backlog/report/apps giữ nguyên; không commit generated dataset binaries, private configuration hoặc quote chứa định danh account.
