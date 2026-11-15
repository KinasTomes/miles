locals {
  dashboard_name = "${var.name}-${var.region}-runtime"
  dashboard_panels = [
    {
      title = "Runtime health (reported nodes)"
      stat  = "Maximum"
      metrics = [
        { name = "miles_ready", label = "Minimum reported readiness", dimensions = ["mode", "runtime"], stat = "Minimum" },
        { name = "miles_draining", label = "Any reported draining", dimensions = [], stat = "Maximum" },
        { name = "miles_active_requests", label = "Peak active requests on a node", dimensions = [], stat = "Maximum" }
      ]
    },
    {
      title = "Warm cache events and hit ratio"
      stat  = "Sum"
      metrics = [
        { name = "miles_warm_read_cache_hits_total", label = "Cache hits / minute", id = "hits", dimensions = [], stat = "Sum" },
        { name = "miles_warm_read_cache_misses_total", label = "Cache misses / minute", id = "misses", dimensions = [], stat = "Sum" }
      ]
    },
    {
      title = "Body traffic (bytes / minute)"
      stat  = "Sum"
      metrics = [
        { name = "miles_client_gateway_body_bytes_total", label = "Client to gateway", dimensions = [], stat = "Sum" },
        { name = "miles_gateway_client_body_bytes_total", label = "Gateway to client", dimensions = [], stat = "Sum" },
        { name = "miles_gateway_s3_request_body_bytes_total", label = "Gateway to S3", dimensions = [], stat = "Sum" },
        { name = "miles_gateway_s3_response_body_bytes_total", label = "S3 to gateway", dimensions = [], stat = "Sum" }
      ]
    },
    {
      title = "Warm reads (events / minute)"
      stat  = "Sum"
      metrics = [
        { name = "miles_warm_read_requests_total", label = "Requests", dimensions = [], stat = "Sum" },
        { name = "miles_warm_read_aborted_total", label = "Aborted", dimensions = [], stat = "Sum" },
        { name = "miles_warm_read_integrity_failures_total", label = "Integrity failures", dimensions = [], stat = "Sum" }
      ]
    },
    {
      title = "Recovery, R1 and telemetry age (seconds)"
      stat  = "Maximum"
      metrics = [
        { name = "miles_recovery_cut_age_seconds", label = "Oldest reported sealed cut age", dimensions = [], stat = "Maximum" },
        { name = "miles_r1_gate_closed_seconds", label = "Longest reported gate closure", dimensions = [], stat = "Maximum" },
        { name = "miles_runtime_observation_age_seconds", label = "Oldest observation", dimensions = [], stat = "Maximum" }
      ]
    },
    {
      title = "DynamoDB (events / minute)"
      stat  = "Sum"
      metrics = [
        { name = "miles_ddb_calls_total", label = "Calls", dimensions = [], stat = "Sum" },
        { name = "miles_ddb_transactions_total", label = "Transactions", dimensions = [], stat = "Sum" },
        { name = "miles_ddb_throttles_total", label = "Throttles", dimensions = [], stat = "Sum" },
        { name = "miles_ddb_retries_total", label = "Retries", dimensions = [], stat = "Sum" }
      ]
    },
    {
      title = "Write revival and orphans (stored bytes / minute)"
      stat  = "Sum"
      metrics = [
        { name = "miles_revived_stored_bytes_total", label = "Revived", dimensions = [], stat = "Sum" },
        { name = "miles_orphaned_stored_bytes_total", label = "Orphaned", dimensions = [], stat = "Sum" }
      ]
    },
    {
      title = "Operational failures (events / minute)"
      stat  = "Sum"
      metrics = [
        { name = "miles_job_dispatch_failures_total", label = "Job dispatch failures", dimensions = [], stat = "Sum" },
        { name = "miles_dependency_probe_failures_total", label = "Dependency probe failures", dimensions = [], stat = "Sum" },
        { name = "miles_runtime_observation_failures_total", label = "Runtime observation failures", dimensions = [], stat = "Sum" }
      ]
    }
  ]
  dashboard_widgets = flatten([
    for profile_index, profile in sort(tolist(var.profiles)) : [
      for panel_index, panel in local.dashboard_panels : {
        type   = "metric"
        x      = (panel_index % 2) * 12
        y      = 4 + profile_index * 24 + floor(panel_index / 2) * 6
        width  = 12
        height = 6
        properties = {
          region  = var.region
          title   = "${profile} · ${panel.title}"
          view    = "timeSeries"
          stacked = false
          period  = 60
          stat    = panel.stat
          metrics = concat([
            for metric in panel.metrics : concat(
              ["Miles/DSG", metric.name, "profile", profile, "installation", var.table_prefix],
              metric.dimensions,
              [merge({ label = metric.label, stat = metric.stat }, try({ id = metric.id }, {}))]
            )
            ], panel_index == 1 ? [[{
              expression = "IF((hits+misses)>0,100*hits/(hits+misses))"
              label      = "Observed hit ratio (%)"
              id         = "ratio"
              yAxis      = "right"
          }]] : [])
        }
      }
    ]
  ])
}

resource "aws_cloudwatch_dashboard" "runtime" {
  count          = var.dashboard_enabled ? 1 : 0
  dashboard_name = local.dashboard_name
  dashboard_body = jsonencode({
    start          = "-PT1H"
    periodOverride = "inherit"
    widgets = concat([{
      type   = "text"
      x      = 0
      y      = 0
      width  = 24
      height = 4
      properties = {
        markdown = "# Miles · ${var.name} · ${var.region}\nManaged by Terraform; edit the source, not this dashboard. Profiles: ${join(", ", sort(tolist(var.profiles)))}. Runtime exports once per minute.\n\nSeries aggregate reports by profile, not by node. Gauges show extrema of reports, not a cluster sum or proof all nodes reported. Missing data is not a healthy zero. Counter deltas are best-effort; empty intervals may be idle or missing exports. Cache ratio requires observed traffic. Use dsgctl stats/cache stats/node ls for storage accounting and node coverage. This dashboard is not a billing ledger."
      }
    }], local.dashboard_widgets)
  })
}

output "dashboard" {
  value = var.dashboard_enabled ? {
    name = aws_cloudwatch_dashboard.runtime[0].dashboard_name
    url  = "https://${var.region}.console.aws.amazon.com/cloudwatch/home?region=${var.region}#dashboards/dashboard/${local.dashboard_name}"
  } : null
}
