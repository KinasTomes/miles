"""Record the operator's spending envelope; a detailed estimate is not a prerequisite."""

from deploy.acceptance.common import now, require, private


def review(r):
    require(0 < r.c["max_usd"] <= 20, "Keep this acceptance run within the operator's 20 USD envelope")
    r.write("cost-review.json", {
        "at": now().isoformat(), "mode": "operator-envelope", "estimate_usd": None,
        "envelope_usd": r.c["max_usd"], "hours": r.c["hours"], "hard_billing_cap": False,
        "source": r.meta["source"], "detailed_estimate_required": False,
        "note": "Operator chose roughly 10–20 USD. Track actual usage; retain 90 minutes for cleanup."
    })
    private(r.file("approval-review.md"), f'''# Review lượt {r.id}

Source: `{r.meta["source"]}`. Region: `{r.c["region"]}`.
Phạm vi chi tiêu: {r.c["max_usd"]} USD; thời hạn {r.c["hours"]} giờ, gồm 90 phút cleanup.
Không yêu cầu dự toán chi tiết. Đây không phải hard cap của AWS billing.

Đọc IAM và exact saved plan trước từng mutation. Không adopt tài nguyên cũ,
không tự tăng thời hạn hoặc chi tiêu. Cold restore có thể chờ lâu: dùng các lệnh
request/status/verify riêng, giữ checkpoint và theo dõi chi phí trong lúc chờ.
Giữ raw evidence, mọi version Cold còn tính phí và audit cleanup.
''')
