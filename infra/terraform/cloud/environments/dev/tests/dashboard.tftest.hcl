mock_provider "aws" {}
variables {
  name         = "miles-member-test"
  table_prefix = "miles-member-test-"
  region       = "ap-southeast-2"
  enabled      = false
}
run "disabled_by_default" {
  module { source = "../../modules/observability" }
  command = plan
  assert {
    condition     = output.dashboard == null && length(aws_cloudwatch_dashboard.runtime) == 0
    error_message = "Existing installations must not create a dashboard implicitly."
  }
}
run "cloud_dashboard" {
  module { source = "../../modules/observability" }
  command = plan
  variables {
    dashboard_enabled = true
    profiles          = ["cloud"]
  }
  assert {
    condition     = output.dashboard.name == "miles-member-test-ap-southeast-2-runtime" && strcontains(output.dashboard.url, "region=ap-southeast-2#dashboards/dashboard/miles-member-test-ap-southeast-2-runtime") && length(output.contract) == 0
    error_message = "Dashboard identity follows installation/Region, independently of alarms."
  }
  assert {
    condition     = length(jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets) == 9 && jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).periodOverride == "inherit"
    error_message = "Cloud-only must have one guide and eight runtime panels."
  }
  assert {
    condition = alltrue([
      for widget in slice(jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets, 1, 9) :
      widget.properties.region == "ap-southeast-2" && widget.properties.period == 60 && startswith(widget.properties.title, "cloud") && alltrue([
        for metric in widget.properties.metrics : try(metric[0].expression != "", false) || try(
          metric[0] == "Miles/DSG" && metric[2] == "profile" && metric[3] == "cloud" && metric[4] == "installation" && metric[5] == "miles-member-test-", false
        )
      ])
    ])
    error_message = "Every series must match exact namespace, profile, installation prefix and Region."
  }
  assert {
    condition = (
      jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[1].properties.metrics[0][6] == "mode" &&
      jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[1].properties.metrics[0][7] == "runtime" &&
      jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[1].properties.metrics[0][8].stat == "Minimum" &&
      jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[2].properties.stat == "Sum" &&
      jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[2].properties.metrics[2][0].expression == "IF((hits+misses)>0,100*hits/(hits+misses))" &&
      jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[5].properties.stat == "Maximum"
    )
    error_message = "Ready has the publisher's mode dimension, counters use Sum and gauges use extrema; idle is not a fabricated cache hit rate."
  }
}
run "edge_dashboard" {
  module { source = "../../modules/observability" }
  command = plan
  variables {
    dashboard_enabled = true
    profiles          = ["edge"]
  }
  assert {
    condition     = length(jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets) == 9 && jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[1].properties.metrics[0][3] == "edge"
    error_message = "Edge-only must not display Cloud runtime series."
  }
}
run "both_profiles" {
  module { source = "../../modules/observability" }
  command = plan
  variables { dashboard_enabled = true }
  assert {
    condition     = length(jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets) == 17 && jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[9].properties.metrics[0][3] == "edge" && jsondecode(aws_cloudwatch_dashboard.runtime[0].dashboard_body).widgets[9].y == 28
    error_message = "Both profiles must render separate non-overlapping panels."
  }
}
