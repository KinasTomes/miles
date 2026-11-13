"""Refresh regional public rates and produce an explicit, non-binding envelope."""

from decimal import Decimal
import json
from deploy.acceptance.common import now, require, private


def review(r):
    r.identity(iam=True)
    specs = {
        "EC2": (
            "AmazonEC2",
            [
                ("instanceType", "t4g.small"),
                ("operatingSystem", "Linux"),
                ("tenancy", "Shared"),
                ("preInstalledSw", "NA"),
                ("capacitystatus", "Used"),
            ],
        ),
        "EBS": ("AmazonEC2", [("volumeApiName", "gp3")]),
        "VPC": ("AmazonVPC", []),
        "ELB": ("AWSELB", []),
        "DDB": ("AmazonDynamoDB", []),
    }
    products = {}
    for name, (service, filters) in specs.items():
        filters = [
            {"Type": "TERM_MATCH", "Field": key, "Value": value}
            for key, value in [("regionCode", r.c["region"]), *filters]
        ]
        result = r.aws(
            "pricing",
            "get-products",
            "--service-code",
            service,
            "--filters",
            json.dumps(filters),
            iam=True,
            region="us-east-1",
        )
        products[name] = [json.loads(x) for x in result.get("PriceList", [])]
        require(products[name], "No regional Pricing products for " + name)
        r.write(
            "pricing/" + name + ".json",
            {
                "captured_at": now().isoformat(),
                "filters": filters,
                "products": products[name],
            },
        )

    def rate(name, unit, word):
        found = []
        for product in products[name]:
            for term in product.get("terms", {}).get("OnDemand", {}).values():
                for dimension in term.get("priceDimensions", {}).values():
                    if (
                        dimension["unit"] == unit
                        and word.lower() in dimension["description"].lower()
                        and Decimal(dimension["pricePerUnit"]["USD"]) > 0
                    ):
                        found.append(Decimal(dimension["pricePerUnit"]["USD"]))
        require(
            found,
            "Missing unambiguous pricing dimension " + name + "/" + unit + "/" + word,
        )
        return max(
            found
        )  # Conservative across applicable positive tiers; no free-tier assumption.

    rates = {
        "vm_hour": rate("EC2", "Hrs", "t4g.small"),
        "gp3_gb_month": rate("EBS", "GB-Mo", "gp3"),
        "endpoint_hour": rate("VPC", "Hrs", "VPC Endpoint Hour"),
        "alb_hour": rate("ELB", "Hrs", "Application LoadBalancer"),
        "read_unit": rate("DDB", "ReadRequestUnits", "read request"),
        "write_unit": rate("DDB", "WriteRequestUnits", "write request"),
    }
    hours = Decimal(str(r.c["hours"]))
    estimate = hours * (
        5 * rates["vm_hour"]
        + 100 * rates["gp3_gb_month"] / Decimal(730)
        + 22 * rates["endpoint_hour"]
        + rates["alb_hour"]
    )
    estimate += (
        8_000_000 * rates["read_unit"] + 500_000 * rates["write_unit"] + Decimal("3.30")
    )
    r.write(
        "cost-review.json",
        {
            "at": now().isoformat(),
            "rates": {k: str(v) for k, v in rates.items()},
            "estimate_usd": str(estimate),
            "envelope_usd": r.c["max_usd"],
            "hours": r.c["hours"],
            "hard_billing_cap": False,
            "assumptions": "at most five t4g.small/100GiB roots,22 endpoint-AZ hours/hour,ALB,8M read/0.5M write units,3.30USD reserve; tax and usage beyond assumptions excluded",
        },
    )
    require(
        estimate <= Decimal(str(r.c["max_usd"])),
        "Fresh estimate exceeds configured envelope; revise scope before activation",
    )
    text = f"""# Review lượt {r.id}

Source: `{r.meta["source"]}`. Region: `{r.c["region"]}`. Foundation saved plan: `{r.read("foundation-plan.json")["sha256"]}`.

Dự toán mới: {estimate:.2f} USD trước thuế, envelope {r.c["max_usd"]} USD, {r.c["hours"]} giờ gồm cleanup. Đây không phải hard cap billing. Đọc `cost-review.json` và raw rates để kiểm assumptions.

Hai Cloud + Edge + client trên AWS; tối đa năm VM khi có thay node, root disks disposable. Private endpoints/internal ALB, shared S3/20 bảng, Chunks read512/các bảng khác128/write128. Chỉ test GET và shutdown/native read pressure; không chạy lại toàn bộ GC/recovery/alarm.

IAM quản trị chỉ thao tác IAM/audit, grant tối đa sáu giờ; deploy role chạy workload. Đọc tất cả `iam/*.json`, native validations/simulations và baseline Autopilot của plan trước activation.

Sau evidence: xóa riêng dữ liệu synthetic, runtime resources và root disks không backup. Giữ bootstrap/state và baseline. Key/secret mới chờ xóa30ngày. Giữ ít nhất90phút cuối cho cleanup; root/deploy session phải còn dùng được để hoàn tất. KMS/secret nằm trong pending-deletion manifest, không coi đã biến mất.

Các lệnh mutate yêu cầu `--confirm-run {r.id}`. Chỉ chạy chúng sau khi chủ tài khoản đã chốt gói cụ thể này. IAM login và việc sinh plan chưa tự cho phép chi tiền.
"""
    private(r.file("approval-review.md"), text)
