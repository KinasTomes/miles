locals {
  alarms = {
    recovery_cut_age = { metric = "miles_recovery_cut_age_seconds", threshold = 3600, statistic = "Maximum", missing = "breaching", description = "Latest sealed cut is older than one hour; export success alone is not RPO." }
    integrity        = { metric = "miles_warm_read_integrity_failures_total", threshold = 0, statistic = "Sum", missing = "notBreaching", description = "A Warm read failed integrity validation. Preserve evidence and stop acceptance." }
    throttling       = { metric = "miles_ddb_throttles_total", threshold = 0, statistic = "Sum", missing = "notBreaching", description = "The runtime observed DynamoDB throttling. Inspect native AWS/DynamoDB table metrics." }
    completing       = { metric = "miles_stuck_completing_uploads", threshold = 0, statistic = "Maximum", missing = "breaching", description = "A Completing upload has an expired completion lease; inspect outcome before recovery." }
    telemetry_age    = { metric = "miles_runtime_observation_age_seconds", threshold = 60, statistic = "Maximum", missing = "breaching", description = "Runtime telemetry is stale or absent. Missing observations are not healthy zeroes." }
  }
  instances = { for pair in setproduct(var.enabled ? var.profiles : [], keys(local.alarms)) : "${pair[0]}-${pair[1]}" => { profile = pair[0], alarm = local.alarms[pair[1]] } }
}
resource "aws_cloudwatch_metric_alarm" "runtime" {
  for_each            = local.instances
  alarm_name          = "${var.name}-${each.key}"
  alarm_description   = each.value.alarm.description
  namespace           = "Miles/DSG"
  metric_name         = each.value.alarm.metric
  dimensions          = { profile = each.value.profile, installation = var.table_prefix }
  statistic           = each.value.alarm.statistic
  period              = 60
  evaluation_periods  = 3
  datapoints_to_alarm = 1
  threshold           = each.value.alarm.threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = each.value.alarm.missing
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
}
output "contract" {
  value = { for k, alarm in aws_cloudwatch_metric_alarm.runtime : k => { metric = alarm.metric_name, dimensions = alarm.dimensions, threshold = alarm.threshold, missing = alarm.treat_missing_data, statistic = alarm.statistic } }
}
