# Pricing, dataset và baseline

Ma trận benchmark để review: [workloads, coverage, gap](benchmark-matrix.md), [ngưỡng và verdict](thresholds.md), [tái sử dụng và đối soát 16 AWS runs lịch sử](aws-evidence-reuse.md); kiểm offline `python3 -B scripts/validate_benchmark_matrix.py benchmarks/workload-matrix.json`. Chưa có full benchmark AWS hoặc phê duyệt ngưỡng cuối.

Harness Warm benchmark: [runner, artifact và lệnh chạy](warm-harness.md),
[validation](warm-harness-validation.md),
[số sơ bộ AWS thật và cleanup](warm-aws-results.md).

Prototype: [cost/simulator, native DynamoDB probe và integration smoke](cost-simulator.md).

Phần W2 dưới đây chuẩn bị hợp đồng/đầu vào; số đo W3 nằm ở báo cáo riêng phía trên. Phụ thuộc use case/scope/DoD ([PR #3](https://github.com/KinasTomes/miles/pull/3)) và [kế hoạch môi trường AWS](../docs/planning/aws-environment-and-integration-plan.md); không yêu cầu thêm host Edge hoặc triển khai Cloud để nghiệm thu phần W2.

| Checklist | Đầu ra |
|---|---|
| Pricing có Region/ngày/nguồn và các nhóm storage/request/retrieval/restore/transfer/infra |[Pricing contract/schema/example](../configs/pricing/README.md), [Go parser/validator](../internal/pricing/catalog.go) và negative tests |
| Ba nhóm dataset, generator/seed/checksum |[Dataset contract](datasets/README.md), [smoke manifest](datasets/smoke-manifest.json), `go run ./cmd/datasetgen` |
| Direct-S3 baseline cùng workload/topology/class; chunk-per-object chỉ phụ |[Baseline specification](baseline.md) gồm workload/transport/cache/Cold/domain và evidence requirements |

Pricing schema được dùng chung bởi prototype `internal/cost`, `internal/simulator` và `cmd/costsim`; không thêm cost route/command vào dsgctl. Generator là dev tool, không thay gateway runtime. `cmd/datasetgen` được kiểm trong `go test ./...`; Makefile fmt thêm package embed schema trong configs/pricing; vet/test ./... kiểm cả pricing package và CLI.

## Kiểm tra nhanh

```bash
make check
go run ./cmd/pricingcheck
go test ./internal/pricing ./cmd/pricingcheck
```

CI giữ required job `benchmark-contracts`, chuyển sang Go cho JSON Schema và negative tests, dùng sample public không cần AWS credential. Job Go `check` kiểm generator cùng các package hiện có. Trạng thái run và required checks được xác minh trực tiếp trên PR; kiểm tra local không được tính là CI pass. Không thay quy tắc review hoặc quyền admin bypass.

Xem [validation và giới hạn](validation.md). Các source kiến trúc/API, [Terraform Cloud](../infra/terraform/cloud/README.md), backlog/report/apps giữ nguyên; không commit generated dataset binaries, private configuration hoặc quote chứa định danh account.
